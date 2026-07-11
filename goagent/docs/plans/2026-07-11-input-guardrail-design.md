# Input Guardrail Design

> Date: 2026-07-11
> Goal: reject unsafe or out-of-scope user input before it reaches memory, run context, the LLM, or tools.

## Context

`agentcore` already has two distinct guardrails:

- `OutputValidator` checks final model output before finalization and memory save.
- `PolicyStage` plus `ApprovalStage` decide whether a concrete tool call may execute.

The missing boundary is host-controlled inspection of the raw request before it
is copied into `RunState.Messages` or causes session memory to load. Adding a
generic phase registry would duplicate the existing output/tool contracts and
make the small ReAct core harder to reason about.

## Chosen Design

Add only an input guardrail:

```go
var ErrInputRejected = errors.New("input rejected")

type InputGuard interface {
	ValidateInput(context.Context, InputGuardRequest) error
}

type InputGuardRequest struct {
	RunID         RunID
	Input         string
	Metadata      map[string]any
	PolicyContext ports.PolicyContext
}

type InputGuardFunc func(context.Context, InputGuardRequest) error

func WithInputGuard(InputGuard) Option
```

`InputGuardStage` is the first default pipeline stage, before `MemoryLoadStage`
and `ContextStage`. When no guard is configured it does nothing. On a nil error
it records a private metadata flag so repeated ReAct iterations do not run it
again. On a non-nil error it returns the stable sentinel `ErrInputRejected`
without wrapping the host error, preventing a guard's diagnostic text from
leaking into `RunDetailed.ExecutionSummary.AbortReason` or events.

The existing output validator and tool policy/approval contracts stay unchanged:

```text
InputGuard -> Memory -> Context -> Prompt -> Think -> Budget -> Policy -> Approval -> Act
                                                                  ... -> OutputValidator
```

## Events And Safety

Add `input.validated` and `input.rejected` events. Both are intentionally empty
events: no input, host error, metadata, or prompt is copied into event payloads.
The host may record rich reasons in its own secure audit boundary if it needs
them.

Input guardrails are request screening, not authorization. They do not replace
policy, approval, output validation, authentication, rate limiting, or model
provider safety controls.

## Resume Behavior

Successful input validation is checkpointed through the existing internal
metadata flag. `ResumeDetailed` does not execute it a second time because the
checkpoint preserves the identical request. Resumed tool execution still
re-runs `PolicyStage`, which is the time-sensitive authority boundary.

## Verification

Tests prove that rejection happens before memory load and LLM use; detailed
errors and events contain no guard diagnostic; acceptance runs once across a
tool iteration; and input validation is not duplicated after a durable approval
resume. Existing output-validator and policy tests remain the regression proof
for the other two guardrail layers.

## Out Of Scope

- configurable guardrail graph or plug-in registry;
- model-based guardrail evaluation;
- automatic retries, sanitization, or rewriting of rejected input;
- host audit persistence, rate limits, authentication, and moderation APIs;
- replacing `OutputValidator`, Policy, or tool approval.
