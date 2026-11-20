package agentcore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
)

// windowLLM rejects requests larger than its window, as a provider rejects a
// prompt that does not fit the model's context.
type windowLLM struct {
	window int
	sizes  []int
}

func (m *windowLLM) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	size := requestChars(req.Messages)
	m.sizes = append(m.sizes, size)
	if size > m.window {
		return nil, fmt.Errorf("provider: %w", ports.ErrContextTooLong)
	}
	return &ports.ChatResponse{Content: "fits"}, nil
}

// shrinkingProjector keeps the newest messages that fit the overflow's
// target, or passes everything through when there is no overflow.
type shrinkingProjector struct {
	overflows []ContextOverflow
	ignore    bool
}

func (p *shrinkingProjector) Project(_ context.Context, req ContextProjectionRequest) (*ContextProjectionResult, error) {
	if req.Overflow == nil || p.ignore {
		if req.Overflow != nil {
			p.overflows = append(p.overflows, *req.Overflow)
		}
		return &ContextProjectionResult{Messages: req.Messages}, nil
	}
	p.overflows = append(p.overflows, *req.Overflow)
	target := req.Overflow.RejectedChars / 2
	kept, size := []Message{}, len(req.SystemPrompt)
	for i := len(req.Messages) - 1; i >= 0; i-- {
		size += len(req.Messages[i].Content)
		if size > target && len(kept) > 0 {
			break
		}
		kept = append([]Message{req.Messages[i]}, kept...)
	}
	return &ContextProjectionResult{Messages: kept}, nil
}

func overflowHistory() []ports.MemoryMessage {
	var history []ports.MemoryMessage
	for i := 0; i < 8; i++ {
		history = append(history, ports.MemoryMessage{Role: "user", Content: strings.Repeat("u", 100)}, ports.MemoryMessage{Role: "assistant", Content: strings.Repeat("a", 100)})
	}
	return history
}

func TestContextOverflowProjectsAgainAndRetries(t *testing.T) {
	llm := &windowLLM{window: 800}
	projector := &shrinkingProjector{}
	sink := &recordingEventSink{}
	agent, err := NewAgent(WithLLM(llm), WithContextProjector(projector), WithEventSink(sink),
		WithMemoryProvider(&mockMemoryProvider{loaded: overflowHistory()}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), RunRequest{SessionID: "s", Input: "question"})
	if err != nil {
		t.Fatalf("run failed: %v (sizes %v)", err, llm.sizes)
	}
	if result.Content != "fits" {
		t.Fatalf("content = %q", result.Content)
	}
	if len(llm.sizes) != 2 || llm.sizes[0] <= 800 || llm.sizes[1] > 800 {
		t.Fatalf("request sizes = %v", llm.sizes)
	}
	if len(projector.overflows) != 1 || projector.overflows[0] != (ContextOverflow{Attempt: 1, RejectedChars: llm.sizes[0]}) {
		t.Fatalf("overflows = %+v", projector.overflows)
	}
	event, ok := sink.findEvent(EventContextOverflow, result.RunID)
	if !ok || event.Metadata["attempt"] != 1 || event.Metadata["rejected_chars"] != llm.sizes[0] {
		t.Fatalf("overflow event = %+v, %v", event, ok)
	}
}

func TestContextOverflowStopsWhenTheProjectionDoesNotShrink(t *testing.T) {
	llm := &windowLLM{window: 600}
	projector := &shrinkingProjector{ignore: true}
	agent, err := NewAgent(WithLLM(llm), WithContextProjector(projector),
		WithMemoryProvider(&mockMemoryProvider{loaded: overflowHistory()}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Run(context.Background(), RunRequest{SessionID: "s", Input: "question"})
	if !errors.Is(err, ports.ErrContextTooLong) {
		t.Fatalf("err = %v", err)
	}
	if len(llm.sizes) != 1 || len(projector.overflows) != 1 {
		t.Fatalf("calls = %v, overflows = %+v", llm.sizes, projector.overflows)
	}
}

func TestContextOverflowRetriesAreBounded(t *testing.T) {
	llm := &windowLLM{window: 10}
	projector := &shrinkingProjector{}
	agent, err := NewAgent(WithLLM(llm), WithContextProjector(projector),
		WithMemoryProvider(&mockMemoryProvider{loaded: overflowHistory()}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = agent.Run(context.Background(), RunRequest{SessionID: "s", Input: "question"})
	if !errors.Is(err, ports.ErrContextTooLong) {
		t.Fatalf("err = %v", err)
	}
	if len(llm.sizes) != 1+MaxContextOverflowRetries {
		t.Fatalf("calls = %v", llm.sizes)
	}
	if projector.overflows[1].Attempt != 2 {
		t.Fatalf("overflows = %+v", projector.overflows)
	}
}

func TestContextOverflowWithoutProjectorEndsTheRun(t *testing.T) {
	llm := &windowLLM{window: 10}
	agent, err := NewAgent(WithLLM(llm))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), RunRequest{Input: "a question longer than ten"}); !errors.Is(err, ports.ErrContextTooLong) {
		t.Fatalf("err = %v", err)
	}
	if len(llm.sizes) != 1 {
		t.Fatalf("calls = %v", llm.sizes)
	}
}
