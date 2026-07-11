# Go Agent Framework

This framework is a small Go library for building ReAct-style agents with typed extension points. The first slice includes a deterministic core loop, prompt compilation, policy-aware tools, bounded window memory, and test doubles for LLM-driven flows.

## Quickstart

Run the full local verification suite:

```bash
make verify
```

Run the basic example:

```bash
go run ./examples/basic
```

## Learning Path

- `examples/basic` shows the smallest ReAct run with one read-only tool.
- `examples/artifacts` shows composed tools using artifact refs and bounded read-back.
- `examples/context-projection` shows the `WithContextProjector` adapter shape.
- `examples/skills` shows model-facing instructions next to executable tools.
- `examples/module` shows host-side module wiring for prompts, skills, and tools.
- `examples/prompt` shows compact prompt assembly order.
- `examples/openai-compatible` shows a real-provider run against an OpenAI-compatible service.
- `examples/events` shows runtime observability with `WithEventSink`.
- `examples/stream` shows in-process lifecycle updates with `Agent.Stream`.
- `examples/stream-sse` shows a host-owned SSE adapter around `Agent.Stream`.
- `examples/audit-log` shows a host-owned JSONL audit adapter around `Agent.Stream`.
- `examples/approval` shows host-controlled tool approval with stream events.
- `examples/approval-deny` shows approval denial with partial stream summaries.
- `examples/approval-resume` shows JSON checkpoint persistence and host-side Agent reconstruction before approved tool execution.
- `examples/structured-output` shows JSON Schema output validation success and failure.

## What The Core Owns

The `agentcore` package owns request/result types, run state, the stage pipeline, and the ReAct execution loop. It depends on ports and concrete default packages for prompt compilation, tools, and policy checks.

The `ports` package owns cross-boundary DTOs so implementation packages can depend on port contracts without creating reverse dependencies into the core or each other.

The core does not include real LLM providers, PostgreSQL, HTTP/SSE transport, vector memory, or multi-agent orchestration.

The broader `goagents` workspace can contain sibling capability modules such as OCR, context compression, retrieval, or durable memory. Keep those modules outside `agentcore`; applications wire them into this framework through tools, providers, memory implementations, or future runtime extension ports. See `docs/extensions.md` for the dependency direction and module boundary rules.

## Core API Stability

The stable host-facing API is the surface applications should build around:

- `agentcore.NewAgent`, `Agent.Run`, `Agent.RunDetailed`, `Agent.Resume`, `Agent.ResumeDetailed`, `RunRequest`, `RunResult`, and `RunID`
- `agentcore.Option` values such as `WithLLM`, `WithToolRegistry`, `WithInputGuard`, `WithPolicyEngine`, `WithMemoryProvider`, `WithContextProjector`, `WithBudget`, and `WithEventSink`
- `agentcore.WithOutputFormat` and `WithOutputValidator` for final-output format instructions and validation
- `agentcore.Event`, `EventSink`, `Skill`, `SkillProvider`, `SystemPromptProvider`, `ToolProvider`, and `Module`
- `ports` interfaces and DTOs for LLM, prompt, tools, policy, and memory
- concrete helper packages such as `prompt`, `tools`, `policy`, and `memory`

The advanced runtime API remains exported for tests and specialized composition. Prefer `Agent` unless you are replacing the runtime loop. This includes `RunState`, `Stage`, `Pipeline`, individual stage structs, `ReActRunner`, `ReActConfig`, and `MutableToolRegistry`.

Common framework-owned abort reasons are classifiable with `errors.Is`:

- `agentcore.ErrMissingLLM`
- `agentcore.ErrMaxIterations`
- `agentcore.ErrBudgetExceeded`
- `agentcore.ErrPolicyDenied`
- `agentcore.ErrInputRejected`
- `agentcore.ErrApprovalDenied`
- `agentcore.ErrApprovalPending`
- `agentcore.ErrInvalidRunCheckpoint`
- `agentcore.ErrInvalidApprovalResolution`
- `tools.ErrToolNotFound`
- `tools.ErrToolInputInvalid`
- `tools.ErrToolSchemaInvalid`
- `tools.ErrToolExecutionFailed`
- `tools.ErrToolTimeout`

Provider-specific failures remain host-owned errors.

## Implementing A Module

Business modules plug into the framework by implementing ports:

- `ports.LLMClient` for model calls.
- `ports.PromptCompiler` for prompt block compilation.
- `ports.ToolRegistry` plus `tools.Tool` for domain tools.
- `ports.PolicyEngine` for permission decisions.
- `ports.MemoryProvider` for conversation memory.

