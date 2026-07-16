# Tool Approval Gate Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a synchronous host-controlled approval gate before tool execution.

**Architecture:** Introduce `ToolApprover` and `ApprovalStage` in `agentcore`, placed between `PolicyStage` and `ActStage`. The stage approves concrete pending tool calls after policy has allowed them and before execution. It emits bounded approval events, aborts with `ErrApprovalDenied` on denial, and preserves existing behavior when no approver is configured.

**Tech Stack:** Go, existing `agentcore` pipeline/stage contracts, mock LLM tests, `go test`, `make verify`.

---

### Task 1: Add Approval Contract And Allow Path

**Files:**
- Create: `agentcore/approval.go`
- Test: `agentcore/approval_stage_test.go`
- Modify: `agentcore/agent.go`
- Modify: `agentcore/finalize_stage.go`

**Step 1: Write the failing tests**

Create `agentcore/approval_stage_test.go`:

```go
package agentcore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
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
```

**Step 2: Run tests to verify failure**

Run:

```bash
go test ./agentcore -run 'TestApprovalStageContinuesWithoutApprover|TestAgentRunsToolWhenApproverAllows' -count=1 -v
```

Expected: FAIL because `ToolApprover`, `ToolApprovalDecision`, `ApprovalStage`, and `WithToolApprover` do not exist.

**Step 3: Implement minimal approval contract**

Add `agentcore/approval.go`:

```go
package agentcore

import (
	"context"
	"encoding/json"
)

type ToolApprover interface {
	ApproveTool(ctx context.Context, req ToolApprovalRequest) ToolApprovalDecision
}

type ToolApprovalRequest struct {
	RunID     RunID
	UserID    string
	SessionID string
	Tool      string
	Input     json.RawMessage
	Metadata  map[string]any
}

type ToolApprovalDecision struct {
	Allowed bool
	Reason  string
}

type ApprovalStage struct {
	Approver ToolApprover
}

func (s ApprovalStage) Name() string {
	return "approval"
}

func (s ApprovalStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Approver == nil || len(state.PendingCalls) == 0 {
		return StageContinue, nil
	}
	for _, call := range state.PendingCalls {
		decision := s.Approver.ApproveTool(ctx, ToolApprovalRequest{
			RunID:     state.RunID,
			UserID:    state.Input.UserID,
			SessionID: state.Input.SessionID,
			Tool:      call.Name,
			Input:     call.Input,
			Metadata:  state.Metadata,
		})
		if !decision.Allowed {
			return StageAbort, approvalDeniedError(call.Name, decision.Reason)
		}
	}
	return StageContinue, nil
}
```

Add an unexported helper for now; Task 2 will add the exported sentinel:

```go
func approvalDeniedError(tool string, reason string) error {
	if reason == "" {
		reason = "approval denied"
	}
	return fmt.Errorf("tool %q approval denied: %s", tool, reason)
}
```

Modify `agentcore/agent.go`:

```go
type Agent struct {
	// existing fields...
	toolApprover ToolApprover
}

func WithToolApprover(approver ToolApprover) Option {
	return func(a *Agent) {
		a.toolApprover = approver
	}
}
```

Thread it into `ReActConfig`.

Modify `agentcore/finalize_stage.go`:

```go
type ReActConfig struct {
	// existing fields...
	ToolApprover ToolApprover
}
```

Insert the stage:

```go
PolicyStage{Engine: config.PolicyEngine, ToolRegistry: registry},
ApprovalStage{Approver: config.ToolApprover},
ActStage{Executor: tools.NewExecutor(registry)},
```

**Step 4: Run tests**

Run:

```bash
go test ./agentcore -run 'TestApprovalStageContinuesWithoutApprover|TestAgentRunsToolWhenApproverAllows' -count=1 -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/approval.go agentcore/approval_stage_test.go agentcore/agent.go agentcore/finalize_stage.go
git commit -m "feat: 增加工具审批门禁"
```

### Task 2: Add Denial Error And Partial Result Semantics

