package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

func TestRunCheckpointRoundTripsJSON(t *testing.T) {
	checkpoint := RunCheckpoint{
		Version:   runCheckpointVersion,
		RunID:     NewRunID().String(),
		Request:   CheckpointRequest{Input: "write", Metadata: map[string]any{"tenant": "t1"}},
		StartedAt: time.Now().UTC(),
		PendingCalls: []ports.ToolCall{{
			ID:    "call-1",
			Name:  "write",
			Input: json.RawMessage(`{"x":1}`),
		}},
	}

	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded RunCheckpoint
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, err := decoded.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

type pendingToolApprover struct{}

func (pendingToolApprover) ApproveTool(context.Context, ToolApprovalRequest) ToolApprovalDecision {
	return ToolApprovalDecision{Pending: true, Reason: "operator review"}
}

type countingToolProvider struct {
	tools []tools.Tool
	calls int
}

func (p *countingToolProvider) Tools(context.Context, RunRequest) ([]tools.Tool, error) {
	p.calls++
	return append([]tools.Tool(nil), p.tools...), nil
}

type flippingPolicyEngine struct {
	calls int
}

func (e *flippingPolicyEngine) Decide(ports.PolicyRequest) ports.PolicyDecision {
	e.calls++
	return ports.PolicyDecision{Allowed: e.calls == 1, Reason: "policy changed"}
}

func TestAgentRunDetailedStopsBeforePendingApproval(t *testing.T) {
	toolRan := false
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write", Permission: policy.PermissionWrite},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			toolRan = true
			return &tools.Result{ForLLM: "unexpected"}, nil
		},
	})
	sink := &recordingEventSink{}
	agent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{{
			ToolCalls: []ports.ToolCall{{
				ID:    "call-1",
				Name:  "write",
				Input: json.RawMessage(`{"secret":"value"}`),
			}},
		}}}),
		WithToolRegistry(registry),
		WithToolApprover(pendingToolApprover{}),
		WithEventSink(sink),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	result, err := agent.RunDetailed(context.Background(), RunRequest{
		Input:              "write",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("err = %v, want ErrApprovalPending", err)
	}
	if result == nil || result.Interruption == nil {
		t.Fatal("missing interruption")
	}
	if toolRan {
		t.Fatal("tool ran before approval")
	}
	checkpoint := result.Interruption.Checkpoint
	if len(checkpoint.PendingCalls) != 1 || checkpoint.PendingCalls[0].Name != "write" {
		t.Fatalf("pending calls = %#v", checkpoint.PendingCalls)
	}

	foundPending := false
	for _, event := range sink.events {
		if event.Type != EventApprovalPending {
			continue
		}
		foundPending = true
		if event.Metadata["tool"] != "write" || event.Metadata["index"] != 0 {
			t.Fatalf("pending metadata = %#v", event.Metadata)
		}
		for _, forbidden := range []string{"input", "checkpoint", "prompt", "message"} {
			if _, ok := event.Metadata[forbidden]; ok {
				t.Fatalf("pending metadata leaks %q: %#v", forbidden, event.Metadata)
			}
		}
	}
	if !foundPending {
		t.Fatal("missing approval pending event")
	}
}

