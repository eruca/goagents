# Durable Tool Approval Resume Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a durable, host-controlled tool-approval interruption and resume path to `agentcore` without introducing workflow or storage dependencies.

**Architecture:** Extend the current approval stage with a fail-closed `Pending` decision and a fourth pipeline result, `StageInterrupt`. Snapshot the completed pre-tool state into a JSON-serializable `RunCheckpoint`; on resume, rebuild the per-run registry, rehydrate request-scoped tools, run current policy, validate all host resolutions, execute once, observe the result, and return to the ordinary ReAct loop.

**Tech Stack:** Go, `agentcore`, `ports`, `tools`, standard-library `encoding/json`, existing mock LLM tests, `go test`.

## Global Constraints

- Keep the feature inside `goagent/agentcore`; do not add databases, HTTP, queues, or workflow dependencies.
- Keep `ToolApprover` compatibility: an existing `{Allowed: true|false}` decision behaves exactly as before.
- Treat `Pending` as higher priority than `Allowed` to fail closed.
- Keep raw tool inputs and checkpoints out of `Event.Metadata` and errors.
- Require a complete, exact, atomic resolution set before running a paused batch.
- Re-run policy before every resumed execution.
- Preserve the existing dirty worktree: stage and commit nothing in this implementation pass.

---

### Task 1: Define the interruption and checkpoint contracts

**Files:**
- Create: `goagent/agentcore/checkpoint.go`
- Modify: `goagent/agentcore/approval.go`
- Modify: `goagent/agentcore/request.go`
- Modify: `goagent/agentcore/events.go`
- Test: `goagent/agentcore/approval_resume_test.go`

**Interfaces:**
- Produces `ErrApprovalPending`, `ErrInvalidRunCheckpoint`, `ErrInvalidApprovalResolution`, `RunCheckpoint`, `ToolApprovalResolution`, and `ToolApprovalInterruption`.
- `RunResult.Interruption` is nil except for a pending approval.

- [x] **Step 1: Write the failing checkpoint test**

```go
func TestRunCheckpointRoundTripsJSON(t *testing.T) {
	checkpoint := RunCheckpoint{
		Version: runCheckpointVersion,
		RunID:   NewRunID().String(),
		Request: CheckpointRequest{Input: "write", Metadata: map[string]any{"tenant": "t1"}},
		PendingCalls: []ports.ToolCall{{ID: "call-1", Name: "write", Input: json.RawMessage(`{"x":1}`)}},
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil { t.Fatalf("Marshal: %v", err) }
	var decoded RunCheckpoint
	if err := json.Unmarshal(encoded, &decoded); err != nil { t.Fatalf("Unmarshal: %v", err) }
	if _, err := decoded.validate(); err != nil { t.Fatalf("validate: %v", err) }
}
```

- [x] **Step 2: Run the test and confirm it fails because checkpoint symbols do not exist**

Run: `cd goagent && go test ./agentcore -run TestRunCheckpointRoundTripsJSON -count=1 -v`

Expected: compile failure naming `RunCheckpoint` or `runCheckpointVersion`.

- [x] **Step 3: Add the versioned DTO and classifiable errors**

Create `checkpoint.go` with these exported types and JSON names:

```go
const runCheckpointVersion = 1

type CheckpointRequest struct {
	Input string `json:"input"`
	UserID string `json:"user_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	AllowedPermissions []policy.Permission `json:"allowed_permissions,omitempty"`
	PolicyContext ports.PolicyContext `json:"policy_context"`
}