**Files:**
- Modify: `agentcore/approval.go`
- Modify: `agentcore/approval_stage_test.go`
- Modify: `README.md`

**Step 1: Write failing tests**

Append tests:

```go
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
		Usage: ports.Usage{InputTokens: 5, OutputTokens: 2},
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
```

Add `errors` import.

**Step 2: Run tests to verify failure**

Run:

```bash
go test ./agentcore -run 'ApprovalDeny|ApproverDenies' -count=1 -v
```

Expected: FAIL because `ErrApprovalDenied` is not exported/wrapped.

**Step 3: Implement classifiable error**

In `agentcore/approval.go`:

```go
var ErrApprovalDenied = errors.New("approval denied")

func approvalDeniedError(tool string, reason string) error {
	if reason == "" {
		reason = "approval denied"
	}
	return fmt.Errorf("%w: tool %q denied: %s", ErrApprovalDenied, tool, reason)
}
```

Update imports.

**Step 4: Run tests**

Run:

```bash
go test ./agentcore -run 'Approval' -count=1 -v
```

Expected: PASS.

**Step 5: Update README**

Add `agentcore.ErrApprovalDenied` to the classifiable error list.

Add a short section after `Policy`:

```markdown
## Tool Approval

Use `WithToolApprover` when a host needs to approve model-requested tool calls after policy checks and before execution. If no approver is configured, runs behave as before.

Approval denial aborts the run before the tool body executes. `RunDetailed` and `Agent.Stream` return partial results with the approval denial reason in `ExecutionSummary.AbortReason`.
```

**Step 6: Commit**

```bash
git add agentcore/approval.go agentcore/approval_stage_test.go README.md
git commit -m "feat: 支持工具审批拒绝"
```

### Task 3: Add Approval Events And Stream Coverage

**Files:**
- Modify: `agentcore/events.go`
- Modify: `agentcore/approval.go`
- Modify: `agentcore/approval_stage_test.go`

**Step 1: Write failing tests**

Add:

```go
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
```

**Step 2: Run tests to verify failure**

Run:

```bash
go test ./agentcore -run 'Approval.*Event|StreamIncludesApprovalDeniedEvent' -count=1 -v
```

Expected: FAIL because approval event constants are missing and `ApprovalStage` does not emit events.

**Step 3: Implement events**

Add constants to `agentcore/events.go`:

```go
EventApprovalRequested EventType = "approval.requested"
EventApprovalCompleted EventType = "approval.completed"
EventApprovalDenied    EventType = "approval.denied"
```

In `ApprovalStage.Run`, emit:

- `approval.requested` before calling the approver;
- `approval.completed` when allowed;
- `approval.denied` before aborting.

Use metadata:

```go
map[string]any{"index": i, "tool": call.Name}
```

Add `reason` only when non-empty.

**Step 4: Run tests**

Run:

```bash
go test ./agentcore -run 'Approval' -count=1 -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/events.go agentcore/approval.go agentcore/approval_stage_test.go
git commit -m "feat: 增加工具审批事件"
```

### Task 4: Documentation And Full Verification

**Files:**
- Modify: `README.md`
- Modify: `docs/plans/2026-04-25-tool-approval-gate-design.md` if the final API differs

**Step 1: Review docs**

Ensure README documents:

- `WithToolApprover`;
- approval runs after policy and before tool execution;
- no approver means existing behavior;
- denial matches `ErrApprovalDenied`;
- `Agent.Stream` receives approval events.

**Step 2: Run full verification**

Run:

```bash
make verify
```

Expected: PASS.

**Step 3: Inspect status**

Run:

```bash
git status --short --branch
```

Expected: clean working tree.

**Step 4: Commit docs if needed**

If Task 2 README changes already cover docs and no docs changed in this task, skip commit. Otherwise:

```bash
git add README.md docs/plans/2026-04-25-tool-approval-gate-design.md
git commit -m "docs: 说明工具审批门禁"
```

**Step 5: Report**

Summarize:

- approval contract and stage added;
- allow/deny behavior tested;
- stream approval events tested;
- `make verify` result.
