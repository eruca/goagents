# Tool Approval Gate Design

> Date: 2026-04-25
> Goal: Add a minimal host-controlled approval gate before tool execution without turning `agentcore` into a workflow engine.

## Context

`goagent` already separates model intent from execution:

- `ThinkStage` receives model tool calls.
- `BudgetStage` checks usage limits.
- `PolicyStage` enforces framework and host policy before tools run.
- `ActStage` executes approved tool calls.
- `Agent.Stream` can expose runtime events and terminal summaries to hosts.

The missing generic affordance is a host-controlled approval point after policy
has accepted a tool call but before the tool body runs. This is useful for UI
confirmation, operator review, and business-specific action gates.

## Positioning

This is tool-call approval, not plan approval.

The first version should approve or deny concrete model-requested tool calls. It
should not require a model-generated plan, durable run state, pause/resume, HTTP
transport, or a UI framework. Those can be layered later once the smaller
contract is stable.

## Design Principles

1. Keep policy and approval distinct.
2. Preserve existing behavior when no approver is configured.
3. Approve concrete tool calls, not free-form model plans.
4. Emit bounded approval lifecycle events for `Agent.Stream`.
5. Fail closed on approval denial and never execute denied tools.

## API Shape

Add host-facing types in `agentcore`:

```go
type ToolApprover interface {
	ApproveTool(ctx context.Context, req ToolApprovalRequest) ToolApprovalDecision
}

type ToolApprovalRequest struct {
	RunID     RunID
	UserID    string
	SessionID string
	Tool      string
	Input     json.RawMessage
	Metadata  map[string]any
}

type ToolApprovalDecision struct {
	Allowed bool
	Reason  string
}

func WithToolApprover(approver ToolApprover) Option
```

Add a classifiable framework error:

```go
var ErrApprovalDenied = errors.New("approval denied")
```

## Runtime Flow

Insert a new stage between `PolicyStage` and `ActStage`:

```text
Think -> Budget -> Policy -> Approval -> Act
```

Stage semantics:

- if there are no pending tool calls, continue;
- if no approver is configured, continue;
- for each pending call, build a `ToolApprovalRequest`;
- if the approver allows the call, continue to the next call;
- if the approver denies a call, abort with `ErrApprovalDenied`;
- denied tools must not reach `ActStage`.

`PolicyStage` remains the hard safety and permission boundary. `ApprovalStage`
is a host-controlled decision point for already policy-eligible tool calls.

## Events

Add three bounded runtime event types:

```go
EventApprovalRequested EventType = "approval.requested"
EventApprovalCompleted EventType = "approval.completed"
EventApprovalDenied    EventType = "approval.denied"
```

Event metadata should include only small facts:

- `tool`
- `index`
- `reason` for denied/completed when present

Do not emit raw tool input, prompts, full user content, or tool output.

These events automatically flow through `WithEventSink` and `Agent.Stream`
because they reuse the existing event path.

## Error Handling

Approval denial is an abort, not a recoverable tool error. `Run` returns a nil
result and an error matching `ErrApprovalDenied`. `RunDetailed` and
`Agent.Stream` return a partial `RunResult` with accumulated usage and
`ExecutionSummary.AbortReason`.

Approver errors are intentionally out of scope for the first version. If a host
needs fail-closed behavior for internal approval failures, it should return
`Allowed: false` with a reason. A later version can add an error-returning
interface if real implementations need that distinction.

## Testing Strategy

Add focused contract tests:

- no approver configured: existing read-tool flow still succeeds;
- approver allows: tool executes and final result is produced;
- approver denies: tool does not execute, error matches `ErrApprovalDenied`;
- `RunDetailed` returns a partial summary on denial;
- `Agent.Stream` includes approval events and a terminal partial result.

Run `go test ./agentcore -run Approval -count=1` for focused work and
`make verify` before completion.

## Out Of Scope

- model-generated execution plans;
- human approval UI;
- asynchronous pending state;
- pause/resume;
- durable approval records;
- HTTP/SSE/WebSocket transport;
- MCP adapters.

Those should be built as host or extension layers after this synchronous
approval contract is stable.
