# Budget Guard Design

> Date: 2026-04-11
> Goal: Add a small budget enforcement contract that stops Agent runs when configured token budgets are exceeded, without adding compaction or summarization yet.

## Context

The framework now has a real-provider boundary through `extensions/providers/openaiapi`. Providers can map service usage back into `ports.Usage`, and `ThinkStage` already accumulates usage into `RunState.Usage`. The missing core contract is what should happen when that usage exceeds a host-defined budget.

The original framework design included `BudgetGuard` and a later `CompactStage`, but the current code only has `WithMaxIterations`. The first budget slice should be explicit and conservative: enforce configured limits and abort. It should not prune messages, summarize memory, retry with smaller prompts, or add provider-specific token estimation.

## Design

Add a budget guard to `agentcore` rather than `ports` for the first slice. It consumes `RunState` and `Usage`, which are core runtime concepts. Hosts configure the default guard through Agent options, and advanced hosts can replace it with their own guard later if needed.

New types:

```go
var ErrBudgetExceeded = errors.New("budget exceeded")

type Budget struct {
	MaxInputTokens  int
	MaxOutputTokens int
	MaxTotalTokens  int
}

type BudgetDecision struct {
	Allowed bool
	Reason  string
}

type BudgetGuard interface {
	Check(ctx context.Context, state *RunState) BudgetDecision
}
```

Default behavior:

- If no budget is configured, behavior is unchanged.
- A zero or negative budget field means that field is unlimited.
- `MaxInputTokens` checks accumulated `state.Usage.InputTokens`.
- `MaxOutputTokens` checks accumulated `state.Usage.OutputTokens`.
- `MaxTotalTokens` checks input plus output tokens.
- Budget is checked after `ThinkStage` records model usage and before policy/tool execution continues.
- If denied, the run aborts with an error wrapping `ErrBudgetExceeded`.
- Memory is not saved because the run did not reach a final answer.
- Event sinks see a stage failure event for the budget stage.

## Runtime Flow

Insert `BudgetStage` after `ThinkStage` and before `PolicyStage`:

```text
MemoryLoadStage
ContextStage
SystemPromptStage
SkillStage
ToolProviderStage
PromptStage
ThinkStage
BudgetStage
PolicyStage
ActStage
ObserveStage
FinalizeStage
memory save
```

This order is intentional. The framework only knows actual provider usage after an LLM response. Checking before `ThinkStage` would require token estimation, which is out of scope. Checking before `PolicyStage` prevents tool execution after a model call has already exceeded budget.

## Agent Options

Add:

```go
func WithBudget(budget Budget) Option
func WithBudgetGuard(guard BudgetGuard) Option
```

`WithBudget` should create the default deterministic guard. `WithBudgetGuard` should allow host-specific logic. If both are provided, the last option wins, following existing option semantics.

## Error Handling

`BudgetStage` returns `StageAbort` and an error like:

```text
budget exceeded: total tokens 1201 exceeds max 1200
```

The error should support:

```go
errors.Is(err, ErrBudgetExceeded)
```

This makes budget failures easy for host applications to classify without parsing error strings.

## Testing

Add deterministic tests using mock LLM usage:

- no budget configured keeps existing behavior
- input token budget exceeded aborts
- output token budget exceeded aborts
- total token budget exceeded aborts
- budget denial stops before tool execution
- budget denial does not save memory
- custom `BudgetGuard` can deny a run
- custom `BudgetGuard` can allow a run
- budget stage emits a stage failure event through existing pipeline event behavior

No tests should call real provider services.

## Non-Goals

- No prompt token estimation.
- No message pruning.
- No `CompactStage`.
- No memory summarization.
- No retry after budget failure.
- No provider-specific token counting.
- No pricing/cost budgets.
- No per-tool budget accounting.
- No persistent budget ledger.

## Success Criteria

- Existing behavior is unchanged when no budget is configured.
- Configured token budgets abort runs deterministically.
- Budget failures are distinguishable with `errors.Is(err, ErrBudgetExceeded)`.
- Tool execution and memory save do not happen after budget denial.
- `make verify` passes.
