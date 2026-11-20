// Package archive provides an in-memory contextkit.Archive and the
// references and search ranking every archive in this repository shares.
package archive

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/eruca/goagents/contextkit"
)

// Config bounds a Memory archive.
type Config struct {
	// MaxMessagesPerScope evicts a scope's oldest messages beyond it; zero
	// means 500.
	MaxMessagesPerScope int
	// MaxScopes evicts the least recently used scope beyond it; zero means
	// 1000.
	MaxScopes int
}

// Memory is a process-local contextkit.Archive. It loses its content on
// restart and is not shared between processes; a reference it cannot find
// reads as contextkit.ErrArchiveNotFound.
type Memory struct {
	cfg    Config
	mu     sync.Mutex
	scopes map[string]*list.Element
	order  *list.List
}

type scope struct {
	name     string
	messages map[string]*entry
	order    []string
}

type entry struct {
	message contextkit.Message
	terms   map[string]bool
}

// New returns an empty Memory archive.
func New(cfg Config) *Memory {
	if cfg.MaxMessagesPerScope <= 0 {
		cfg.MaxMessagesPerScope = 500
	}
	if cfg.MaxScopes <= 0 {
		cfg.MaxScopes = 1000
	}
	return &Memory{cfg: cfg, scopes: map[string]*list.Element{}, order: list.New()}
}

var errNoScope = errors.New("contextkit/archive: the context has no archive scope")

// Offload stores msg in the context's scope and returns its reference,
// which is derived from the scope and the message, so storing the same
// message again returns the same reference.
func (m *Memory) Offload(ctx context.Context, msg contextkit.Message) (string, error) {
	name := contextkit.ArchiveScope(ctx)
	if name == "" {
		return "", errNoScope
	}
	ref := Ref(name, msg)
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.scope(name, true)
	if _, exists := s.messages[ref]; !exists {
		s.messages[ref] = &entry{message: clone(msg), terms: termSet(SearchText(msg))}
		s.order = append(s.order, ref)
		for len(s.order) > m.cfg.MaxMessagesPerScope {
			delete(s.messages, s.order[0])
			s.order = s.order[1:]
		}
	}
	return ref, nil
}

// Read returns the message stored under ref in the context's scope.
func (m *Memory) Read(ctx context.Context, ref string) (contextkit.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.scope(contextkit.ArchiveScope(ctx), false)
	if s == nil {
		return contextkit.Message{}, contextkit.ErrArchiveNotFound
	}
	stored, ok := s.messages[ref]
	if !ok {
		return contextkit.Message{}, contextkit.ErrArchiveNotFound
	}
	return clone(stored.message), nil
}

// Search ranks the scope's messages by the query terms they share, each
// weighted by how rare it is in the scope, and returns at most limit.
func (m *Memory) Search(ctx context.Context, query string, limit int) ([]contextkit.ArchiveHit, error) {
	if limit <= 0 {
		return nil, nil
	}
	terms := termSet(query)
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.scope(contextkit.ArchiveScope(ctx), false)
	if s == nil || len(terms) == 0 {
		return nil, nil
	}
	documents := map[string]int{}
	for _, stored := range s.messages {
		for term := range terms {
			if stored.terms[term] {
				documents[term]++
			}
		}
	}
	n := len(s.messages)
	var hits []contextkit.ArchiveHit
	for position, ref := range s.order {
		stored := s.messages[ref]
		score := 0.0
		for term := range terms {
			if stored.terms[term] {
				score += TermWeight(n, documents[term])
			}
		}
		if score > 0 {
			// Newer messages win ties.
			hits = append(hits, contextkit.ArchiveHit{Ref: ref, Message: clone(stored.message), Score: TieBreak(score, position)})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// DeleteScope drops a scope's messages, for example when its conversation
// ends.
func (m *Memory) DeleteScope(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if element, ok := m.scopes[name]; ok {
		m.order.Remove(element)
		delete(m.scopes, name)
	}
}

func (m *Memory) scope(name string, create bool) *scope {
	if name == "" {
		return nil
	}
	if element, ok := m.scopes[name]; ok {
		m.order.MoveToFront(element)
		return element.Value.(*scope)
	}
	if !create {
		return nil
	}
	s := &scope{name: name, messages: map[string]*entry{}}
	m.scopes[name] = m.order.PushFront(s)
	for m.order.Len() > m.cfg.MaxScopes {
		oldest := m.order.Back()
		m.order.Remove(oldest)
		delete(m.scopes, oldest.Value.(*scope).name)
	}
	return s
}

// Ref is the reference an archive stores msg under in scope. It depends only
// on the scope and the message, so storing the same message again returns the
// same reference, in any archive implementation.
func Ref(scope string, msg contextkit.Message) string {
	h := sha256.New()
	for _, part := range []string{scope, string(msg.Role), msg.ToolCallID, msg.Content, msg.Reasoning} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	for _, call := range msg.ToolCalls {
		h.Write([]byte(call.ID + "\x00" + call.Name + "\x00" + call.Arguments + "\x00"))
	}
	return "ctx:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// SearchText is the text of msg that search matches: its tool name, content
// and reasoning.
func SearchText(msg contextkit.Message) string {
	return msg.ToolName + " " + msg.Content + " " + msg.Reasoning
}

// Terms returns the sorted search terms of text: lowercase words and, for
// Chinese and Japanese, two-character grams.
func Terms(text string) []string {
	set := termSet(text)
	terms := make([]string, 0, len(set))
	for term := range set {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	return terms
}

// TermWeight is the weight of a query term found in documents of the n
// messages in a scope: rarer terms weigh more. A message scores the sum of
// the weights of the query terms it contains.
func TermWeight(n, documents int) float64 {
	df := float64(documents)
	return math.Log(1 + (float64(n)-df+0.5)/(df+0.5))
}

// TieBreak adds a tiny amount for a message's position in insertion order, so
// newer messages win ties.
func TieBreak(score float64, position int) float64 {
	return score + float64(position)*1e-9
}

// termSet splits text into lowercase words and, for Chinese and Japanese,
// two-character grams.
func termSet(text string) map[string]bool {
	terms := map[string]bool{}
	var word, cjk []rune
	flush := func() {
		if len(word) > 1 {
			terms[strings.ToLower(string(word))] = true
		}
		word = word[:0]
		if len(cjk) == 1 {
			terms[string(cjk)] = true
		}
		for i := 0; i+1 < len(cjk); i++ {
			terms[string(cjk[i:i+2])] = true
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana):
			if len(word) > 0 {
				flush()
			}
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if len(cjk) > 0 {
				flush()
			}
			word = append(word, r)
		default:
			flush()
		}
	}
	flush()
	return terms
}

func clone(msg contextkit.Message) contextkit.Message {
	msg.ToolCalls = append([]contextkit.ToolCall(nil), msg.ToolCalls...)
	msg.Images = append([]contextkit.Image(nil), msg.Images...)
	if len(msg.Metadata) > 0 {
		copied := make(map[string]any, len(msg.Metadata))
		for key, value := range msg.Metadata {
			copied[key] = value
		}
		msg.Metadata = copied
	}
	return msg
}
