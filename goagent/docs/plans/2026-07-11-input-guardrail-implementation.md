# Input Guardrail Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a host-controlled input guardrail that rejects a request before memory, context, model, or tool execution.

**Architecture:** `InputGuardStage` is inserted as the first ReAct stage and runs the host `InputGuard` at most once per run. It returns the stable, content-free `ErrInputRejected` on host rejection and emits bounded input lifecycle events; existing output and tool guardrails remain unchanged.

**Tech Stack:** Go, existing `agentcore` Stage/Pipeline contracts, `go test`.

## Global Constraints

- Change only `goagent/agentcore` and its documentation; do not introduce a guardrail registry, model calls, storage, or transport.
- A guard rejection must not expose its host error or raw input through events, `AbortReason`, or framework errors.
- Input validation happens before `MemoryLoadStage` and `ContextStage` and at most once for a checkpointed request.
- Preserve the current `OutputValidator`, `PolicyStage`, and `ApprovalStage` behavior.
- Do not stage or commit because the main worktree already contains unrelated user changes.

---

### Task 1: Define the input guardrail contract and block before memory

**Files:**
- Create: `goagent/agentcore/input_guard.go`
- Modify: `goagent/agentcore/agent.go`
- Modify: `goagent/agentcore/events.go`
- Modify: `goagent/agentcore/finalize_stage.go`
- Test: `goagent/agentcore/input_guard_test.go`

**Interfaces:**
- Produces `ErrInputRejected`, `InputGuard`, `InputGuardRequest`, `InputGuardFunc`, `InputGuardStage`, and `WithInputGuard`.
- `ReActConfig.InputGuard` passes the configured guard into the first pipeline stage.

- [x] **Step 1: Write rejection tests before production code**

```go
func TestInputGuardRejectsBeforeMemoryAndLLM(t *testing.T) {
	memory := &mockMemoryProvider{}
	agent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{{Content: "must not run"}}}),
		WithMemoryProvider(memory),
		WithInputGuard(InputGuardFunc(func(context.Context, InputGuardRequest) error {
			return errors.New("secret diagnostic")
		})),
	)
	if err != nil { t.Fatalf("NewAgent: %v", err) }
	result, err := agent.RunDetailed(context.Background(), RunRequest{Input: "private request", SessionID: "s1"})
	if !errors.Is(err, ErrInputRejected) { t.Fatalf("err = %v", err) }
	if result.ExecutionSummary.AbortReason != ErrInputRejected.Error() { t.Fatalf("abort = %q", result.ExecutionSummary.AbortReason) }
	if memory.loadCalls != 0 { t.Fatalf("memory loads = %d", memory.loadCalls) }
}
```

Also assert the mock LLM receives no request and `input.rejected` event metadata is empty.

- [x] **Step 2: Run the test and confirm missing symbols fail**

Run: `cd goagent && go test ./agentcore -run TestInputGuardRejectsBeforeMemoryAndLLM -count=1 -v`

Expected: compile failure for `WithInputGuard`, `InputGuardFunc`, and `ErrInputRejected`.

- [x] **Step 3: Implement the minimal contract and first pipeline stage**

Create `input_guard.go` with:

```go
var ErrInputRejected = errors.New("input rejected")

type InputGuard interface {
	ValidateInput(context.Context, InputGuardRequest) error
}

type InputGuardFunc func(context.Context, InputGuardRequest) error

func (f InputGuardFunc) ValidateInput(ctx context.Context, req InputGuardRequest) error { return f(ctx, req) }
```

`InputGuardStage.Run` returns `StageContinue` when no guard is configured or
the `agentcore.input_guard.validated` metadata flag is true. Otherwise it calls
the guard with cloned request metadata and policy context. A rejection emits
`EventInputRejected` with no metadata and returns `StageAbort, ErrInputRejected`.
A pass sets the flag and emits `EventInputValidated` with no metadata.

Add `inputGuard InputGuard` to `Agent`, `WithInputGuard`, and
`ReActConfig.InputGuard`. Put `InputGuardStage{Guard: config.InputGuard}` before
`MemoryLoadStage` in `NewReActRunner`.

- [x] **Step 4: Run rejection and existing output tests**

Run: `cd goagent && go test ./agentcore -run 'TestInputGuardRejectsBeforeMemoryAndLLM|TestOutput' -count=1 -v`

Expected: PASS.

### Task 2: Prove exactly-once and durable-resume behavior

**Files:**
- Modify: `goagent/agentcore/input_guard_test.go`
- Modify: `goagent/agentcore/approval_resume_test.go`
- Modify: `goagent/README.md`

**Interfaces:**
- Consumes the metadata flag created by Task 1.
- Proves repeated ReAct turns and a durable approval resume do not re-screen the same request.

- [x] **Step 1: Write the once-per-request tests**

```go
type countingInputGuard struct { calls int }

func (g *countingInputGuard) ValidateInput(context.Context, InputGuardRequest) error {
	g.calls++
	return nil
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
	agent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}}},
			{Content: "done"},
		}}),
		WithToolRegistry(registry),
		WithInputGuard(guard),
	)
	if err != nil { t.Fatalf("NewAgent: %v", err) }
	if _, err := agent.Run(context.Background(), RunRequest{Input: "lookup"}); err != nil { t.Fatalf("Run: %v", err) }
	if guard.calls != 1 { t.Fatalf("guard calls = %d, want 1", guard.calls) }
}
```

Add a durable approval test that pauses a write call, JSON round-trips its
checkpoint, resumes with a matching resolution, and asserts `guard.calls == 1`.

- [x] **Step 2: Run the tests and confirm the resume case preserves the metadata flag**

Run: `cd goagent && go test ./agentcore -run 'TestInputGuardRunsOnceAcrossToolIteration|TestInputGuardDoesNotRepeatAfterApprovalResume' -count=1 -v`

Expected: the initial contract implementation passes the iteration test; the
resume assertion verifies the checkpoint carries the same metadata flag.

- [x] **Step 3: Document the three guardrail boundaries**

Add `ErrInputRejected` to the README error list. State that `WithInputGuard`
screens raw requests before memory/context, `WithOutputValidator` validates the
final answer, and Policy/Approval protect concrete tool calls. State that guard
diagnostics belong in host-controlled secure logging, not framework events.

- [x] **Step 4: Run module and workspace verification**

Run: `cd goagent && gofmt -w agentcore/input_guard.go agentcore/input_guard_test.go agentcore/agent.go agentcore/events.go agentcore/finalize_stage.go && go test ./... && go test -race ./...`

Run: `cd .. && bash ./scripts/verify-all.sh && git diff --check`

Expected: both commands exit 0.