Use `agentcore.NewAgent` with options such as `WithLLM`, `WithPromptBlocks`, `WithSkillProvider`, `WithToolRegistry`, `WithPolicyEngine`, `WithMemoryProvider`, `WithBudget`, and `WithMaxIterations` to wire a module without modifying the core.

Use `WithModule` when a business module provides system prompts, skills, and request-scoped tools together.

Use `WithEventSink` when a host application needs to observe run lifecycle events for logs, traces, metrics, or UI status. The core emits stage lifecycle events, tool events, memory events, and finalization events. Event sink errors are ignored so observability failures do not fail agent runs.

Use `WithContextProjector` when a host application needs to compress or reshape the model-facing message view before each LLM call. The projector receives the full run messages and returns a projection for `ThinkStage`; it does not mutate the original run state. Concrete compression algorithms should live outside `agentcore`, for example in a sibling module such as `contextkit`.

Pass `RunRequest.RunID` when the caller already has a trace ID; otherwise `Agent.Run` generates a typed `RunID` backed by `github.com/google/uuid` and returns it on `RunResult.RunID`.

When a memory provider is configured and `RunRequest.SessionID` is set, the runner loads session memory before the current input and saves the final message history after a successful run.

Skills are model-facing instructions. Tools are executable actions. Policy decides whether a requested tool action is allowed.

## Provider Extensions

The core depends only on `ports.LLMClient`. Real model clients live outside `agentcore`.

`extensions/providers/openaiapi` implements `ports.LLMClient` for OpenAI-compatible Chat Completions services. It maps framework messages, tool specs, tool call IDs, and tool outputs to the compatible request/response shape while keeping provider-specific code out of the core.

See `examples/openai-compatible` for a real-provider example. The example requires `OPENAI_COMPAT_BASE_URL` and `OPENAI_COMPAT_MODEL`; `OPENAI_COMPAT_API_KEY` is optional. Without the required values it prints a skip message and exits successfully.

## Observable Runs

Use `Agent.Run` for normal execution. Use `Agent.RunDetailed` when the host needs an auditable run summary even if the run aborts.

`RunResult` includes the final content, accumulated provider usage, and an `ExecutionSummary`:

- `LLMCalls`: successful model responses received during the run.
- `ToolCalls`: tool executions that completed.
- `UsedTools`: unique completed tool names in first-use order.
- `Duration`: wall-clock run duration measured by the framework.
- `AbortReason`: empty on success, or the framework/provider error message on abort.

On success, `Run` and `RunDetailed` both return the final `RunResult`. On error, `Run` returns a nil result, while `RunDetailed` returns a partial `RunResult` with `RunID`, accumulated `Usage`, and `ExecutionSummary`. A pending tool approval is a classifiable interruption: `RunDetailed` also returns `RunResult.Interruption.Checkpoint`, while `Run` returns a nil result and `ErrApprovalPending`.

```go
result, err := agent.RunDetailed(ctx, agentcore.RunRequest{
	Input: "look up the account and summarize it",
})
if err != nil {
	log.Printf("run aborted: %s", result.ExecutionSummary.AbortReason)
}
log.Printf("llm=%d tools=%d used=%v tokens=%+v",
	result.ExecutionSummary.LLMCalls,
	result.ExecutionSummary.ToolCalls,
	result.ExecutionSummary.UsedTools,
	result.Usage,
)
```

## Run Streams

Use `Agent.Stream` when a host needs in-process lifecycle updates while a run is executing. The stream emits bounded runtime events and then one terminal event with the final `RunResult` or abort error.

```go
stream := agent.Stream(ctx, agentcore.RunRequest{Input: "look up the account"})
for event := range stream.Events {
	if event.Done {
		break
	}
	log.Printf("%s %s", event.Event.Type, event.Event.Stage)
}
result, err := stream.Wait()
```

`Stream` uses `RunDetailed` semantics for terminal results. On abort, `Wait` returns the partial result and error. Existing sinks configured with `WithEventSink` still receive runtime events.

The stream is in-process and transport-neutral; HTTP, SSE, WebSocket, and durable audit storage belong in the host application.

## Prompt Assembly

Prompt blocks are model-facing instructions. They are not tools, memory, policy, or orchestration.

- `ModeCacheable` blocks sort before `ModeDynamic` blocks.
- Lower `Priority` sorts earlier.
- Blocks with the same mode and priority sort by `Name`.
- Empty block content is omitted from compiled text.
- Non-empty block content is joined with newlines.
- The compiled prompt is sent as the first `system` message when non-empty.
- Memory messages load before current user input.
- Tool observations are appended before the next LLM turn unless the tool result is `Silent`.

