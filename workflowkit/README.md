# WorkflowKit

`workflowkit` is a host-side orchestration module for long-running application
flows with workflow state, approval waits, audit references, artifacts, and
external capabilities.

It is not an agent runtime. `goagent` owns a single ReAct-style agent run;
`workflowkit` owns the application lifecycle around one or more runs.

## Install

```bash
go get github.com/eruca/goagents/workflowkit
```

For local development before publishing:

```bash
go mod edit -replace github.com/eruca/goagents/workflowkit=/Users/nick/VibeCoding/goagents/workflowkit
go get github.com/eruca/goagents/workflowkit
```

## Boundary

Keep the dependency direction at the application boundary:

```text
application
  imports github.com/eruca/goagents/workflowkit
  imports github.com/eruca/goagents/goagent
  imports github.com/eruca/goagents/workflowkit/agentstep when needed
  imports capability modules such as github.com/eruca/goagents/ocrs

github.com/eruca/goagents/goagent does not import github.com/eruca/goagents/workflowkit
github.com/eruca/goagents/workflowkit does not import github.com/eruca/goagents/goagent
```

`workflowkit` currently provides:

- `WorkflowRun` and stable workflow statuses
- `Store` and `MemoryStore`
- ordered `Step` execution through `Executor`
- step progress through `CurrentStep` and `CompletedSteps`
- step history through `StepRecords`
- `Continue` for resuming a persisted run after `waiting_approval`
- `AuditRef`, `InputRef`, `OutputRef`, `AgentRunID`, and `ApprovalRef` for host-owned references
- `WorkflowQueryStore` for host-owned workflow list/query views
- `QueueStore` for worker claim proofs and `QueueLeaseStore` for host-owned lease lifecycle proofs
- lifecycle guards for `Run`, `Continue`, and `Cancel`
- explicit retry policy for transient step errors
- `sqlitestore` for SQLite-backed persistence
- `storetest` conformance suite for Store implementations

It intentionally does not provide DAG execution, distributed worker loops, cron,
durable databases, UI, or multi-agent team orchestration.

## Basic Workflow

```go
store := workflowkit.NewMemoryStore()
executor := workflowkit.NewExecutor(store, []workflowkit.Step{
	prepareStep{},
	finalizeStep{},
})

run, err := executor.Run(ctx, workflowkit.WorkflowRun{
	ID:       "wf-1",
	Status:   workflowkit.StatusPending,
	InputRef: "artifact:input",
})
```

Steps return `StepResult` values. Returning `waiting_approval`, `failed`, or
`cancelled` stops the executor and persists the current run. Returning a Go error
marks the workflow failed.

When a step returns `waiting_approval`, include host-owned references instead of
raw payloads:

```go
return workflowkit.StepResult{
	Status:        workflowkit.StatusWaitingApproval,
	ApprovalRef:   "approval:req-1",
	WaitingReason: "operator approval required",
}
```

After the host records approval, call:

```go
run, err = executor.Approve(ctx, "wf-1", workflowkit.Approval{
	AuditRef: "audit:approval-recorded",
	Metadata: map[string]any{
		"approved_by": "operator-1",
	},
})
```

`Approve` stores host-owned audit refs and bounded metadata, then continues the
workflow from the next step. Use `Continue` directly only when the host has
already updated the run itself.

## Lifecycle

Workflow status transitions are guarded:

```text
pending -> running
running -> waiting_approval
running -> succeeded
running -> failed
running -> cancelled
waiting_approval -> running
waiting_approval -> cancelled
```

`Run` accepts only `pending` or an empty status. `Continue` and `Approve` accept
only `waiting_approval`. `Cancel` accepts `pending`, `running`, or
`waiting_approval`. `succeeded`, `failed`, and `cancelled` are terminal.

Invalid transitions return `InvalidTransitionError` and match
`ErrInvalidTransition` with `errors.Is`.

Step results may use only `running`, `waiting_approval`, `succeeded`, `failed`,
or `cancelled`; an empty status means the step succeeded and the workflow should
continue. `pending` is only an initial workflow status. Unknown step result
statuses return `InvalidStatusError`, match `ErrInvalidStatus` with `errors.Is`,
mark the workflow failed, and record the attempt as failed.

Steps that return `failed` or `cancelled` stop the workflow but are not added to
`CompletedSteps`. Steps that return `waiting_approval` are added to
`CompletedSteps` so `Continue` and `Approve` resume from the next step.

## Retry

Retries are opt-in. By default, even transient errors are attempted once.

Configure retry policy on the executor:

```go
executor := workflowkit.NewExecutor(store, steps,
	workflowkit.WithRetryPolicy(workflowkit.RetryPolicy{MaxAttempts: 3}),
)
```

Only errors marked as transient are retried:

```go
return workflowkit.StepResult{}, workflowkit.TransientError{Err: err}
```

Plain Go errors fail the workflow immediately. Retry is intentionally local to a
step execution; it is not a distributed scheduler or delayed job queue.

## Step History

`StepRecords` is the workflow observability trail. Each step attempt appends one
record with the step name, attempt number, status, refs, bounded metadata, error,
and timestamps. Retry leaves multiple records for the same step.

`StepRecords` should carry refs such as `OutputRef`, `AuditRef`, `AgentRunID`, and
small metadata. It should not store raw prompts, raw tool inputs, full model
messages, or full tool output.

## SQLite Store

Use `sqlitestore` when a host needs workflow state to survive process restarts:

