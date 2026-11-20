package agentadapter

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/window"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
)

// contextWindowLLM rejects requests over its window, as a provider rejects a
// prompt that does not fit the model's context, and answers otherwise.
type contextWindowLLM struct {
	window int
	sizes  []int
}

func (m *contextWindowLLM) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	size := requestChars(req)
	m.sizes = append(m.sizes, size)
	if size > m.window {
		return nil, fmt.Errorf("provider: %w", ports.ErrContextTooLong)
	}
	return &ports.ChatResponse{Content: "ok"}, nil
}

type sessionMemory struct {
	messages map[string][]ports.MemoryMessage
}

func (m *sessionMemory) Load(_ context.Context, session string) ([]ports.MemoryMessage, error) {
	return m.messages[session], nil
}

func (m *sessionMemory) Append(_ context.Context, session string, messages []ports.MemoryMessage) error {
	m.messages[session] = append(m.messages[session], messages...)
	return nil
}

func sessionScope(metadata map[string]any) string {
	session, _ := metadata["session"].(string)
	return session
}

// TestOverflowShrinksAndIsRemembered runs a session whose compressor budget
// is larger than the model's real window. The first overflow shrinks the
// projection and retries; later calls in the session keep under the learned
// limit instead of overflowing again.
func TestOverflowShrinksAndIsRemembered(t *testing.T) {
	llm := &contextWindowLLM{window: 1500}
	projector, err := NewProjector(Config{
		Compressor: window.New(window.Config{Budget: contextkit.Budget{MaxChars: 100000}}),
		Scope:      sessionScope,
	})
	if err != nil {
		t.Fatal(err)
	}
	memory := &sessionMemory{messages: map[string][]ports.MemoryMessage{}}
	agent, err := agentcore.NewAgent(agentcore.WithLLM(llm), agentcore.WithContextProjector(projector), agentcore.WithMemoryProvider(memory))
	if err != nil {
		t.Fatal(err)
	}
	for turn := 0; turn < 12; turn++ {
		_, err := agent.Run(context.Background(), agentcore.RunRequest{
			SessionID: "a", Input: fmt.Sprintf("turn %d %s", turn, strings.Repeat("x", 200)),
			Metadata: map[string]any{"session": "a"},
		})
		if err != nil {
			t.Fatalf("turn %d: %v (sizes %v)", turn, err, llm.sizes)
		}
	}
	overflows := 0
	for _, size := range llm.sizes {
		if size > llm.window {
			overflows++
		}
	}
	if overflows == 0 || overflows > 2 {
		t.Fatalf("overflows = %d, sizes %v", overflows, llm.sizes)
	}
	if limit := projector.limit("a"); limit <= 0 || limit > llm.window {
		t.Fatalf("learned limit = %d", limit)
	}
	if projector.limit("b") != 0 {
		t.Fatal("a limit leaked to another scope")
	}

	// A projection for the session reports the limit it used.
	result, err := projector.Project(context.Background(), agentcore.ContextProjectionRequest{
		Messages: []agentcore.Message{{Role: "user", Content: "hi"}}, Metadata: map[string]any{"session": "a"},
	})
	if err != nil || result.Metadata["contextkit.overflow_limit"] != projector.limit("a") {
		t.Fatalf("metadata = %+v, %v", result.Metadata, err)
	}
}

func TestLearnedLimitsOnlyGoDownAndAreBounded(t *testing.T) {
	projector, _ := NewProjector(Config{Compressor: window.New(window.Config{})})
	projector.learnLimit("s", 1000)
	projector.learnLimit("s", 2000)
	if projector.limit("s") != 1000 {
		t.Fatalf("limit = %d", projector.limit("s"))
	}
	for i := 0; i < maxOverflowLimits+5; i++ {
		projector.learnLimit(fmt.Sprint("scope-", i), 500)
	}
	if len(projector.limits) != maxOverflowLimits || projector.limit("s") != 0 {
		t.Fatalf("limits = %d, s = %d", len(projector.limits), projector.limit("s"))
	}
}