Do not put secrets or raw sensitive data into prompt blocks unless the host intentionally wants that data in model context.

See `examples/prompt` for a compact prompt assembly example.

## Guardrails And Policy

`agentcore` keeps three guardrail boundaries separate:

- `WithInputGuard` screens a raw request before memory load, context assembly, the LLM, and tools.
- `WithOutputValidator` validates the final model answer before it is finalized or saved to memory.
- Policy and approval decide whether a concrete tool call can execute.

An input guard implements `InputGuard`. Return a non-nil error to reject the request; the framework returns `ErrInputRejected` without embedding that diagnostic in events or `ExecutionSummary.AbortReason`. Hosts that need a detailed reason should retain it in their own access-controlled audit path.

```go
agent, err := agentcore.NewAgent(
	agentcore.WithLLM(llm),
	agentcore.WithInputGuard(agentcore.InputGuardFunc(func(ctx context.Context, req agentcore.InputGuardRequest) error {
		if strings.Contains(req.Input, "restricted source") {
			return errors.New("source is not allowed")
		}
		return nil
	})),
)
```

`input.validated` and `input.rejected` events intentionally contain no raw input, guard diagnostic, or request metadata. Successful validation runs once for the request, including when a paused approval checkpoint is later resumed.

## Policy

Policy is the host-side safety gate between model-requested tool calls and tool execution.

- The model can request a tool call, but policy must allow it before `ActStage` runs the tool.
- The default policy allows explicit `read` tools.
- The default policy denies `write`, `exec`, empty, and unknown permissions.
- `RunRequest.AllowedPermissions` can allow mutating actions for a specific run.
- Policy engines receive run ID, user ID, session ID, tool input, metadata, and `RunRequest.PolicyContext`.
- A policy denial aborts the run before tool execution and memory is not saved.
- Use `WithPolicyEngine` to replace the default policy with host-specific checks.

This is not a full RBAC or approval system. It is the Agent-side enforcement point.

```go
result, err := agent.Run(ctx, agentcore.RunRequest{
	Input: "update the draft",
	AllowedPermissions: []policy.Permission{
		policy.PermissionWrite,
	},
	PolicyContext: ports.PolicyContext{
		RequestID: "request-123",
	},
})
```

See `examples/policy` for a minimal policy example.

## Tool Approval

Use `WithToolApprover` when a host needs to approve model-requested tool calls after policy checks and before execution. If no approver is configured, runs behave as before.

Approval denial aborts the run before the tool body executes. `RunDetailed` and `Agent.Stream` return partial results with the approval denial reason in `ExecutionSummary.AbortReason`.

An approver can return `ToolApprovalDecision{Pending: true}` to stop before execution. `Pending` takes precedence over `Allowed`, so ambiguous decisions fail closed. `RunDetailed` then returns an error matching `ErrApprovalPending` and a `RunCheckpoint`; serialize that checkpoint with `encoding/json`, store it in host-controlled sensitive storage, obtain host authorization, and call `ResumeDetailed` with exactly one matching resolution for every pending tool call.

```go
result, err := agent.RunDetailed(ctx, agentcore.RunRequest{
	Input: "publish the approved draft",
})
if errors.Is(err, agentcore.ErrApprovalPending) {
	checkpointBytes, err := json.Marshal(result.Interruption.Checkpoint)
	if err != nil {
		return err
	}
	// Store checkpointBytes with host access control, encryption, and expiration.
	var checkpoint agentcore.RunCheckpoint
	if err := json.Unmarshal(checkpointBytes, &checkpoint); err != nil {
		return err
	}

	result, err = agent.ResumeDetailed(ctx, checkpoint, []agentcore.ToolApprovalResolution{
		{
			Index:      0,
			ToolCallID: checkpoint.PendingCalls[0].ID,
			Tool:       checkpoint.PendingCalls[0].Name,
			Allowed:    true,
		},
	})
}
if err != nil {
	return err
}
```

`RunCheckpoint` contains raw user content and tool inputs. It is not an event payload and must not be sent to ordinary logs, telemetry, or untrusted clients. A resumed batch is atomic: duplicate, missing, swapped, or mismatched resolutions fail before any tool or new LLM call; if any resolution denies, no call in that batch executes. Policy is evaluated again immediately before resumed execution, and `ExecutionSummary.Duration` remains wall-clock time, including an approval wait.