```go
store, err := sqlitestore.Open("workflow.db")
if err != nil {
	panic(err)
}
defer store.Close()
```

The SQLite store implements the same `Store` contract as `MemoryStore`. It keeps
queryable scalar fields in columns and stores step history, metadata, attempts,
and completed steps as JSON fields. It still stores refs and bounded metadata,
not raw prompts or full tool payloads. The current SQLite schema is versioned as
`sqlitestore.SchemaVersion`.

## Queue Lease

Use `QueueStore` when a host needs to claim a pending workflow for background
execution. Use `QueueLeaseStore` when the host also needs to extend or release
the claimed lease:

```go
queue := store.(workflowkit.QueueLeaseStore)
run, err := queue.ClaimRunnable(ctx, "worker-1", 30*time.Second)
run, err = queue.ExtendLease(ctx, run.ID, "worker-1", 30*time.Second)
run, err = queue.ReleaseLease(ctx, run.ID, "worker-1")
```

`ClaimRunnable` selects the oldest pending workflow whose lease is empty or
expired, writes `LeaseOwner` and `LeaseUntil`, and returns the claimed run. It
does not run the workflow. `ExtendLease` refreshes only the current active owner;
an expired owner cannot revive its lease. `ReleaseLease` clears the current
owner's lease after execution stops at `waiting_approval` or terminal.

`QueueLeaseStore` still does not start worker goroutines, run heartbeat loops,
recover stuck workers, or define multi-worker scheduling policy. Those remain
host-owned execution concerns.

After claiming, hosts can pass the returned pending run to `Executor.Run`.

## Workflow Query

Use `WorkflowQueryStore` when a host needs an operational list view over stored
workflow runs:

```go
query := store.(workflowkit.WorkflowQueryStore)
runs, err := query.ListWorkflows(ctx, workflowkit.WorkflowQuery{
	Status:         workflowkit.StatusPending,
	MetadataEquals: map[string]string{"run_mode": "queued"},
	Order:          workflowkit.WorkflowOrderDesc,
	Limit:          50,
})
```

`ListWorkflows` returns copies ordered by `CreatedAt` then workflow id. `Status`
and `MetadataEquals` are optional filters. `Order` defaults to ascending and can
be set to `WorkflowOrderDesc`. `Limit` is optional and means no store-level limit
when zero.

## Store Conformance

Use `storetest` when adding a new `Store` implementation:

```go
func TestStoreConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) workflowkit.Store {
		return newStoreForTest(t)
	})
}
```

The suite verifies copy-on-read/write behavior, `Update`, and not-found errors so
store implementations keep the same semantics.

## Agent Step

Use the optional `github.com/eruca/goagents/workflowkit/agentstep` module when a workflow
step should run a `goagent` agent:

```go
step := agentstep.New("agent", agent, func(run workflowkit.WorkflowRun) agentcore.RunRequest {
	return agentcore.RunRequest{Input: run.InputRef}
})
```

The adapter preserves the agent run ID and maps aborts into a failed workflow
step. Host applications decide how to persist audit events and full artifacts.

## Verify

```bash
./scripts/verify-e2e.sh
```

The script runs core tests, race tests, basic workflow examples, SQLite resume,
the main-module dependency boundary check, the optional `agentstep` module, and
the nested `agent-approval` and `ocr-review` examples.

See `docs/contracts.md` for API and persistence contracts, and
`docs/release.md` and `docs/release-readiness.md` for release preparation.

## Examples

- `examples/basic` shows retry, approval wait, `Approve`, and resume.
- `examples/agent-approval` shows `agentstep` wrapping a mock `goagent` run, mapping the agent result to `waiting_approval`, recording an audit ref, and continuing to finalization.
- `examples/sqlite-resume` shows waiting approval, closing the process-owned store, reopening SQLite, updating audit refs, and continuing from persisted state.
- `examples/ocr-review` is a nested example module that shows a host-owned composition across `ocrs`, `contextkit`, `goagent`, and `workflowkit`. OCR output is stored behind artifact refs, `contextkit` builds a bounded model-facing projection, `agentstep` maps the review to `waiting_approval`, and the workflow resumes after the host records an audit ref.

## API Stability

The intended stable surface is:

- `WorkflowRun`, `Status`, `Step`, `StepResult`, and `StepRecord`
- `Executor` methods: `Run`, `Continue`, `Approve`, and `Cancel`
- `Store`, `WorkflowQueryStore`, `QueueStore`, `QueueLeaseStore`, `MemoryStore`, `RetryPolicy`, `TransientError`
- lifecycle errors: `ErrRunNotFound`, `ErrInvalidTransition`, and `InvalidTransitionError`
- queue errors: `ErrNoRunnableWorkflow` and `ErrWorkflowLeaseNotOwned`
- status errors: `ErrInvalidStatus` and `InvalidStatusError`

Extension packages are useful but still early:

- `agentstep` is a separately versioned optional module. It depends on `goagent`;
  the main `workflowkit` module does not.
- `sqlitestore` is suitable for local persistence and prototypes; schema
  compatibility should be treated as experimental until versioned migrations are
  introduced.
- `storetest` is intended for future store implementations.

`workflowkit/agentstep` requires tagged `goagent` and `workflowkit` versions and
contains no local replace. The root `go.work` supplies version-specific local
mappings before those tags exist; nested example modules keep relative replaces
because they are verification programs, not published libraries.
