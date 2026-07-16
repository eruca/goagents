# WorkflowKit Host Orchestration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.
>
> Historical note: this plan predates the final module split. The current
> release contract keeps `github.com/eruca/goagents/workflowkit` free of `goagent`
> imports and places the adapter in `github.com/eruca/goagents/workflowkit/agentstep`.
> See `../contracts.md` and `../release.md`.

**Goal:** Build the first `workflowkit` module as a small host-side orchestration layer for ordered workflow steps and optional `goagent` run adapters.

**Architecture:** `workflowkit` is a standalone module under `/Users/nick/VibeCoding/goagents/workflowkit`. The core package defines workflow run state, a store interface, an in-memory store, and a sequential executor. A small separate `agentstep` module adapts `goagent/agentcore.RunDetailed` into workflow step results without changing `goagent`.

**Tech Stack:** Go 1.26.1, standard library, SQLite for the optional store. `github.com/eruca/goagents/goagent` is used only by the optional `agentstep` module and nested examples via local replace during workspace development.

---

### Task 1: Module Skeleton And Core Types

**Files:**
- Create: `workflowkit/go.mod`
- Create: `workflowkit/run.go`
- Create: `workflowkit/store.go`
- Test: `workflowkit/store_test.go`

**Step 1: Write the failing test**

Create `workflowkit/store_test.go` with a test proving `MemoryStore` returns copies:

```go
func TestMemoryStoreSavesAndReturnsCopies(t *testing.T) {
	store := NewMemoryStore()
	run := WorkflowRun{
		ID:       "wf-1",
		Status:   StatusPending,
		Metadata: map[string]any{"k": "v"},
	}
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	loaded, err := store.Get(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	loaded.Status = StatusSucceeded
	loaded.Metadata["k"] = "changed"

	again, err := store.Get(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if again.Status != StatusPending {
		t.Fatalf("status mutated through loaded copy: %s", again.Status)
	}
	if again.Metadata["k"] != "v" {
		t.Fatalf("metadata mutated through loaded copy: %#v", again.Metadata)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./...`

Expected: FAIL because `NewMemoryStore`, `WorkflowRun`, and statuses are undefined.

**Step 3: Write minimal implementation**

Create `go.mod`, define statuses and `WorkflowRun`, define `Store`, implement `MemoryStore` with clone-on-save and clone-on-get.

**Step 4: Run test to verify it passes**

Run: `go test ./...`

Expected: PASS.

### Task 2: Sequential Executor

**Files:**
- Create: `workflowkit/executor.go`
- Test: `workflowkit/executor_test.go`

**Step 1: Write failing tests**

Add tests for:

- successful ordered steps mark the run `succeeded`
- `waiting_approval` stops execution and persists that status
- returned step error marks the run `failed`

**Step 2: Run test to verify it fails**

Run: `go test ./...`

Expected: FAIL because `Executor`, `Step`, and `StepResult` are undefined.

**Step 3: Write minimal implementation**

Define:

```go
type Step interface {
	Name() string
	Run(context.Context, WorkflowRun) (StepResult, error)
}

type StepResult struct {
	Status     Status
	OutputRef  string
	AgentRunID string
	Error      string
	Metadata   map[string]any
}
```

Implement `Executor.Run(ctx, run)` as ordered execution with store persistence after status changes.

**Step 4: Run test to verify it passes**

Run: `go test ./...`

Expected: PASS.

### Task 3: Agent Step Adapter

**Files:**
- Create: `workflowkit/agentstep/agent_step.go`
- Test: `workflowkit/agentstep/agent_step_test.go`

**Step 1: Write failing tests**

Add tests for:

- agent success maps `RunID` and final content into a successful `StepResult`
- agent error maps partial `RunID` and abort reason into a failed `StepResult`

Use a fake runner implementing:

```go
type Runner interface {
	RunDetailed(context.Context, agentcore.RunRequest) (*agentcore.RunResult, error)
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./...`

Expected: FAIL because `agentstep` package is missing.

**Step 3: Write minimal implementation**

Implement `agentstep.Step` with host-provided request builder and optional result mapper.

**Step 4: Run test to verify it passes**

Run: `go test ./...`

Expected: PASS.

### Task 4: README And Example

**Files:**
- Create: `workflowkit/README.md`
- Create: `workflowkit/examples/basic/main.go`

**Step 1: Add a deterministic example**

The example should run two simple workflow steps against `MemoryStore` and print final status. It should not require network, secrets, real LLMs, or external services.

**Step 2: Run example**

Run: `go run ./examples/basic`

Expected: prints a compact successful workflow line.

**Step 3: Final verification**

Run: `go test ./...`

Expected: PASS.