Request-scoped tools are recreated through `ToolProvider` before a resume. Providers must therefore return the same valid tools for the checkpoint request while that checkpoint remains resumable.

Approval requested, completed, denied, and pending events flow through `WithEventSink` and `Agent.Stream` with bounded metadata such as tool name, index, and reason. They never contain raw tool input or a checkpoint.

## Budgets

Use `agentcore.WithBudget` to stop a run when accumulated provider token usage exceeds host-defined limits. Zero values are unlimited.

```go
agent, err := agentcore.NewAgent(
	agentcore.WithLLM(llm),
	agentcore.WithBudget(agentcore.Budget{
		MaxInputTokens:  4000,
		MaxOutputTokens: 1000,
		MaxTotalTokens:  5000,
	}),
)
```

Budget checks run after each model response and before policy or tool execution. When a budget is exceeded, `Run` returns an error that matches `agentcore.ErrBudgetExceeded`.

## Structured Output

Use `WithOutputFormat` when a host needs the final answer to follow a declared
shape. If `JSONSchema` is set, the core adds model-facing output instructions
before the first LLM call and validates the final answer as JSON before building
`RunResult`. Valid structured output is copied to `RunResult.StructuredOutput`.

Use `WithOutputValidator` for host-specific output checks that cannot be
represented as JSON Schema. Validation runs after the final model response and
before memory save. A validation failure aborts the run with an error matching
`agentcore.ErrOutputInvalid`.

This is an output contract, not a model-as-judge or business grading system.
Reusable quality checks should live in host code or `evalkit` graders.

## Session Memory

Memory is enabled only when an Agent has a `MemoryProvider` and the request includes `SessionID`.

- Memory loads before the current user input is appended.
- Memory loads at most once per run, even across multiple ReAct iterations.
- Memory saves only after a successful final answer.
- Memory is not saved when load, policy, tool, or max-iteration errors abort the run.
- Saved messages include prior loaded messages, current user input, non-silent tool observations, and the final assistant answer.

`memory.WindowMemory` is an in-process bounded session store. It is safe for concurrent use, drops older messages past its limit, and does not survive process restart. Summarization, compaction, vector retrieval, and durable storage are extension concerns, not core memory behavior.

See `examples/memory` for a minimal session continuity example.

## ReAct Stages

The default ReAct runner uses focused stages:

- `ContextStage` adds request input to the run messages.
- `PromptStage` compiles prompt blocks deterministically.
- `ContextProjectionStage` optionally compresses or reshapes the model-facing message view.
- `ThinkStage` calls the LLM and records final content or tool calls.
- `BudgetStage` aborts when configured usage limits are exceeded.
- `PolicyStage` checks requested tool permissions.
- `ActStage` executes tool calls.
- `ObserveStage` appends tool observations for the next LLM turn.
- `FinalizeStage` builds the final `RunResult`.

## Runtime Events

Runtime events are intentionally small. They include identifiers, event type, stage name, iteration, short messages, and bounded metadata such as tool name or memory message count.

Event payloads must not include raw prompts, raw tool inputs, full user content, or full tool output. Applications that need richer logs should make that an explicit host-layer decision around their own privacy and retention rules.

## Tool Result Separation

Tool results keep model-visible and user-visible output separate:

- `ForLLM` is appended as the observation used by later LLM calls.
- `ForUser` is available for UI surfaces.
- `Ref` can point to a host-owned artifact such as a stored query result, OCR output, or generated file.
- `Metadata` can carry small structured facts about the result, such as counts or MIME type.
- `Silent` suppresses observation messages.
- `IsError` marks recoverable tool errors so the agent can ask the LLM to correct tool arguments.

## Writing Tools

A tool is a host-owned typed action. Keep each tool focused on one operation and describe its boundary in `Spec`.

- `Permission` declares whether the action is read, write, or exec so policy can approve it.
- `ExecutionMode` controls scheduling: read tools run in parallel by default, while write, exec, empty, and unknown permissions run sequentially unless marked `ExecutionModeParallel`.
- `Schema.JSONSchema` is sent to providers as the native tool parameter schema and is also enforced locally before the tool body runs.
- `Schema.Validate` can add host-specific validation after JSON Schema validation succeeds.
- `Timeout` bounds tool and middleware execution.
- Return `ForLLM` for compact model observations, `ForUser` for host/UI output, and `Ref` when the full result lives in host storage.
- Use `Silent` when a successful tool result should not be appended to model context.
- Use `IsError` for recoverable domain errors the model can correct; return a Go error for executor failures that should abort the run.

See `examples/tools` for a minimal tool-authoring example.
