# Durable Audit Adapter Design

> Date: 2026-04-26
> Goal: Define a host-owned durable audit shape for `goagent` runs without
> adding storage, database dependencies, or replay semantics to `agentcore`.

## Context

`goagent` now exposes enough bounded runtime facts for host applications to
persist run audits:

- `WithEventSink` receives bounded lifecycle events during a run.
- `Agent.Stream` exposes the same runtime events plus one terminal result.
- `Agent.RunDetailed` returns a `RunResult` with `ExecutionSummary`, including
  partial summaries when a run aborts.
- tool results already separate model-visible observations, user-visible
  payloads, artifact refs, and metadata.

The missing piece is not a core API. It is a clear host adapter pattern for
storing these facts durably while keeping raw prompts, raw tool inputs, full tool
outputs, and compliance policy outside the runtime.

## Positioning

Durable audit is a host adapter, not an `agentcore` feature.

The core should keep emitting bounded events and terminal summaries. The host
decides:

- where audit records are stored;
- which request/user/session fields are retained;
- which metadata keys are allowed;
- how artifact refs map to object storage;
- how long records are retained;
- who can query or export audit records.

## Design Principles

1. Persist bounded facts by default.
2. Keep raw model context and raw tool input out of the default audit trail.
3. Treat full payload storage as an explicit host decision tied to artifact
   refs.
4. Make terminal run records authoritative for final status.
5. Do not require a database, queue, or tracing vendor in `goagent`.
6. Do not define replay, pause/resume, or recovery semantics in this layer.

## Recommended Storage Shape

A host can use any storage backend. The logical shape should be three durable
record families.

### `audit_runs`

One row or document per run.

Recommended fields:

- `run_id`
- `user_id`
- `session_id`
- `status`: `running`, `succeeded`, or `aborted`
- `started_at`
- `completed_at`
- `input_tokens`
- `output_tokens`
- `llm_calls`
- `tool_calls`
- `used_tools`
- `duration_ms`
- `abort_reason`
- `request_metadata`: allowlisted request metadata only
- `policy_context_ref`: optional host-owned reference, not raw policy context by
  default
- `terminal_content_preview`: optional bounded final answer preview
- `terminal_content_ref`: optional host-owned reference to a full final answer

`audit_runs` is the place hosts query first. It should be enough to answer:

- did the run finish?
- why did it abort?
- which tools completed?
- how expensive was it?
- where can the host find richer artifacts if retention policy allows it?

### `audit_events`

One row or document per runtime event.

Recommended fields:

- `run_id`
- `sequence`
- `event_type`
- `stage`
- `iteration`
- `message`
- `metadata`
- `recorded_at`

`sequence` is host-assigned when persisting events. `agentcore.Event` does not
currently expose a timestamp or sequence, so adapters should assign both at the
storage boundary.

`metadata` must remain bounded. Recommended allowed keys include:

- `tool`
- `ref`
- `index`
- `reason`
- `count`
- `permission`

Do not store raw prompts, raw tool inputs, raw user content, or full tool output
in event metadata unless the host has an explicit redaction and retention
policy.

### `audit_artifacts`

One row or document per host-owned artifact ref.

Recommended fields:

- `ref`
- `run_id`
- `kind`
- `mime_type`
- `preview`
- `storage_uri`
- `metadata`
- `created_at`
- `retention_policy`

This table is optional. If the host already has an artifact store, audit records
can store only refs.

Artifact records are where full OCR output, retrieval result sets, generated
files, or user-visible tool payloads belong. They should not be embedded into
`audit_events`.

## Adapter Wiring

There are two valid host wiring patterns.

### Pattern A: EventSink Plus RunDetailed

Use this when a service executes runs synchronously and does not need a live
transport stream:

```go
audit := NewAuditRecorder(store)
agent, err := agentcore.NewAgent(
	agentcore.WithLLM(llm),
	agentcore.WithToolRegistry(registry),
	agentcore.WithEventSink(audit),
)
if err != nil {
	return err
}

result, err := agent.RunDetailed(ctx, req)
audit.RecordTerminal(ctx, req, result, err)
```

The `EventSink` writes `audit_events`. `RecordTerminal` upserts `audit_runs`
with final status, usage, summary, and bounded final content preview.

### Pattern B: Stream Wrapper

Use this when a host already consumes `Agent.Stream` for CLI, SSE, WebSocket, or
UI updates:

```go
stream := agent.Stream(ctx, req)
for event := range stream.Events {
	if event.Done {
		audit.RecordTerminal(ctx, req, event.Result, event.Error)
		continue
	}
	audit.RecordEvent(ctx, event.Event)
}
result, err := stream.Wait()
```

This keeps live transport and durable audit as sibling adapters around the same
runtime stream.

## Terminal Status Mapping

Terminal persistence should use the run result and error together:

- `err == nil`: `status = succeeded`
- `err != nil`: `status = aborted`
- `result == nil`: store the error, but keep summary fields empty
- `result != nil`: store `RunID`, usage, execution summary, and bounded content
  preview

`RunDetailed` and `Stream` should be preferred for audited runs because they
return partial `RunResult` values on abort.

## Privacy And Redaction

The durable adapter must assume that audit records can outlive the request.

Default policy:

- Store IDs, event types, stage names, tool names, refs, small counts, and
  summary numbers.
- Store request metadata only through an allowlist.
- Store final content only as a bounded preview unless the host explicitly wants
  full answer retention.
- Store full tool outputs only in artifact storage behind refs.
- Do not store raw `RunRequest.Input` by default.
- Do not store raw `ToolCall.Input` by default.
- Do not store model messages or compiled prompts by default.

If a host needs full transcript retention, implement it as a separate,
explicitly named host policy such as `StoreFullTranscript`, not as the default
durable audit adapter.

## Failure Handling

Audit failure should be a host policy choice:

- best-effort mode: audit write failures do not fail the agent run;
- strict mode: audit write failures cancel or abort the host request before or
  after the run.

`agentcore.EventSink` errors are currently ignored by the runtime. Therefore a
strict durable audit adapter cannot rely only on `WithEventSink`. It should wrap
`Agent.Stream` or call explicit `Record*` methods in host code so persistence
errors can be surfaced.

## Queries The Shape Should Support

The recommended shape should support:

- list recent runs by user or session;
- show one run timeline;
- find aborted runs by `abort_reason`;
- count tool usage by tool name;
- correlate final answers to artifact refs;
- inspect approval decisions without raw tool input;
- estimate token usage by user, session, or time window.

## Non-Goals

- No database package in `goagent`.
- No SQL migrations in core.
- No replay.
- No pause/resume.
- No async recovery.
- No full transcript retention by default.
- No OpenTelemetry exporter.
- No compliance policy baked into `agentcore`.

## Follow-Up Options

1. Add a documentation-only example that shows an in-memory `AuditRecorder`
   wrapping `Agent.Stream`.
2. Add a runnable `examples/audit-log` that writes JSONL records to stdout or a
   temp file.
3. If repeated host implementations need it, add a small helper package outside
   `agentcore`, but keep database-specific code out of the runtime.

The recommended next step is Option 2: a JSONL `examples/audit-log` adapter. It
would prove the storage shape without selecting a database.