func TestAgentResumeDetailedExecutesApprovedCallThenContinues(t *testing.T) {
	toolRuns := 0
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write", Permission: policy.PermissionWrite},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			toolRuns++
			return &tools.Result{ForLLM: "written"}, nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call-1", Name: "write", Input: json.RawMessage(`{"x":1}`)}}},
		{Content: "done"},
	}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
		WithToolApprover(pendingToolApprover{}),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	paused, err := agent.RunDetailed(context.Background(), RunRequest{
		Input:              "write",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("pause err = %v", err)
	}
	encoded, err := json.Marshal(paused.Interruption.Checkpoint)
	if err != nil {
		t.Fatalf("Marshal checkpoint: %v", err)
	}
	var checkpoint RunCheckpoint
	if err := json.Unmarshal(encoded, &checkpoint); err != nil {
		t.Fatalf("Unmarshal checkpoint: %v", err)
	}

	result, err := agent.ResumeDetailed(context.Background(), checkpoint, []ToolApprovalResolution{{
		Index:      0,
		ToolCallID: "call-1",
		Tool:       "write",
		Allowed:    true,
	}})
	if err != nil {
		t.Fatalf("ResumeDetailed: %v", err)
	}
	if result == nil || result.Content != "done" {
		t.Fatalf("result = %#v", result)
	}
	if toolRuns != 1 {
		t.Fatalf("tool runs = %d, want 1", toolRuns)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("LLM requests = %d, want 2", len(llm.requests))
	}
	last := llm.requests[1].Messages[len(llm.requests[1].Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "call-1" || last.Content != "written" {
		t.Fatalf("last resumed message = %#v", last)
	}
}

func TestAgentResumeDetailedRejectsMismatchedResolutionBeforeExecution(t *testing.T) {
	toolRuns := 0
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write", Permission: policy.PermissionWrite},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			toolRuns++
			return &tools.Result{ForLLM: "written"}, nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call-1", Name: "write", Input: json.RawMessage(`{"x":1}`)}}},
		{Content: "unexpected"},
	}}
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry), WithToolApprover(pendingToolApprover{}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	paused, err := agent.RunDetailed(context.Background(), RunRequest{
		Input:              "write",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("pause err = %v", err)
	}

	_, err = agent.ResumeDetailed(context.Background(), paused.Interruption.Checkpoint, []ToolApprovalResolution{{
		Index:      0,
		ToolCallID: "call-1",
		Tool:       "different",
		Allowed:    true,
	}})
	if !errors.Is(err, ErrInvalidApprovalResolution) {
		t.Fatalf("err = %v, want ErrInvalidApprovalResolution", err)
	}
	if toolRuns != 0 {
		t.Fatalf("tool runs = %d, want 0", toolRuns)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("LLM requests = %d, want 1", len(llm.requests))
	}
}

func TestAgentResumeDetailedRejectsMixedBatchAtomically(t *testing.T) {
	toolRuns := 0
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write", Permission: policy.PermissionWrite},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			toolRuns++
			return &tools.Result{ForLLM: "written"}, nil
		},
	})
	agent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{{
			ToolCalls: []ports.ToolCall{
				{ID: "call-1", Name: "write", Input: json.RawMessage(`{"x":1}`)},
				{ID: "call-2", Name: "write", Input: json.RawMessage(`{"x":2}`)},
			},
		}}}),
		WithToolRegistry(registry),
		WithToolApprover(pendingToolApprover{}),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	paused, err := agent.RunDetailed(context.Background(), RunRequest{
		Input:              "write twice",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("pause err = %v", err)
	}

	result, err := agent.ResumeDetailed(context.Background(), paused.Interruption.Checkpoint, []ToolApprovalResolution{
		{Index: 0, ToolCallID: "call-1", Tool: "write", Allowed: true},
		{Index: 1, ToolCallID: "call-2", Tool: "write", Allowed: false, Reason: "operator rejected"},
	})
	if !errors.Is(err, ErrApprovalDenied) {
		t.Fatalf("err = %v, want ErrApprovalDenied", err)
	}
	if result == nil {
		t.Fatal("missing detailed result")
	}
	if toolRuns != 0 {
		t.Fatalf("tool runs = %d, want 0", toolRuns)
	}
}

func TestAgentResumeDetailedRehydratesRequestScopedTools(t *testing.T) {
	toolRuns := 0
	provider := &countingToolProvider{tools: []tools.Tool{testAgentTool{
		spec: tools.Spec{Name: "provided", Permission: policy.PermissionRead},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			toolRuns++
			return &tools.Result{ForLLM: "provided result"}, nil
		},
	}}}
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call-1", Name: "provided", Input: json.RawMessage(`{}`)}}},
		{Content: "done"},
	}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolProvider(provider),
		WithToolApprover(pendingToolApprover{}),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	paused, err := agent.RunDetailed(context.Background(), RunRequest{Input: "use provided tool"})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("pause err = %v", err)
	}
	result, err := agent.ResumeDetailed(context.Background(), paused.Interruption.Checkpoint, []ToolApprovalResolution{{
		Index:      0,
		ToolCallID: "call-1",
		Tool:       "provided",
		Allowed:    true,
	}})
	if err != nil || result == nil || result.Content != "done" {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.calls)
	}
	if toolRuns != 1 {
		t.Fatalf("tool runs = %d, want 1", toolRuns)
	}
}

