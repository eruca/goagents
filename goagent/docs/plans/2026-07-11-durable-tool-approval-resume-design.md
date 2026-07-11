# Durable Tool Approval Resume Design

> Date: 2026-07-11
> Goal: let a host stop an Agent before a tool executes, persist a versioned checkpoint, and resume the same tool-call batch after an explicit host decision.

## Context

`agentcore` currently has a synchronous `ToolApprover`: it allows or denies a
model-requested tool call after policy has accepted it and before `ActStage`
runs it. This covers immediate confirmation but cannot support a user leaving a
UI, an operator queue, or process replacement. A return value that means
"pending" must not execute the tool and must leave the host enough state to
continue without calling the model again.

## Scope

This is a narrow, Agent-core feature:

- add an explicit pending result to `ToolApprovalDecision`;
- return a classifiable interruption with a JSON-safe, versioned checkpoint;
- resume one pending tool-call batch after host resolutions;
- re-run policy immediately before the resumed execution;
- preserve the existing synchronous allow/deny behavior;
- expose only bounded lifecycle events; checkpoints, not events, hold raw tool
  input and conversation state.

It deliberately does not add a database, a queue, an HTTP endpoint, workflow
step checkpoints, generic graph persistence, multi-agent handoff, or automatic
checkpoint storage. The host owns storage, encryption, retention, identity
verification, and authorization of the operator who supplies a resolution.

## Public Contract

```go
var ErrApprovalPending = errors.New("approval pending")
var ErrInvalidRunCheckpoint = errors.New("invalid run checkpoint")
var ErrInvalidApprovalResolution = errors.New("invalid approval resolution")

type ToolApprovalDecision struct {
	Allowed bool
	Pending bool
	Reason  string
}

type ToolApprovalResolution struct {
	Index      int
	ToolCallID string
	Tool       string
	Allowed    bool
	Reason     string
}

type RunCheckpoint struct {
	Version      int
	RunID        string
	Request      CheckpointRequest
	Messages     []Message
	PendingCalls []ports.ToolCall
}

func (a *Agent) Resume(ctx context.Context, checkpoint RunCheckpoint, resolutions []ToolApprovalResolution) (*RunResult, error)
func (a *Agent) ResumeDetailed(ctx context.Context, checkpoint RunCheckpoint, resolutions []ToolApprovalResolution) (*RunResult, error)
```

`Pending` has precedence over `Allowed`. This fail-closed choice makes an
accidental decision containing both fields pause rather than execute. Existing
approvers that set only `Allowed` retain their current behavior.

The first pending decision interrupts the run. `Run` returns a nil result and
an error matching `ErrApprovalPending`. `RunDetailed` returns a partial result
and the same error. The partial result has `Interruption.Checkpoint`; it is the
only supported source for a later `Resume` call. A denial remains an error
matching `ErrApprovalDenied`.

## Checkpoint Boundary

`RunCheckpoint` is a versioned DTO, not a live `RunState`. It stores only the
data necessary to execute the already-modelled calls and then continue the
ReAct loop:

- checkpoint version and string Run ID;
- input, user, session, policy context, permissions, and metadata;
- iteration, messages, usage, accumulated LLM/tool summary, and start time;
- pending `ports.ToolCall` values and previously built prompt blocks.

It excludes the compiled prompt, context projection, last LLM response, tool
registry, LLM client, event sink, and execution results. Those are runtime
objects and are recreated. On restore, the initial stages are idempotent from
the captured metadata; the compiled prompt and context projection are rebuilt
for the next model call.

The checkpoint contains user content and raw tool inputs. It must never be
written to runtime-event metadata, telemetry, or ordinary audit logs. The host
must serialize it with `encoding/json`, store it in an access-controlled
location, encrypt it where its threat model requires it, and apply expiration.
The generic `map[string]any` metadata must contain JSON-compatible values.

## Approval And Resume Flow

```text
Think -> Budget -> Policy -> Approval
                            | allow  -> Act -> Observe -> next Think
                            | deny   -> ErrApprovalDenied
                            | pending-> checkpoint + ErrApprovalPending

checkpoint + complete host resolutions
    -> rehydrate dynamic tools -> re-run Policy -> verify resolutions
    -> Act -> Observe -> normal ReAct loop
```

All calls in a pending batch are resolved atomically. A resolution includes its
original index, ID, and tool name. Resume requires exactly one matching
resolution per call. If any resolution denies, no call in the batch executes.
If a checkpoint or resolutions do not match, resume fails closed before a tool
or a new model request runs.

Policy is deliberately run again immediately before resumed execution: policy,
permissions, tenant state, or a tool registry can change while an operator is
deciding. The original approver is not called on resume because the host
resolutions are the recorded decision.

Request-scoped tools are re-created by calling `ToolProvider.Tools` with the
captured request. Therefore such providers must return the same valid tools for
the same request while a checkpoint can be resumed. Static tools are cloned
from the Agent registry as in an ordinary run.

## Events And Results

Add `approval.pending`. Its metadata is limited to `tool`, `index`, and an
optional reason, like the existing approval events. It never includes tool
input, checkpoints, prompts, messages, or tool output. A resumed approval
emits `approval.completed` for accepted calls; a rejection emits
`approval.denied` and stops before `ActStage`.

`RunResult` gains an optional `Interruption *ToolApprovalInterruption`. Normal
successful and denied results leave it nil. This keeps the existing result
shape usable for hosts that do not use asynchronous approval.

## Invariants

1. No tool body executes before a synchronous allow or a validated resumed
   resolution.
2. No tool body executes when any call in the resumed batch is denied.
3. A resumed batch is never sent back to the LLM before its tool results have
   been observed.
4. A malformed or mismatched checkpoint/resolution fails before tool execution
   and before a new LLM call.
5. Runtime events remain bounded and contain no raw tool input or checkpoint
   payload.
6. A later model-requested approval can interrupt again and yields a new
   checkpoint.

## Non-goals And Follow-up Boundary

This design establishes a reliable core seam. `runkit` persistence, workflow
pause/resume, API/UI approval queues, durable audit adapters, cross-process
leases, trace propagation, guardrail pipelines, and multi-agent handoff stay
outside this change. Those layers can use the checkpoint contract without
making `agentcore` depend on their transports or stores.
