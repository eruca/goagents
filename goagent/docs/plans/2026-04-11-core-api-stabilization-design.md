# Core API Stabilization Design

> Date: 2026-04-11
> Goal: Clarify the public core API boundary and make common abort reasons classifiable without expanding core responsibilities.

## Context

The framework now has stable-looking extension points for LLMs, prompts, tools, policy, memory, events, and budgets. It also exposes many runtime types because the implementation is intentionally small and stage-based. Before adding more extensions, the project needs a sharper API contract so host applications know what is safe to depend on.

The current code already has sentinel errors for missing LLM, max iterations, and budget exceeded. Policy denial and missing tools are still plain formatted errors. That makes host applications parse strings for two common operational outcomes.

## Options

### Option A: Documentation-Only Stabilization

Document stable APIs, advanced runtime APIs, and extension responsibilities. Do not change code.

Trade-off: safest short term, but leaves policy/tool errors hard to classify.

### Option B: Boundary Docs Plus Small Error Sentinels

Document API tiers and add sentinel errors only for common framework-owned abort reasons:

- `agentcore.ErrPolicyDenied`
- `tools.ErrToolNotFound`

Trade-off: small API addition, low risk, and immediately improves host error handling.

### Option C: Aggressive API Hiding

Rename stage/pipeline/runtime types to unexported identifiers and expose only `Agent`.

Trade-off: cleaner surface later, but disruptive now and likely premature while the framework is still evolving.

## Decision

Use Option B.

Do not hide exported stage or pipeline types in this pass. Instead, classify them as advanced runtime API. This keeps tests, custom runners, and examples working while making the intended host-facing surface clearer.

## API Tiers

### Stable Host API

These are intended for host applications and extension packages:

- `agentcore.NewAgent`
- `agentcore.Agent.Run`
- `agentcore.RunRequest`, `RunResult`, `RunID`
- `agentcore.Option` and `With...` options
- `agentcore.Budget`, `BudgetGuard`, `WithBudget`, `WithBudgetGuard`
- `agentcore.Event`, `EventSink`, `EventType`
- `agentcore.Skill`, `SkillProvider`
- `agentcore.SystemPromptProvider`, `ToolProvider`, `Module`
- `ports.LLMClient`
- `ports.PromptCompiler`
- `ports.Tool`, `ToolRegistry`, `ToolSpec`, `ToolCall`, `ToolResult`
- `ports.PolicyEngine`
- `ports.MemoryProvider`
- `prompt.Block`, `prompt.Compiler`
- `tools.Tool`, `tools.Registry`, `tools.Executor`
- `policy.Engine`
- `memory.WindowMemory`

### Advanced Runtime API

These remain exported for tests and advanced composition, but host applications should prefer `Agent` unless they are replacing the runtime loop:

- `RunState`
- `Message`
- `Usage`
- `Stage`, `StageResult`
- `Pipeline`
- stage structs such as `ThinkStage`, `PolicyStage`, `ActStage`, and `FinalizeStage`
- `ReActRunner`, `ReActConfig`
- `MutableToolRegistry`

### Internal Responsibilities

The core should not own:

- provider-specific clients beyond optional extension packages
- HTTP/SSE transport
- durable memory backends
- vector memory
- context compression or summary generation
- multi-agent orchestration
- cost ledgers or pricing

## Error Semantics

Keep existing sentinels:

- `agentcore.ErrMissingLLM`
- `agentcore.ErrMaxIterations`
- `agentcore.ErrBudgetExceeded`

Add:

- `agentcore.ErrPolicyDenied`: returned when policy denies a model-requested tool call.
- `tools.ErrToolNotFound`: returned by the built-in tool registries when a named tool is missing.

Errors should keep useful messages but support `errors.Is`. For example:

```go
errors.Is(err, agentcore.ErrPolicyDenied)
errors.Is(err, tools.ErrToolNotFound)
```

Do not add sentinels for every possible provider, schema, middleware, or tool execution error. Those errors are often host-owned.

## Testing

Add focused contract tests:

- policy denial from `Agent.Run` matches `ErrPolicyDenied`
- missing tool from `PolicyStage` matches `tools.ErrToolNotFound`
- built-in `tools.Registry.MustGet` missing tool matches `tools.ErrToolNotFound`

Existing budget and max-iteration tests already cover their sentinels.

## Success Criteria

- README clearly describes stable API, advanced runtime API, and out-of-core responsibilities.
- Package docs for `agentcore`, `ports`, and `tools` reinforce the boundary.
- Host applications can classify policy denial and missing tools with `errors.Is`.
- Existing behavior and messages remain useful.
- `make verify` passes.
