package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

func TestPolicyStageAllowsReadTool(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(policyTestTool{spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead}})
	state := NewRunState(NewRunID(), RunRequest{})
	state.PendingCalls = []tools.Call{{Name: "lookup", Input: json.RawMessage(`{}`)}}

	result, err := PolicyStage{Engine: policy.NewEngine(), ToolRegistry: registry}.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result != StageContinue {
		t.Fatalf("result = %v", result)
	}
}

func TestPolicyStageDeniesWriteBeforeExecution(t *testing.T) {
	called := false
	registry := tools.NewRegistry()
	registry.Register(policyTestTool{
		spec: tools.Spec{Name: "write_file", Permission: policy.PermissionWrite},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			called = true
			return &tools.Result{ForLLM: "wrote"}, nil
		},
	})
	state := NewRunState(NewRunID(), RunRequest{})
	state.PendingCalls = []tools.Call{{Name: "write_file", Input: json.RawMessage(`{}`)}}

	result, err := PolicyStage{Engine: policy.NewEngine(), ToolRegistry: registry}.Run(context.Background(), state)
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
	if result != StageAbort {
		t.Fatalf("result = %v", result)
	}
	if called {
		t.Fatal("tool body was called")
	}
}

func TestPolicyStageAllowsRequestScopedWritePermission(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(policyTestTool{spec: tools.Spec{Name: "write_file", Permission: policy.PermissionWrite}})
	state := NewRunState(NewRunID(), RunRequest{})
	state.PendingCalls = []tools.Call{{Name: "write_file", Input: json.RawMessage(`{}`)}}
	state.AllowedPermissions = []policy.Permission{policy.PermissionWrite}

	result, err := PolicyStage{Engine: policy.NewEngine(), ToolRegistry: registry}.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result != StageContinue {
		t.Fatalf("result = %v", result)
	}
}

func TestPolicyStagePassesRunContextToPolicyEngine(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(policyTestTool{spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead}})
	runID := NewRunID()
	input := json.RawMessage(`{"q":"go"}`)
	engine := &capturingPolicyEngine{decision: ports.PolicyDecision{Allowed: true, Reason: "ok"}}
	state := NewRunState(runID, RunRequest{
		UserID:    "user-1",
		SessionID: "session-1",
		Metadata: map[string]any{
			"scope": "unit-test",
		},
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
		PolicyContext: ports.PolicyContext{
			TenantID:  "tenant-1",
			RequestID: "request-1",
			TraceID:   "trace-1",
			Labels:    map[string]string{"risk": "low"},
		},
	})
	state.PendingCalls = []tools.Call{{Name: "lookup", Input: input}}

	result, err := PolicyStage{Engine: engine, ToolRegistry: registry}.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result != StageContinue {
		t.Fatalf("result = %v", result)
	}
	if engine.called != 1 {
		t.Fatalf("policy calls = %d, want 1", engine.called)
	}
	got := engine.request
	if got.RunID != runID.String() {
		t.Fatalf("RunID = %q, want %q", got.RunID, runID.String())
	}
	if got.UserID != "user-1" || got.SessionID != "session-1" {
		t.Fatalf("user/session = %q/%q", got.UserID, got.SessionID)
	}
	if got.Tool != "lookup" || got.Permission != policy.PermissionRead {
		t.Fatalf("tool/permission = %q/%q", got.Tool, got.Permission)
	}
	if string(got.Input) != string(input) {
		t.Fatalf("input = %s, want %s", got.Input, input)
	}
	if len(got.Allowed) != 1 || got.Allowed[0] != policy.PermissionWrite {
		t.Fatalf("allowed = %#v", got.Allowed)
	}
	if got.Context.TenantID != "tenant-1" || got.Context.RequestID != "request-1" || got.Context.TraceID != "trace-1" {
		t.Fatalf("context = %#v", got.Context)
	}
	if got.Context.Labels["risk"] != "low" {
		t.Fatalf("labels = %#v", got.Context.Labels)
	}
	if got.Metadata["scope"] != "unit-test" {
		t.Fatalf("metadata = %#v", got.Metadata)
	}
}

func TestPolicyStageReturnsMissingToolError(t *testing.T) {
	state := NewRunState(NewRunID(), RunRequest{})
	state.PendingCalls = []tools.Call{{Name: "missing", Input: json.RawMessage(`{}`)}}

	result, err := PolicyStage{Engine: policy.NewEngine(), ToolRegistry: tools.NewRegistry()}.Run(context.Background(), state)
	if err == nil || !strings.Contains(err.Error(), `tool "missing" not registered`) {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, tools.ErrToolNotFound) {
		t.Fatalf("err = %v, want ErrToolNotFound", err)
	}
	if result != StageAbort {
		t.Fatalf("result = %v", result)
	}
}

type capturingPolicyEngine struct {
	called   int
	request  ports.PolicyRequest
	decision ports.PolicyDecision
}

func (e *capturingPolicyEngine) Decide(req ports.PolicyRequest) ports.PolicyDecision {
	e.called++
	e.request = req
	return e.decision
}

type policyTestTool struct {
	spec tools.Spec
	run  func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error)
}

func (t policyTestTool) Spec() tools.Spec {
	return t.spec
}

func (t policyTestTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	if t.run == nil {
		return &tools.Result{ForLLM: "ok"}, nil
	}
	return t.run(ctx, input, env)
}

var _ ports.Tool = policyTestTool{}
