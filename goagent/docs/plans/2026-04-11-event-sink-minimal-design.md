# Event Sink Minimal Design

> Date: 2026-04-11
> Goal: Add lightweight observability hooks for embedded Agent runs without introducing an observability platform.

## Positioning

`EventSink` is an in-process observer for Agent runtime lifecycle events. It helps host applications debug, audit, and test runs without modifying core stages.

This is not OpenTelemetry, HTTP streaming, async logging, or persistent audit storage. It emits small structured events and leaves transport/storage decisions to host applications.

## API Shape

Keep event types in `agentcore` because they describe runtime state and use typed `RunID`.

```go
type EventType string

const (
    EventStageStarted   EventType = "stage.started"
    EventStageCompleted EventType = "stage.completed"
    EventStageFailed    EventType = "stage.failed"
    EventToolStarted    EventType = "tool.started"
    EventToolCompleted  EventType = "tool.completed"
    EventToolFailed     EventType = "tool.failed"
    EventMemoryLoaded   EventType = "memory.loaded"
    EventMemorySaved    EventType = "memory.saved"
    EventFinalized      EventType = "finalized"
)

type Event struct {
    RunID     RunID
    Type      EventType
    Stage     string
    Iteration int
    Message   string
    Metadata  map[string]any
}

type EventSink interface {
    Emit(ctx context.Context, event Event) error
}
```

Agent wiring:

```go
func WithEventSink(sink EventSink) Option
```

## Runtime Flow

The pipeline should emit stage lifecycle events:

- `stage.started` before stage execution
- `stage.completed` after a successful stage
- `stage.failed` when a stage returns an error

The runner and stages should emit high-value runtime events:

- memory load/save
- tool start/complete/fail
- finalization

## Error Handling

Sink errors should not fail the Agent run. Event sinks are observability side effects. Host applications that need strict audit delivery can implement their own fail-closed sink later, but the default core should not make logging outages break user runs.

## Privacy And Payload Discipline

Do not emit full prompts, full LLM responses, raw tool inputs, or user content by default. Events should carry identifiers and small metadata such as:

- stage name
- tool name
- message count
- iteration
- error string

## Out Of Scope

- OpenTelemetry exporter
- Async queues
- HTTP/SSE event streaming
- Persistent audit log
- Full prompt/LLM payload logging
- Sampling
- Metrics aggregation