func TestAgentResumeDetailedRechecksPolicyBeforeToolExecution(t *testing.T) {
	toolRuns := 0
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write", Permission: policy.PermissionWrite},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			toolRuns++
			return &tools.Result{ForLLM: "written"}, nil
		},
	})
	policyEngine := &flippingPolicyEngine{}
	agent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{{
			ToolCalls: []ports.ToolCall{{ID: "call-1", Name: "write", Input: json.RawMessage(`{}`)}},
		}}}),
		WithToolRegistry(registry),
		WithPolicyEngine(policyEngine),
		WithToolApprover(pendingToolApprover{}),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	paused, err := agent.RunDetailed(context.Background(), RunRequest{Input: "write"})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("pause err = %v", err)
	}
	_, err = agent.ResumeDetailed(context.Background(), paused.Interruption.Checkpoint, []ToolApprovalResolution{{
		Index:      0,
		ToolCallID: "call-1",
		Tool:       "write",
		Allowed:    true,
	}})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
	if policyEngine.calls != 2 {
		t.Fatalf("policy calls = %d, want 2", policyEngine.calls)
	}
	if toolRuns != 0 {
		t.Fatalf("tool runs = %d, want 0", toolRuns)
	}
}

func TestAgentResumeDetailedCanPauseAgain(t *testing.T) {
	toolRuns := 0
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write", Permission: policy.PermissionWrite},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			toolRuns++
			return &tools.Result{ForLLM: "written"}, nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call-1", Name: "write", Input: json.RawMessage(`{"x":1}`)}}},
		{ToolCalls: []ports.ToolCall{{ID: "call-2", Name: "write", Input: json.RawMessage(`{"x":2}`)}}},
	}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
		WithToolApprover(pendingToolApprover{}),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	paused, err := agent.RunDetailed(context.Background(), RunRequest{
		Input:              "write twice",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("pause err = %v", err)
	}
	result, err := agent.ResumeDetailed(context.Background(), paused.Interruption.Checkpoint, []ToolApprovalResolution{{
		Index:      0,
		ToolCallID: "call-1",
		Tool:       "write",
		Allowed:    true,
	}})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("resume err = %v, want ErrApprovalPending", err)
	}
	if result == nil || result.Interruption == nil {
		t.Fatal("missing second interruption")
	}
	if result.Interruption.Checkpoint.PendingCalls[0].ID != "call-2" {
		t.Fatalf("second pending calls = %#v", result.Interruption.Checkpoint.PendingCalls)
	}
	if toolRuns != 1 {
		t.Fatalf("tool runs = %d, want 1", toolRuns)
	}
}

func TestAgentResumeDetailedPreservesMaxIterationBudget(t *testing.T) {
	toolRuns := 0
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write", Permission: policy.PermissionWrite},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			toolRuns++
			return &tools.Result{ForLLM: "written"}, nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call-1", Name: "write", Input: json.RawMessage(`{}`)}}},
		{Content: "must not be requested"},
	}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
		WithToolApprover(pendingToolApprover{}),
		WithMaxIterations(1),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	paused, err := agent.RunDetailed(context.Background(), RunRequest{
		Input:              "write",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("pause err = %v", err)
	}

	_, err = agent.ResumeDetailed(context.Background(), paused.Interruption.Checkpoint, []ToolApprovalResolution{{
		Index:      0,
		ToolCallID: "call-1",
		Tool:       "write",
		Allowed:    true,
	}})
	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
	if toolRuns != 1 {
		t.Fatalf("tool runs = %d, want 1", toolRuns)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("LLM requests = %d, want 1", len(llm.requests))
	}
}

func TestAgentStreamReturnsPendingApprovalInterruption(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "unexpected"}, nil
		},
	})
	agent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{{
			ToolCalls: []ports.ToolCall{{ID: "call-1", Name: "lookup", Input: json.RawMessage(`{}`)}},
		}}}),
		WithToolRegistry(registry),
		WithToolApprover(pendingToolApprover{}),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "lookup"})
	foundPending := false
	var terminal RunStreamEvent
	for event := range stream.Events {
		if event.Event.Type == EventApprovalPending {
			foundPending = true
		}
		if event.Done {
			terminal = event
		}
	}
	result, err := stream.Wait()
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("Wait err = %v, want ErrApprovalPending", err)
	}
	if !foundPending {
		t.Fatal("missing pending event")
	}
	if result == nil || result.Interruption == nil || terminal.Result == nil || terminal.Result.Interruption == nil {
		t.Fatalf("result = %#v, terminal = %#v", result, terminal)
	}
}
