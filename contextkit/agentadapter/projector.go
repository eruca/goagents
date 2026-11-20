// Package agentadapter connects a contextkit Compressor to goagent as an
// agentcore.ContextProjector, so a run's model requests use a compressed view
// of its messages.
//
// goagent projects the whole run before every model call. The projector
// remembers recent compressions by a hash of the messages they covered; when
// a run's messages extend a remembered prefix, it compresses that earlier
// projection plus the new messages, so a Summarizer folds in only the newly
// dropped messages instead of summarizing the run again on every call.
package agentadapter

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
)

const (
	defaultCacheSize = 32
	// systemPromptKey marks the compiled system prompt the projector adds
	// for compression only: it counts against the budget and is removed
	// again, because goagent adds the prompt to the model call itself.
	systemPromptKey = "contextkit/agentadapter.system_prompt"
)

// Config configures a Projector.
type Config struct {
	// Compressor builds the model-facing view. Required.
	Compressor contextkit.Compressor
	// Next, when set, projects the compressed messages further, for example
	// memorykit's projector inserting recalled memory. Compressing first
	// keeps recalled memory out of the summaries.
	Next agentcore.ContextProjector
	// UseAgentBudget compresses to the run's agentcore.Budget.MaxInputTokens
	// when it is set, instead of the compressor's own budget. The compressor
	// then needs a token counter.
	//
	// Either budget covers the compiled system prompt (the request's
	// SystemPrompt), which is counted and then left out of the projection
	// because goagent adds it to the model call. Tool schemas and anything
	// Next adds, such as recalled memory, are not counted.
	UseAgentBudget bool
	// OnCompress, when set, receives every compression result, for example
	// to keep deep-mode Collapses. It must not modify the result.
	OnCompress func(context.Context, *contextkit.Result)
	// CacheSize bounds the remembered compressions; zero means 32.
	CacheSize int
}

// Projector is an agentcore.ContextProjector backed by a contextkit
// Compressor. It is safe for concurrent use.
type Projector struct {
	cfg Config

	mu      sync.Mutex
	order   *list.List
	entries map[[sha256.Size]byte]*list.Element
}

type cacheEntry struct {
	key       [sha256.Size]byte
	projected []contextkit.Message
}

// NewProjector validates cfg and returns a Projector.
func NewProjector(cfg Config) (*Projector, error) {
	if cfg.Compressor == nil {
		return nil, errors.New("contextkit/agentadapter: Compressor is required")
	}
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = defaultCacheSize
	}
	return &Projector{cfg: cfg, order: list.New(), entries: map[[sha256.Size]byte]*list.Element{}}, nil
}

