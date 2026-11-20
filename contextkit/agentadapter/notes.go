package agentadapter

import (
	"container/list"
	"strings"
	"sync"
)

// NotesConfig bounds a Notes store.
type NotesConfig struct {
	// MaxChars bounds each scope's notes; the oldest are dropped beyond it.
	// Zero means 2000.
	MaxChars int
	// MaxScopes evicts the least recently used scope beyond it; zero means
	// 1000.
	MaxScopes int
}

// Notes is the model's working memory: notes it writes with context_note are
// pinned to the top of every projection of its conversation, counted against
// the budget, and never compressed away. It is process-local and safe for
// concurrent use.
type Notes struct {
	cfg    NotesConfig
	mu     sync.Mutex
	scopes map[string]*list.Element
	order  *list.List
}

type noteScope struct {
	name  string
	notes []string
}

// NewNotes returns an empty store.
func NewNotes(cfg NotesConfig) *Notes {
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = 2000
	}
	if cfg.MaxScopes <= 0 {
		cfg.MaxScopes = 1000
	}
	return &Notes{cfg: cfg, scopes: map[string]*list.Element{}, order: list.New()}
}

// Add appends a note to scope, dropping the oldest notes beyond MaxChars.
func (n *Notes) Add(scope, note string) {
	note = strings.Join(strings.Fields(note), " ")
	if scope == "" || note == "" {
		return
	}
	if runes := []rune(note); len(runes) > n.cfg.MaxChars {
		note = string(runes[:n.cfg.MaxChars])
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	element, ok := n.scopes[scope]
	if !ok {
		element = n.order.PushFront(&noteScope{name: scope})
		n.scopes[scope] = element
		for n.order.Len() > n.cfg.MaxScopes {
			oldest := n.order.Back()
			n.order.Remove(oldest)
			delete(n.scopes, oldest.Value.(*noteScope).name)
		}
	}
	n.order.MoveToFront(element)
	s := element.Value.(*noteScope)
	s.notes = append(s.notes, note)
	for total(s.notes) > n.cfg.MaxChars && len(s.notes) > 1 {
		s.notes = s.notes[1:]
	}
}

// List returns scope's notes, oldest first.
func (n *Notes) List(scope string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	element, ok := n.scopes[scope]
	if !ok {
		return nil
	}
	return append([]string(nil), element.Value.(*noteScope).notes...)
}

// Delete drops scope's notes, for example when its conversation ends.
func (n *Notes) Delete(scope string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if element, ok := n.scopes[scope]; ok {
		n.order.Remove(element)
		delete(n.scopes, scope)
	}
}

func total(notes []string) int {
	chars := 0
	for _, note := range notes {
		chars += len([]rune(note))
	}
	return chars
}

func renderNotes(notes []string) string {
	return "Your notes (kept in your context through compression; add more with " + ContextNoteTool + "):\n- " + strings.Join(notes, "\n- ")
}
