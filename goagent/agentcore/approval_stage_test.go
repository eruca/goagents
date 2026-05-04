package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/tools"
)

type staticToolApprover struct {
	decision ToolApprovalDecision
	requests []ToolApprovalRequest
}

func (a *staticToolApprover) ApproveTool(ctx context.Context, req ToolApprovalRequest) ToolApprovalDecision {
	a.requests = append(a.requests, req)
	return a.decision
}

func TestApprovalStageContinuesWithoutApprover(t *testing.T) {
	state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
	state.PendingCalls = []tools.Call{{Name: "lookup", Input: json.RawMessage(`{}`)}}

	result, err := ApprovalStage{}.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result != StageContinue {
		t.Fatalf("result = %v", result)
	}
}

func TestAgentRunsToolWhenApproverAllows(t *testing.T) {
	toolRan := false
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			toolRan = true
			return &tools.Result{ForLLM: "lookup result"}, nil
		},
	})
	approver := &staticToolApprover{decision: ToolApprovalDecision{Allowed: true, Reason: "approved"}}
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{"q":"go"}`)}}},
		{Content: "done"},
	}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
		WithToolApprover(approver),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{UserID: "u1", SessionID: "s1", Input: "lookup"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Content != "done" {
		t.Fatalf("content = %q", result.Content)
	}
	if !toolRan {
		t.Fatal("tool did not run")
	}
	if len(approver.requests) != 1 {
		t.Fatalf("approval requests = %d", len(approver.requests))
	}
	req := approver.requests[0]
	if req.Tool != "lookup" || req.UserID != "u1" || req.SessionID != "s1" || string(req.Input) != `{"q":"go"}` {
		t.Fatalf("approval request = %#v", req)
	}
}

func TestAgentDoesNotRunToolWhenApproverDenies(t *testing.T) {
	toolRan := false
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			toolRan = true
			return &tools.Result{ForLLM: "lookup result"}, nil
		},
	})
	approver := &staticToolApprover{decision: ToolApprovalDecision{Allowed: false, Reason: "operator rejected"}}
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{"q":"go"}`)}}},
	}}
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry), WithToolApprover(approver))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "lookup"})
	if !errors.Is(err, ErrApprovalDenied) {
		t.Fatalf("err = %v, want ErrApprovalDenied", err)
	}
	if toolRan {
		t.Fatal("tool ran after approval denial")
	}
}

func TestAgentRunDetailedReturnsPartialResultOnApprovalDeny(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "lookup result"}, nil
		},
	})
	approver := &staticToolApprover{decision: ToolApprovalDecision{Allowed: false, Reason: "operator rejected"}}
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{"q":"go"}`)}},
		Usage:     ports.Usage{InputTokens: 5, OutputTokens: 2},
	}}}
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry), WithToolApprover(approver))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.RunDetailed(context.Background(), RunRequest{Input: "lookup"})
	if !errors.Is(err, ErrApprovalDenied) {
		t.Fatalf("err = %v, want ErrApprovalDenied", err)
	}
	if result == nil {
		t.Fatal("RunDetailed returned nil result")
	}
	if result.ExecutionSummary.LLMCalls != 1 || result.ExecutionSummary.ToolCalls != 0 {
		t.Fatalf("summary = %#v", result.ExecutionSummary)
	}
	if result.ExecutionSummary.AbortReason == "" {
		t.Fatal("AbortReason is empty")
	}
}

func TestApprovalStageEmitsApprovalEvents(t *testing.T) {
	sink := &recordingEventSink{}
	state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
	state.EventSink = sink
	state.PendingCalls = []tools.Call{{Name: "lookup", Input: json.RawMessage(`{}`)}}
	stage := ApprovalStage{Approver: &staticToolApprover{decision: ToolApprovalDecision{Allowed: true, Reason: "ok"}}}

	result, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result != StageContinue {
		t.Fatalf("result = %v", result)
	}
	if !sink.hasEvent(EventApprovalRequested, state.RunID) {
		t.Fatal("missing approval requested event")
	}
	if !sink.hasEvent(EventApprovalCompleted, state.RunID) {
		t.Fatal("missing approval completed event")
	}
}

func TestAgentStreamIncludesApprovalDeniedEvent(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "lookup result"}, nil
		},
	})
	approver := &staticToolApprover{decision: ToolApprovalDecision{Allowed: false, Reason: "operator rejected"}}
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{"q":"go"}`)}},
	}}}
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry), WithToolApprover(approver))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "lookup"})
	foundDenied := false
	var terminal RunStreamEvent
	for event := range stream.Events {
		if event.Event.Type == EventApprovalDenied {
			foundDenied = true
		}
		if event.Done {
			terminal = event
		}
	}
	_, err = stream.Wait()
	if !errors.Is(err, ErrApprovalDenied) {
		t.Fatalf("err = %v, want ErrApprovalDenied", err)
	}
	if !foundDenied {
		t.Fatal("missing approval denied stream event")
	}
	if terminal.Result == nil || terminal.Error == nil {
		t.Fatalf("terminal = %#v", terminal)
	}
}
