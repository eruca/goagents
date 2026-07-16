package agentcore

import (
	"context"
	"testing"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
	"github.com/eruca/goagents/goagent/tools"
)

type testProjector struct{}

func (p testProjector) Project(ctx context.Context, req ContextProjectionRequest) (*ContextProjectionResult, error) {
	projected := []Message{
		{Role: "user", Content: "projected current question"},
		{Role: "tool", Content: "tool=lookup\nstatus=success\nresult=projected"},
	}
	return &ContextProjectionResult{
		Messages: projected,
		Metadata: map[string]any{"projected": true},
	}, nil
}

func TestContextProjectionStageFeedsProjectedMessagesToThink(t *testing.T) {
	t.Parallel()

	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "done"}}}
	state := NewRunState(NewRunID(), RunRequest{Input: "current question"})
	runner := NewReActRunner(ReActConfig{
		LLM:              llm,
		PromptCompiler:   prompt.NewCompiler(),
		ToolRegistry:     tools.NewRegistry(),
		PolicyEngine:     policy.NewEngine(),
		ContextProjector: testProjector{},
		MaxIterations:    1,
	})

	_, err := runner.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("unexpected llm requests: %d", len(llm.requests))
	}
	got := llm.requests[0].Messages
	if len(got) != 2 {
		t.Fatalf("unexpected projected message count: %#v", got)
	}
	if got[0].Content != "projected current question" || got[1].Content != "tool=lookup\nstatus=success\nresult=projected" {
		t.Fatalf("LLM received unprojected messages: %#v", got)
	}
	if state.Messages[len(state.Messages)-1].Content != "done" {
		t.Fatalf("original run state should still receive final answer: %#v", state.Messages)
	}
	if state.ContextProjection == nil || state.ContextProjection.Metadata["projected"] != true {
		t.Fatalf("projection metadata not recorded: %#v", state.ContextProjection)
	}
}
