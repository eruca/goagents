# WorkflowKit Contracts

This document defines the contracts that should remain stable before adding more
workflow features.

## Module Boundary

`github.com/eruca/goagents/workflowkit` is the core host-side orchestration module. It
must not import `github.com/eruca/goagents/goagent` or
`github.com/eruca/goagents/workflowkit/agentstep`.

`github.com/eruca/goagents/workflowkit/agentstep` is an optional adapter module. It may
import both `workflowkit` and `goagent`.

Host applications compose modules at the boundary:

```text
application
  imports workflowkit
  optionally imports workflowkit/agentstep
  optionally imports goagent
  optionally imports capability modules
```

## Workflow Status

Valid statuses are:

- `pending`
- `running`
- `waiting_approval`
- `succeeded`
- `failed`
- `cancelled`

`pending` is only an initial workflow status. `succeeded`, `failed`, and
`cancelled` are terminal.

Allowed lifecycle transitions:

```text
pending -> running
running -> waiting_approval
running -> succeeded
running -> failed
running -> cancelled
waiting_approval -> running
waiting_approval -> cancelled
```

Invalid lifecycle transitions return `InvalidTransitionError` and match
`ErrInvalidTransition` with `errors.Is`.

## Step Result Status

A step may return:

- empty status: treat the step as succeeded and continue
- `running`: mark the step completed and continue
- `waiting_approval`: persist the run and stop until `Continue` or `Approve`
- `succeeded`: mark the step completed and stop as a succeeded workflow
- `failed`: stop as a failed workflow
- `cancelled`: stop as a cancelled workflow

A step must not return `pending` or an unknown status. Invalid step statuses
return `InvalidStatusError`, match `ErrInvalidStatus`, mark the workflow failed,
and record the attempt as failed.

## Completed Steps

`CompletedSteps` is a resume cursor, not a human-readable audit log.

Steps that return empty status, `running`, `waiting_approval`, or `succeeded`
are added to `CompletedSteps`.

Steps that return `failed` or `cancelled` are not added to `CompletedSteps`.
This prevents failed or cancelled work from being treated as already completed
if the host later inspects or repairs the run.

## Step Records

`StepRecords` is the observability trail. Each attempt appends one record with:

- step name
- attempt number
- final attempt status
- refs such as `OutputRef`, `AuditRef`, `AgentRunID`, and `ApprovalRef`
- bounded metadata
- error text
- start and end timestamps

Retries create multiple records for the same step. Invalid step result statuses
are recorded as failed attempts.

`StepRecords` should not store raw prompts, raw tool inputs, full model messages,
full OCR payloads, full tool output, or large artifacts. Store those in a
host-owned artifact/audit system and put refs in the workflow run.

## Store

`Store` implementations must provide:

- `Save`
- `Get`
- `Update`

Stores must preserve copy-on-read and copy-on-write semantics for slices, maps,
step attempts, step records, and metadata. Mutating a returned `WorkflowRun`
must not mutate persisted state unless `Save` or `Update` is called.

Missing runs return `ErrRunNotFound`.

Use `storetest.RunStoreConformance` for new store implementations.

## Workflow Query Store

`WorkflowQueryStore` is an optional store extension for host-owned operational
views. `ListWorkflows` accepts optional `Status` and `MetadataEquals` filters,
optional `Order` (`asc` by default or `desc`), and optional `Limit`. Results are
ordered by `CreatedAt` and then workflow id, with both fields reversed for
descending order. Returned workflows must preserve the same copy semantics as
`Get`.

Use `storetest.RunWorkflowQueryStoreConformance` for implementations that expose
workflow listing.

## SQLite Store

`sqlitestore` persists workflow state in SQLite and currently uses
`SchemaVersion = 4`.

The schema version is recorded in `workflowkit_schema` with id `sqlitestore`.
`Open` upgrades older databases in place: version 3 stored times as integers
instead of zone-qualified text, and version 4 moved step records from the
`step_records_json` column into `workflow_step_records`, one row per record, so
a save writes only the records that changed. Future schema changes must bump
`SchemaVersion`, add migration tests, and document whether old databases are
upgraded in place or require host rebuild.

## Retry

Retries are opt-in. `RetryPolicy.MaxAttempts <= 0` means one attempt.

Only errors marked with `TransientError` are retried. Ordinary Go errors fail
the workflow immediately. `StepRetries` carries the retry budget across claims.
A run claimed from a queue (it has a lease owner) does not sleep through a
backoff: it returns to `pending` with `NextAttemptAt` set, and `ClaimRunnable`
hands it out again once the retry is due. Unleased runs wait in-process.