type RunCheckpoint struct {
	Version int `json:"version"`
	RunID string `json:"run_id"`
	Request CheckpointRequest `json:"request"`
	Iteration int `json:"iteration"`
	Messages []Message `json:"messages"`
	Usage Usage `json:"usage"`
	LLMCalls int `json:"llm_calls"`
	ToolCalls int `json:"tool_calls"`
	UsedTools []string `json:"used_tools,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Metadata map[string]any `json:"metadata"`
	AllowedPermissions []policy.Permission `json:"allowed_permissions"`
	PendingCalls []ports.ToolCall `json:"pending_calls"`
	PromptBlocks []prompt.Block `json:"prompt_blocks"`
}
```

Implement `validate() (RunID, error)` to reject a version other than `1`, an
invalid/zero Run ID, an empty pending-call list, an empty call name, and an
empty `StartedAt`, always wrapping `ErrInvalidRunCheckpoint`. Add
`ToolApprovalResolution` with `Index`, `ToolCallID`, `Tool`, `Allowed`, and
`Reason`; add `ToolApprovalInterruption{Checkpoint RunCheckpoint}` and add an
optional `Interruption *ToolApprovalInterruption` to `RunResult`. Add the three
sentinel errors. Add `EventApprovalPending = "approval.pending"`.

- [x] **Step 4: Re-run the focused test**

Run: `cd goagent && go test ./agentcore -run TestRunCheckpointRoundTripsJSON -count=1 -v`

Expected: PASS.

### Task 2: Interrupt before execution and return a safe partial result

**Files:**
- Modify: `goagent/agentcore/stage.go`
- Modify: `goagent/agentcore/approval.go`
- Modify: `goagent/agentcore/agent.go`
- Modify: `goagent/agentcore/run_state.go`
- Test: `goagent/agentcore/approval_resume_test.go`

**Interfaces:**
- Consumes `RunCheckpoint` from Task 1.
- Produces `StageInterrupt` and `RunDetailed` partial results with
  `ErrApprovalPending`.

- [x] **Step 1: Write failing pending-approval tests**

```go
func TestAgentRunDetailedStopsBeforePendingApproval(t *testing.T) {
	toolRan := false
	agent := newApprovalResumeAgent(t, &mockLLM{responses: []*ports.ChatResponse{{
		ToolCalls: []ports.ToolCall{{ID: "call-1", Name: "write", Input: json.RawMessage(`{"secret":"value"}`)}},
	}}}, pendingApprover{}, func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
		toolRan = true; return &tools.Result{ForLLM: "unexpected"}, nil
	})
	result, err := agent.RunDetailed(context.Background(), RunRequest{Input: "write"})
	if !errors.Is(err, ErrApprovalPending) { t.Fatalf("err = %v", err) }
	if result == nil || result.Interruption == nil { t.Fatal("missing interruption") }
	if toolRan { t.Fatal("tool ran before approval") }
	if got := result.Interruption.Checkpoint.PendingCalls[0].Name; got != "write" { t.Fatalf("tool = %q", got) }
}
```

Also collect emitted events and assert the `approval.pending` metadata has
`tool` and `index` but no `input`, checkpoint, prompt, or message field.

- [x] **Step 2: Run the test and confirm it fails**

Run: `cd goagent && go test ./agentcore -run TestAgentRunDetailedStopsBeforePendingApproval -count=1 -v`

Expected: compile failure for `pendingApprover` / `ErrApprovalPending`, then a
behavioral failure until `ApprovalStage` interrupts.

- [x] **Step 3: Implement the smallest interruption path**

Add `StageInterrupt` after `StageAbort`. In `ApprovalStage.Run`, check
`decision.Pending` before `decision.Allowed`; emit bounded
`EventApprovalPending` metadata and return `StageInterrupt, nil`. Add
`checkpointFromState` in `checkpoint.go`: clone request/messages/raw calls,
metadata, prompt blocks, usage, summary counters, and start time, then validate
that `json.Marshal(checkpoint)` succeeds before returning it. In `agent.go`,
when `runner.Run` returns `StageInterrupt`, construct a partial `RunResult`
whose `ExecutionSummary.AbortReason` is `ErrApprovalPending.Error()` and whose
interruption contains the checkpoint. `Run` returns nil with that error;
`RunDetailed` returns the partial result with that error.

- [x] **Step 4: Run all approval tests**

Run: `cd goagent && go test ./agentcore -run Approval -count=1 -v`

Expected: existing allow/deny tests and the new pending test PASS.

### Task 3: Validate resolutions and resume the exact pending batch

**Files:**
- Modify: `goagent/agentcore/agent.go`
- Modify: `goagent/agentcore/checkpoint.go`
- Create: `goagent/agentcore/resume_stage.go`
- Test: `goagent/agentcore/approval_resume_test.go`

**Interfaces:**
- Consumes `RunCheckpoint` and complete `[]ToolApprovalResolution`.
- Produces `Agent.Resume` and `Agent.ResumeDetailed`.

- [x] **Step 1: Write failing resume tests**

```go
func TestAgentResumeDetailedExecutesApprovedCallThenContinues(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call-1", Name: "write", Input: json.RawMessage(`{"x":1}`)}}},
		{Content: "done"},
	}}
	approver := &sequenceApprover{decisions: []ToolApprovalDecision{{Pending: true}}}
	toolRuns := 0
	agent := newApprovalResumeAgent(t, llm, approver, func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
		toolRuns++; return &tools.Result{ForLLM: "written"}, nil
	})
	paused, err := agent.RunDetailed(context.Background(), RunRequest{Input: "write"})
	if !errors.Is(err, ErrApprovalPending) { t.Fatalf("pause err = %v", err) }
	result, err := agent.ResumeDetailed(context.Background(), paused.Interruption.Checkpoint, []ToolApprovalResolution{{Index: 0, ToolCallID: "call-1", Tool: "write", Allowed: true}})
	if err != nil { t.Fatalf("ResumeDetailed: %v", err) }
	if result.Content != "done" || toolRuns != 1 || llm.calls != 2 { t.Fatalf("result=%#v toolRuns=%d llm.calls=%d", result, toolRuns, llm.calls) }
}
```

Add tests that a missing, duplicate, swapped, or mismatched resolution returns
an error matching `ErrInvalidApprovalResolution` before a tool or second LLM
call; an atomic batch with one denial returns `ErrApprovalDenied` and executes
no calls; a request-scoped tool provider is invoked both before pausing and
before resume; and a second model-requested pending approval yields a fresh
checkpoint.

- [x] **Step 2: Run the resume tests and confirm they fail because ResumeDetailed does not exist**

Run: `cd goagent && go test ./agentcore -run 'TestAgentResumeDetailed|TestAgentResumeRejects' -count=1 -v`

Expected: compile failure for `ResumeDetailed`.

- [x] **Step 3: Restore runtime state and execute only after validation**

Implement `RunCheckpoint.restore(EventSink) (*RunState, error)` to validate
and rebuild `RunState`, including the private summary lookup and original start
time. It must leave `CompiledPrompt`, `ContextProjection`, `LastResponse`, and
`ToolResults` empty. Implement `validateApprovalResolutions` to require one
resolution at each exact pending-call index and to compare the original tool ID
and name. It must reject duplicates, out-of-range indices, missing entries, and
mismatches with `ErrInvalidApprovalResolution`.

Implement `Agent.Resume` / `ResumeDetailed` through a private `resume` method:

```go
runRegistry := newRunToolRegistry(a.toolRegistry)
if err := a.rehydrateToolProvider(ctx, state, runRegistry); err != nil {
	return nil, err
}
result, err := NewPipeline(
	PolicyStage{Engine: a.policyEngine, ToolRegistry: runRegistry},
	ResolvedApprovalStage{Resolutions: resolutions},
	ActStage{Executor: tools.NewExecutor(runRegistry)},
	ObserveStage{},
).Run(ctx, state)
```

`ResolvedApprovalStage` must validate the whole batch before emitting accepted
events or allowing `ActStage`; when any resolution denies it emits only the
rejection event and wraps `ErrApprovalDenied`. After the mini-pipeline returns
`StageContinue`, build the normal `ReActRunner` with the same Agent options and
call `Run` so the next model invocation observes tool results. If it interrupts
again, return a new partial checkpoint using the Task 2 path.

- [x] **Step 4: Run focused contract tests**

Run: `cd goagent && go test ./agentcore -run 'Approval|Resume' -count=1 -v`

Expected: PASS.

### Task 4: Document host responsibilities and verify module behavior

**Files:**
- Modify: `goagent/README.md`
- Create: `goagent/examples/approval-resume/main.go`
- Create: `goagent/examples/approval-resume/README.md`
- Modify: `goagent/Makefile`
- Modify: `goagent/agentcore/approval.go`
- Test: `goagent/agentcore/approval_resume_test.go`

**Interfaces:**
- Documents the exact `RunDetailed` -> persist -> `ResumeDetailed` flow and
  sensitivity boundary.

- [x] **Step 1: Add the README usage example**

Add a compact example that checks `errors.Is(err, agentcore.ErrApprovalPending)`,
serializes `result.Interruption.Checkpoint` with `json.Marshal`, obtains host
resolutions, and calls `ResumeDetailed`. State that checkpoint storage is
host-owned, sensitive, expiring, and not event payload; state that every call
in a batch needs a matching resolution and that policy runs again at resume.

Add `examples/approval-resume`: it must JSON round-trip a pending checkpoint,
rebuild an Agent and its static tool registry, submit the matching resolution,
and print one executed tool plus the final response. Add this example to
`Makefile` `smoke` so `make verify` proves the public path.

- [x] **Step 2: Add API comments for exported interruption contracts**

Document `Pending` precedence, resolution matching, request-scoped tool
provider determinism, and the fact that the checkpoint includes raw user and
tool data. Do not add unrelated comments or refactors.

- [x] **Step 3: Run formatting and module verification**

Run: `gofmt -w goagent/agentcore/approval.go goagent/agentcore/agent.go goagent/agentcore/checkpoint.go goagent/agentcore/events.go goagent/agentcore/request.go goagent/agentcore/resume_stage.go goagent/agentcore/run_state.go goagent/agentcore/stage.go goagent/agentcore/approval_resume_test.go`

Run: `cd goagent && go test ./agentcore ./tools ./policy && go test ./...`

Expected: PASS.

- [x] **Step 4: Run workspace verification without staging user files**

Run: `bash ./scripts/verify-all.sh && git diff --check && git status --short`

Expected: all verification commands pass; pre-existing modifications remain
unstaged and no unrelated file changes are introduced.
