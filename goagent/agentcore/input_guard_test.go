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

type countingInputGuard struct {
	calls    int
	requests []InputGuardRequest
	err      error
}

func (g *countingInputGuard) ValidateInput(_ context.Context, request InputGuardRequest) error {
	g.calls++
	g.requests = append(g.requests, request)
	return g.err
}

func TestInputGuardRejectsBeforeMemoryAndLLM(t *testing.T) {
	memory := &mockMemoryProvider{}
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "must not run"}}}
	sink := &recordingEventSink{}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
		WithEventSink(sink),
		WithInputGuard(InputGuardFunc(func(context.Context, InputGuardRequest) error {
			return errors.New("secret diagnostic must not leak")
		})),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	result, err := agent.RunDetailed(context.Background(), RunRequest{
		Input:     "private request",
		SessionID: "s1",
	})
	if !errors.Is(err, ErrInputRejected) {
		t.Fatalf("err = %v, want ErrInputRejected", err)
	}
	if result == nil || result.ExecutionSummary.AbortReason != ErrInputRejected.Error() {
		t.Fatalf("result = %#v", result)
	}
	if memory.loadCalls != 0 {
		t.Fatalf("memory loads = %d, want 0", memory.loadCalls)
	}
	if len(llm.requests) != 0 {
		t.Fatalf("LLM requests = %d, want 0", len(llm.requests))
	}

	foundRejected := false
	for _, event := range sink.events {
		if event.Type != EventInputRejected {
			continue
		}
		foundRejected = true
		if event.Message != "" || len(event.Metadata) != 0 {
			t.Fatalf("rejected event = %#v", event)
		}
	}
	if !foundRejected {
		t.Fatal("missing input rejected event")
	}
}

func TestInputGuardRunsOnceAcrossToolIteration(t *testing.T) {
	guard := &countingInputGuard{}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "found"}, nil
		},
	})
	sink := &recordingEventSink{}
	agent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}}},
			{Content: "done"},
		}}),
		WithToolRegistry(registry),
		WithInputGuard(guard),
		WithEventSink(sink),
	)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{
		Input:    "lookup",
		Metadata: map[string]any{"request_scope": "test"},
		PolicyContext: ports.PolicyContext{
			TenantID: "tenant-1",
			Labels:   map[string]string{"source": "test"},
		},
	})
	if err != nil || result.Content != "done" {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if guard.calls != 1 {
		t.Fatalf("guard calls = %d, want 1", guard.calls)
	}
	request := guard.requests[0]
	if request.Input != "lookup" || request.Metadata["request_scope"] != "test" || request.PolicyContext.TenantID != "tenant-1" || request.PolicyContext.Labels["source"] != "test" {
		t.Fatalf("guard request = %#v", request)
	}
	validated := 0
	for _, event := range sink.events {
		if event.Type == EventInputValidated {
			validated++
			if event.Message != "" || len(event.Metadata) != 0 {
				t.Fatalf("validated event = %#v", event)
			}
		}
	}
	if validated != 1 {
		t.Fatalf("input validated events = %d, want 1", validated)
	}
}

func TestInputGuardDoesNotRepeatAfterApprovalResume(t *testing.T) {
	guard := &countingInputGuard{}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write", Permission: policy.PermissionWrite},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
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
		WithInputGuard(guard),
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
	if err != nil || result.Content != "done" {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	if guard.calls != 1 {
		t.Fatalf("guard calls = %d, want 1", guard.calls)
	}
}
