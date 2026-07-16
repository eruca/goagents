# WorkflowKit Host Orchestration Design

> Historical note: this was the initial implementation design. The current
> release contract is stricter: the core `github.com/eruca/goagents/workflowkit` module
> must not import `goagent`; the optional adapter lives in the separate
> `github.com/eruca/goagents/workflowkit/agentstep` module. See `../contracts.md`.

## Goal

`workflowkit` provides host-side workflow contracts for composing `goagent` runs with approval, audit, artifacts, retries, and external capabilities. It is not a new agent runtime and does not change `goagent/agentcore`.

## Boundary

`goagent` owns a single agent run: prompt assembly, ReAct loop, tool policy, tool approval, event streaming, and run summaries.

`workflowkit` owns the application-level lifecycle around one or more agent runs:

- workflow run identity and status
- ordered step execution
- waiting states such as manual approval
- artifact and audit references
- host-owned persistence contracts
- failure and cancellation state

The dependency direction is:

```text
application
  imports workflowkit
  imports goagent
  imports workflowkit/agentstep when needed
  imports capability modules such as ocrs/contextkit

workflowkit/agentstep
  may import goagent

goagent
  does not import workflowkit

workflowkit
  does not import goagent
```

## First Slice

The first slice should be deliberately small:

- define `WorkflowRun`
- define stable statuses: `pending`, `running`, `waiting_approval`, `succeeded`, `failed`, `cancelled`
- define `Step`, `StepResult`, and `Executor`
- provide an in-memory `Store` for tests and examples
- provide an optional `AgentStep` helper in a separate adapter module that wraps `Agent.RunDetailed`
- keep durable storage, distributed workers, DAGs, cron, UI, and multi-agent teams out of scope

## Data Model

`WorkflowRun` should contain:

- `ID`
- `Status`
- `InputRef`
- `OutputRef`
- `AgentRunID`
- `AuditRef`
- `Error`
- `ApprovalRef`
- `WaitingReason`
- `CurrentStep`
- `CompletedSteps`
- `Metadata`
- `CreatedAt`
- `UpdatedAt`

`StepResult` should contain:

- `Status`
- `OutputRef`
- `AgentRunID`
- `AuditRef`
- `Error`
- `ApprovalRef`
- `WaitingReason`
- `Metadata`

Steps should return a full status instead of throwing control-flow-specific sentinel values. Ordinary Go errors mean the step itself failed unexpectedly.

## Execution Semantics

The first executor is sequential:

1. create or load a workflow run
2. mark it `running`
3. execute each step in order
4. stop immediately if a step returns `waiting_approval`, `failed`, or `cancelled`
5. mark `succeeded` only after all steps complete successfully
6. persist status changes through `Store`

When a run is resumed with `Continue`, the executor loads the persisted run,
marks it `running`, skips `CompletedSteps`, and continues from the next step.

This is intentionally not a DAG engine. A host can build richer scheduling on top later.

## Agent Step

`AgentStep` should be an adapter, not a framework dependency hidden in the executor. It should take:

- a `goagent` agent-like interface
- a function that builds `agentcore.RunRequest` from the workflow run
- an optional function that maps `RunResult` to `StepResult`

On success it should set `AgentRunID`, `OutputRef` or metadata according to host mapping. On agent error it should return `failed` with the partial `AgentRunID` when available.

## Audit And Artifacts

`workflowkit` should not store raw prompts, raw tool input, full model messages, or full tool outputs by default. It should carry references:

- `InputRef`
- `OutputRef`
- `AgentRunID`
- `AuditRef`
- metadata allowlisted by host code

Durable audit writing remains a host adapter concern. A later package can define optional recorder interfaces if repeated examples reveal a stable shape.

## Testing

Use TDD for the first implementation:

- executor marks a run succeeded after ordered steps complete
- executor stops and persists `waiting_approval`
- executor marks failed on step error
- memory store copies records so callers cannot mutate persisted state accidentally
- agent step maps `RunDetailed` success and failure into workflow results
