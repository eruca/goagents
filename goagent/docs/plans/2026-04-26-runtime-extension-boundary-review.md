# Runtime Extension Boundary Review

> Date: 2026-04-26
> Goal: Re-state the stable runtime boundary after observable runs, streaming,
> policy context, tool safety, and tool approval, then choose the next extension
> proof without expanding `agentcore` into a workflow engine.

## Current Position

`goagent` is now a small ReAct runtime core with host-facing observability and
execution gates. The project has enough runtime surface for applications to
wire real capabilities without putting provider, transport, storage, or domain
workflow logic into `agentcore`.

The important shift is that the next useful work should prove extension
composition around the core, not add another orchestration concept to the core
itself.

## Stable Core Boundary

The stable core contract is:

- `Agent.Run` for normal execution with the historical `nil, err` abort
  behavior.
- `Agent.RunDetailed` for host-facing audit summaries, including partial
  `RunResult` values on abort.
- `Agent.Stream` for in-process ordered runtime updates and one terminal result
  or abort.
- `RunRequest` and `RunResult` as the main application contract.
- `ExecutionSummary` as the bounded run summary for LLM calls, completed tool
  calls, used tools, duration, and abort reason.
- `WithEventSink` for bounded lifecycle events.
- `WithContextProjector` for model-facing message reshaping before `ThinkStage`.
- `PolicyStage` as the hard permission and safety boundary before tool
  execution.
- `ApprovalStage` as a host-controlled approval point for concrete
  policy-eligible tool calls.

This is enough for host applications to build CLIs, UIs, service endpoints,
approval flows, and audit logs around the runtime without the core owning those
surfaces.

## Capability Boundary

Tools remain the main capability boundary.

The model may request a tool call, but host code owns:

- input validation;
- dependency access;
- authorization and policy;
- optional approval;
- side effects;
- result shaping;
- artifact storage;
- follow-up bounded read tools.

Large capability outputs should still use the existing split:

- `ForLLM` for bounded model-visible observation;
- `ForUser` for user-visible full payload or structured data;
- `Ref` for host-owned artifact references;
- `Metadata` for small facts.

This keeps OCR, retrieval, database queries, file parsing, and domain actions
outside the runtime while still making them available to the agent.

## What Must Stay Outside Core

The following concerns should remain host or extension responsibilities:

- HTTP, SSE, WebSocket, and RPC transports.
- Durable run, event, audit, approval, and artifact storage.
- OpenTelemetry exporters and vendor-specific observability clients.
- MCP servers and client adapters.
- Human approval UI.
- Async pending state, pause, resume, and recovery after process restart.
- Model-generated plan approval.
- Multi-agent orchestration.
- Domain workflows such as research pipelines, OCR pipelines, clinical review,
  coding assistants, or business automations.

These features may be valuable, but they should be composed around `Agent`,
`RunDetailed`, `Stream`, tools, policy, approval, and memory rather than added
as first-class runtime responsibilities.

## Boundary Clarifications

### Policy Versus Approval

`PolicyStage` decides whether a requested action is allowed by framework and
host rules. It is a hard pre-execution safety gate.

`ApprovalStage` is later and narrower. It lets a host approve or deny concrete
tool calls that already passed policy. It is not a plan approval system, not a
human UI, and not a durable pause point.

### Stream Versus Transport

`Agent.Stream` is an in-process adapter. It combines bounded runtime events and
the terminal `RunDetailed` result into one ordered channel.

It is not an SSE server, WebSocket protocol, durable event log, trace exporter,
or UI state machine. Those can wrap the stream in host code.

### RunDetailed Versus Durable Audit

`RunDetailed` gives the host a bounded summary and partial result when a run
aborts. It is enough to make aborts inspectable.

It is not durable storage. Hosts that need retention, replay, compliance logs,
or searchable traces should persist `RunDetailed` results and stream events in
their own storage layer.

### Context Projection Versus Memory

`WithContextProjector` changes the model-facing view before LLM calls. It does
not mutate canonical run messages, replace memory, or own summarization storage.

Deep compression, reversible collapse, semantic retrieval, and durable memory
belong in sibling modules or host code.

## Next Extension Proofs

There are three plausible next steps.

### Option A: SSE Stream Adapter Example

Build an example host service that wraps `Agent.Stream` and exposes Server-Sent
Events.

This should live outside `agentcore`, probably as an example, and should avoid
adding a transport package to the core. It would validate that stream events and
terminal partial results are enough for a real service boundary.

This is the recommended next implementation.

### Option B: Durable Audit Adapter Design

Write a design for persisting bounded runtime events, terminal run summaries,
and host-owned artifact refs.

This should define storage shape and privacy constraints, but should not add a
database dependency to `goagent`.

This is useful after the SSE proof, or before it if audit persistence becomes
the next product need.

### Option C: Plan Approval Design Review

Write a design-only document for model-generated plan approval.

This should not be implemented yet. Plan approval introduces a different
contract from tool-call approval because it needs plan structure, editing,
approval state, and possible pause/resume semantics. It is likely a host
workflow layer first, not an `agentcore` stage.

This is useful only after the current tool-call approval boundary has been
validated by at least one host adapter.

## Recommendation

Do Option A next: create an SSE stream adapter example outside core.

The implementation should prove:

- `Agent.Stream` can drive a service endpoint without runtime changes;
- approval lifecycle events are sufficient for a UI or service log;
- terminal partial results are usable on approval, policy, budget, or tool
  aborts;
- event payloads stay bounded and do not leak raw prompts, raw tool inputs, or
  full tool outputs;
- backpressure and cancellation behavior are understandable at the host layer.

If the SSE example exposes a missing primitive, add the smallest possible core
contract change after the proof. Do not preemptively add transport abstractions
to `agentcore`.

## Proposed Follow-Up Plan

1. Add `docs/plans/2026-04-26-sse-stream-adapter-example-design.md`.
2. Implement `examples/stream-sse` as a host-owned adapter around
   `Agent.Stream`.
3. Keep all HTTP/SSE code in the example.
4. Add focused tests only if the adapter has reusable logic worth testing.
5. Update README learning path with the new example.
6. Run `make verify`.

## Non-Goals For The Next Implementation

- No changes to `Run`, `RunDetailed`, or `Stream` unless the example proves a
  concrete gap.
- No durable storage.
- No approval UI.
- No pause/resume.
- No plan approval.
- No generic transport framework.
- No MCP adapter.

## Review Checklist

Before adding more core surface, require a concrete answer to these questions:

- Does the runtime need to understand this concept to execute a single run
  correctly?
- Can the feature be expressed as a tool, provider, memory implementation,
  context projector, event sink, approver, or wrapper around `Agent.Stream`?
- Would adding it to `agentcore` force provider, transport, storage, or domain
  dependencies into the core?
- Can an example or host adapter prove the need first?
- Does the change keep model-visible data bounded by default?

If the answer points to host composition, keep the feature outside core.
