# Run Stream Adapter Design

> Date: 2026-04-25
> Goal: Expose a small host-facing stream adapter for Agent runs without adding transport or storage responsibilities to `agentcore`.

## Context

`goagent` already has two pieces of observability:

- `EventSink` emits bounded runtime lifecycle events.
- `RunDetailed` returns an `ExecutionSummary` even when a run aborts.

Host applications still need to compose those pieces manually when they want a
CLI, UI, log tail, approval layer, or future SSE endpoint to observe a run as it
is happening. The next useful step is a library-level stream adapter that
combines runtime events and final completion into one ordered channel.

## Design Principles

1. Keep streaming in-process and transport-neutral.
2. Preserve existing `Run` and `RunDetailed` semantics.
3. Reuse `EventSink` instead of creating a second event mechanism.
4. Return final result or abort error as terminal stream data.
5. Keep event payloads bounded; do not stream raw prompts, raw tool inputs, or
   full tool outputs by default.

## API Shape

Add a host-facing stream API to `agentcore`:

```go
type RunStream struct {
	Events <-chan RunStreamEvent
	done   <-chan runStreamDone
}

type RunStreamEvent struct {
	Event  Event
	Result *RunResult
	Error  error
	Done   bool
}

func (a *Agent) Stream(ctx context.Context, req RunRequest) *RunStream
func (s *RunStream) Wait() (*RunResult, error)
```

The public shape can use unexported internals for completion state, but hosts
should be able to:

- range over `stream.Events`;
- receive runtime `Event` values as they happen;
- receive exactly one terminal event carrying `Done`, `Result`, and/or `Error`;
- call `stream.Wait()` after or during event consumption to get the final
  `RunDetailed` semantics.

Terminal behavior:

- success: terminal event has `Done == true`, `Result != nil`, `Error == nil`;
- abort: terminal event has `Done == true`, partial `Result`, `Error != nil`;
- stream channel closes after the terminal event is delivered.

## Runtime Flow

`Agent.Stream` should run the existing agent flow in a goroutine. It should
install a temporary `EventSink` that forwards bounded runtime events into the
stream channel while preserving any existing agent sink configured by the host.

Event fan-out should be best-effort:

1. emit to the stream;
2. emit to the configured sink if present;
3. ignore sink errors, matching existing `EventSink` behavior.

The actual run should use the same code path as `RunDetailed` so failures return
partial summaries consistently. The stream adapter must not duplicate the ReAct
loop or rebuild stages manually.

## Backpressure And Cancellation

The first implementation can use a small buffered channel. If the host never
consumes events, the run may block once the buffer fills. That is acceptable for
an in-process API because it makes backpressure explicit and avoids silently
dropping audit events.

Cancellation remains context-driven. If `ctx` is canceled and the LLM or tool
honors it, the terminal event carries the resulting error and partial summary.

## Error Handling

The stream adapter should not make observability failures fail the run. Existing
external sink errors remain ignored. Internal stream delivery should be ordered;
the implementation can block rather than drop.

`Wait` should be safe to call once. Multiple calls are not part of the first
contract unless a simple implementation makes it free. Document it as a single
consumer API for now.

## Testing Strategy

Add tests for:

- success path emits stage/tool/final events and terminal result;
- policy denial emits a terminal partial result and classifiable error;
- existing configured `EventSink` still receives events when streaming;
- `Run` and `RunDetailed` behavior remains unchanged.

Use mock LLMs and in-memory tools only. Do not add HTTP, SSE, or persistent
storage tests in this batch.

## Out Of Scope

- HTTP/SSE/WebSocket transport.
- Durable event or audit storage.
- OpenTelemetry exporters.
- Human approval, pause, or resume.
- MCP adapters.
- Multi-agent orchestration.

Those should be layered on top of this stream adapter or implemented as host
extensions after the in-process contract is stable.