// Project compresses the run's messages and hands the result to Next.
func (p *Projector) Project(ctx context.Context, req agentcore.ContextProjectionRequest) (*agentcore.ContextProjectionResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	budget := contextkit.Budget{}
	if p.cfg.UseAgentBudget && req.Budget.MaxInputTokens > 0 {
		budget.MaxTokens = req.Budget.MaxInputTokens
	}
	messages := ToContextKit(req.Messages)
	prefixes := prefixHashes(budget, req.SystemPrompt, messages)

	input, reused := messages, 0
	if cached, length := p.lookup(prefixes); length > 0 {
		input = append(cached, cloneMessages(messages[length:])...)
		reused = length
	}
	if req.SystemPrompt != "" {
		prompt := contextkit.Message{
			Role: contextkit.RoleSystem, Content: req.SystemPrompt,
			Metadata: map[string]any{systemPromptKey: true},
		}
		input = append([]contextkit.Message{prompt}, input...)
	}
	result, err := p.cfg.Compressor.Compress(ctx, contextkit.Request{
		Messages: input,
		Budget:   budget,
		Metadata: cloneMetadata(req.Metadata),
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("contextkit/agentadapter: compressor returned no result")
	}
	result.Messages = withoutSystemPrompt(result.Messages)
	p.store(prefixes[len(messages)], result.Messages)
	if p.cfg.OnCompress != nil {
		p.cfg.OnCompress(ctx, result)
	}

	metadata := cloneMetadata(req.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["contextkit.dropped"] = result.Dropped
	metadata["contextkit.over_budget"] = result.OverBudget
	metadata["contextkit.reused_messages"] = reused
	projected := FromContextKit(result.Messages)
	if p.cfg.Next == nil {
		return &agentcore.ContextProjectionResult{Messages: projected, Metadata: metadata}, nil
	}
	next, err := p.cfg.Next.Project(ctx, agentcore.ContextProjectionRequest{
		Messages: projected, Budget: req.Budget, Metadata: metadata, SystemPrompt: req.SystemPrompt,
	})
	if err != nil || next == nil {
		return next, err
	}
	merged := cloneMetadata(next.Metadata)
	if merged == nil {
		merged = map[string]any{}
	}
	for key, value := range metadata {
		if _, exists := merged[key]; !exists {
			merged[key] = value
		}
	}
	return &agentcore.ContextProjectionResult{Messages: next.Messages, Metadata: merged}, nil
}

// lookup returns a copy of the projection remembered for the longest prefix
// of the messages, and that prefix's length.
func (p *Projector) lookup(prefixes [][sha256.Size]byte) ([]contextkit.Message, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for length := len(prefixes) - 1; length > 0; length-- {
		if element, ok := p.entries[prefixes[length]]; ok {
			p.order.MoveToFront(element)
			return cloneMessages(element.Value.(*cacheEntry).projected), length
		}
	}
	return nil, 0
}

func (p *Projector) store(key [sha256.Size]byte, projected []contextkit.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if element, ok := p.entries[key]; ok {
		element.Value.(*cacheEntry).projected = cloneMessages(projected)
		p.order.MoveToFront(element)
		return
	}
	p.entries[key] = p.order.PushFront(&cacheEntry{key: key, projected: cloneMessages(projected)})
	for p.order.Len() > p.cfg.CacheSize {
		oldest := p.order.Back()
		p.order.Remove(oldest)
		delete(p.entries, oldest.Value.(*cacheEntry).key)
	}
}

func withoutSystemPrompt(messages []contextkit.Message) []contextkit.Message {
	out := messages[:0:0]
	for _, msg := range messages {
		if marked, _ := msg.Metadata[systemPromptKey].(bool); !marked {
			out = append(out, msg)
		}
	}
	return out
}

// prefixHashes returns, for each n from 0 to len(messages), a hash of the
// budget, the system prompt, and the first n messages.
func prefixHashes(budget contextkit.Budget, systemPrompt string, messages []contextkit.Message) [][sha256.Size]byte {
	hashes := make([][sha256.Size]byte, len(messages)+1)
	seed := sha256.New()
	writeInt(seed, budget.MaxChars)
	writeInt(seed, budget.MaxTokens)
	writeString(seed, systemPrompt)
	copy(hashes[0][:], seed.Sum(nil))
	for index, msg := range messages {
		h := sha256.New()
		h.Write(hashes[index][:])
		writeString(h, string(msg.Role))
		writeString(h, msg.Content)
		writeString(h, msg.Reasoning)
		writeString(h, msg.ToolCallID)
		writeInt(h, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			writeString(h, call.ID)
			writeString(h, call.Name)
			writeString(h, call.Arguments)
		}
		copy(hashes[index+1][:], h.Sum(nil))
	}
	return hashes
}

type hashWriter interface{ Write([]byte) (int, error) }

func writeInt(h hashWriter, value int) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(value))
	_, _ = h.Write(buf[:])
}

func writeString(h hashWriter, value string) {
	writeInt(h, len(value))
	_, _ = h.Write([]byte(value))
}

// ToContextKit maps goagent messages to contextkit messages, keeping tool
// calls and naming each tool reply after the call it answers.
func ToContextKit(messages []agentcore.Message) []contextkit.Message {
	toolNames := map[string]string{}
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			if call.ID != "" {
				toolNames[call.ID] = call.Name
			}
		}
	}
	out := make([]contextkit.Message, 0, len(messages))
	for _, msg := range messages {
		converted := contextkit.Message{
			Role:       contextkit.Role(msg.Role),
			Content:    msg.Content,
			ToolCallID: msg.ToolCallID,
			Reasoning:  msg.Reasoning,
		}
		if converted.Role == contextkit.RoleTool {
			converted.ToolName = toolNames[msg.ToolCallID]
		}
		for _, call := range msg.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, contextkit.ToolCall{
				ID: call.ID, Name: call.Name, Arguments: string(call.Input),
			})
		}
		out = append(out, converted)
	}
	return out
}

// FromContextKit maps contextkit messages back to goagent messages.
func FromContextKit(messages []contextkit.Message) []agentcore.Message {
	out := make([]agentcore.Message, 0, len(messages))
	for _, msg := range messages {
		converted := agentcore.Message{
			Role:       string(msg.Role),
			Content:    msg.Content,
			ToolCallID: msg.ToolCallID,
			Reasoning:  msg.Reasoning,
		}
		for _, call := range msg.ToolCalls {
			var input json.RawMessage
			if call.Arguments != "" {
				input = json.RawMessage(call.Arguments)
			}
			converted.ToolCalls = append(converted.ToolCalls, ports.ToolCall{ID: call.ID, Name: call.Name, Input: input})
		}
		out = append(out, converted)
	}
	return out
}

func cloneMessages(messages []contextkit.Message) []contextkit.Message {
	out := make([]contextkit.Message, len(messages))
	for index, msg := range messages {
		msg.ToolCalls = append([]contextkit.ToolCall(nil), msg.ToolCalls...)
		msg.Images = append([]contextkit.Image(nil), msg.Images...)
		msg.Metadata = cloneMetadata(msg.Metadata)
		out[index] = msg
	}
	return out
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	copied := make(map[string]any, len(metadata))
	for key, value := range metadata {
		copied[key] = value
	}
	return copied
}
