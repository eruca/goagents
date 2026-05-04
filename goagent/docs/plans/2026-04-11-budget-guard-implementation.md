# Budget Guard Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add deterministic token budget enforcement that aborts Agent runs after provider usage exceeds configured limits.

**Architecture:** Keep budgeting in `agentcore` because it depends on `RunState` and accumulated runtime usage. Add a small `BudgetGuard` contract, a default guard, and a `BudgetStage` placed after `ThinkStage` and before `PolicyStage` so denied runs cannot execute tools or save memory.

**Tech Stack:** Go standard library, `agentcore` stage pipeline, existing mock LLM and memory tests, `make verify`.

---

## Constraints

- Do not add compaction, summarization, pruning, retries, pricing, token estimation, or provider-specific behavior.
- Preserve current behavior when no budget or guard is configured.
- Budget failures must support `errors.Is(err, ErrBudgetExceeded)`.
- Keep tests deterministic; do not call real provider services.

## Task 1: Add Budget Types and Default Guard

**Files:**
- Create: `agentcore/budget.go`
- Test: `agentcore/budget_test.go`

**Step 1: Write failing tests**

Create `agentcore/budget_test.go` with table-driven tests for the default guard:

```go
package agentcore

import (
	"context"
	"strings"
	"testing"
)

func TestDefaultBudgetGuardAllowsUnlimitedBudget(t *testing.T) {
	state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
	state.Usage.InputTokens = 100
	state.Usage.OutputTokens = 50

	decision := NewBudgetGuard(Budget{}).Check(context.Background(), state)
	if !decision.Allowed {
		t.Fatalf("allowed = false, reason = %q", decision.Reason)
	}
}

func TestDefaultBudgetGuardDeniesExceededBudgets(t *testing.T) {
	tests := []struct {
		name       string
		budget     Budget
		input      int
		output     int
		wantReason string
	}{
		{
			name:       "input",
			budget:     Budget{MaxInputTokens: 9},
			input:      10,
			wantReason: "input tokens 10 exceeds max 9",
		},
		{
			name:       "output",
			budget:     Budget{MaxOutputTokens: 4},
			output:     5,
			wantReason: "output tokens 5 exceeds max 4",
		},
		{
			name:       "total",
			budget:     Budget{MaxTotalTokens: 14},
			input:      10,
			output:     5,
			wantReason: "total tokens 15 exceeds max 14",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
			state.Usage.InputTokens = tt.input
			state.Usage.OutputTokens = tt.output

			decision := NewBudgetGuard(tt.budget).Check(context.Background(), state)
			if decision.Allowed {
				t.Fatalf("allowed = true")
			}
			if !strings.Contains(decision.Reason, tt.wantReason) {
				t.Fatalf("reason = %q, want contains %q", decision.Reason, tt.wantReason)
			}
		})
	}
}
```

**Step 2: Run tests to verify they fail**

Run:

```bash
go test ./agentcore -run 'TestDefaultBudgetGuard' -count=1
```

Expected: FAIL because `Budget` and `NewBudgetGuard` do not exist.

**Step 3: Implement minimal code**

Create `agentcore/budget.go`:

```go
package agentcore

import (
	"context"
	"errors"
	"fmt"
)

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

type budgetGuard struct {
	budget Budget
}

func NewBudgetGuard(budget Budget) BudgetGuard {
	return budgetGuard{budget: budget}
}

func (g budgetGuard) Check(ctx context.Context, state *RunState) BudgetDecision {
	_ = ctx
	usage := state.Usage
	if g.budget.MaxInputTokens > 0 && usage.InputTokens > g.budget.MaxInputTokens {
		return BudgetDecision{Reason: fmt.Sprintf("input tokens %d exceeds max %d", usage.InputTokens, g.budget.MaxInputTokens)}
	}
	if g.budget.MaxOutputTokens > 0 && usage.OutputTokens > g.budget.MaxOutputTokens {
		return BudgetDecision{Reason: fmt.Sprintf("output tokens %d exceeds max %d", usage.OutputTokens, g.budget.MaxOutputTokens)}
	}
	total := usage.InputTokens + usage.OutputTokens
	if g.budget.MaxTotalTokens > 0 && total > g.budget.MaxTotalTokens {
		return BudgetDecision{Reason: fmt.Sprintf("total tokens %d exceeds max %d", total, g.budget.MaxTotalTokens)}
	}
	return BudgetDecision{Allowed: true}
}
```

**Step 4: Run tests**

Run:

```bash
go test ./agentcore -run 'TestDefaultBudgetGuard' -count=1
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/budget.go agentcore/budget_test.go
git commit -m "feat: add budget guard contract"
```

## Task 2: Add BudgetStage

**Files:**
- Create: `agentcore/budget_stage.go`
- Test: `agentcore/budget_stage_test.go`

**Step 1: Write failing tests**

Create `agentcore/budget_stage_test.go`:

```go
package agentcore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type denyBudgetGuard struct {
	reason string
}

func (g denyBudgetGuard) Check(ctx context.Context, state *RunState) BudgetDecision {
	return BudgetDecision{Allowed: false, Reason: g.reason}
}

type allowBudgetGuard struct{}

func (g allowBudgetGuard) Check(ctx context.Context, state *RunState) BudgetDecision {
	return BudgetDecision{Allowed: true}
}

func TestBudgetStageContinuesWithoutGuard(t *testing.T) {
	result, err := BudgetStage{}.Run(context.Background(), NewRunState(NewRunID(), RunRequest{Input: "hello"}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result != StageContinue {
		t.Fatalf("result = %v", result)
	}
}

func TestBudgetStageContinuesWhenGuardAllows(t *testing.T) {
	result, err := BudgetStage{Guard: allowBudgetGuard{}}.Run(context.Background(), NewRunState(NewRunID(), RunRequest{Input: "hello"}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if result != StageContinue {
		t.Fatalf("result = %v", result)
	}
}

func TestBudgetStageAbortsWhenGuardDenies(t *testing.T) {
	result, err := BudgetStage{Guard: denyBudgetGuard{reason: "total tokens 11 exceeds max 10"}}.Run(context.Background(), NewRunState(NewRunID(), RunRequest{Input: "hello"}))
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if !strings.Contains(err.Error(), "total tokens 11 exceeds max 10") {
		t.Fatalf("err = %q", err.Error())
	}
	if result != StageAbort {
		t.Fatalf("result = %v", result)
	}
}
```

**Step 2: Run tests to verify they fail**

Run:

```bash
go test ./agentcore -run 'TestBudgetStage' -count=1
```

Expected: FAIL because `BudgetStage` does not exist.

**Step 3: Implement minimal code**

Create `agentcore/budget_stage.go`:

```go
package agentcore

import (
	"context"
	"fmt"
)

type BudgetStage struct {
	Guard BudgetGuard
}

func (s BudgetStage) Name() string {
	return "budget"
}

func (s BudgetStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Guard == nil {
		return StageContinue, nil
	}
	decision := s.Guard.Check(ctx, state)
	if decision.Allowed {
		return StageContinue, nil
	}
	if decision.Reason == "" {
		decision.Reason = "budget guard denied run"
	}
	return StageAbort, fmt.Errorf("%w: %s", ErrBudgetExceeded, decision.Reason)
}
```

**Step 4: Run tests**

Run:

```bash
go test ./agentcore -run 'TestBudgetStage|TestDefaultBudgetGuard' -count=1
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/budget_stage.go agentcore/budget_stage_test.go
git commit -m "feat: add budget stage"
```

## Task 3: Wire Agent Options and Pipeline

**Files:**
- Modify: `agentcore/agent.go`
- Modify: `agentcore/finalize_stage.go`
- Test: `agentcore/agent_test.go`

**Step 1: Write failing integration tests**

Add tests to `agentcore/agent_test.go` near the other Agent option and memory tests:

```go
func TestAgentAbortsWhenBudgetExceeded(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		Content: "agent answer",
		Usage:   ports.Usage{InputTokens: 7, OutputTokens: 5},
	}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithBudget(Budget{MaxTotalTokens: 10}),
	)
	if err != nil {
		t.Fatalf("NewAgent() err = %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
}

func TestAgentBudgetDenialStopsBeforeToolExecution(t *testing.T) {
	toolRan := false
	registry := tools.NewRegistry()
	registry.Register(tools.Tool{
		Spec: tools.ToolSpec{Name: "lookup", Permission: tools.PermissionRead},
		Handler: func(ctx context.Context, input json.RawMessage) (any, error) {
			toolRan = true
			return "result", nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}},
		Usage:     ports.Usage{InputTokens: 6, OutputTokens: 5},
	}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
		WithBudget(Budget{MaxTotalTokens: 10}),
	)
	if err != nil {
		t.Fatalf("NewAgent() err = %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if toolRan {
		t.Fatalf("tool ran after budget denial")
	}
}

func TestAgentBudgetDenialDoesNotSaveMemory(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		Content: "agent answer",
		Usage:   ports.Usage{InputTokens: 6, OutputTokens: 5},
	}}}
	memory := &mockMemoryProvider{}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
		WithBudget(Budget{MaxTotalTokens: 10}),
	)
	if err != nil {
		t.Fatalf("NewAgent() err = %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello", SessionID: "session_1"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if memory.saveCalls != 0 {
		t.Fatalf("save calls = %d", memory.saveCalls)
	}
}

func TestAgentCustomBudgetGuardCanAllowRun(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithBudgetGuard(allowBudgetGuard{}),
	)
	if err != nil {
		t.Fatalf("NewAgent() err = %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if result.Content != "agent answer" {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestAgentBudgetStageEmitsFailureEvent(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		Content: "agent answer",
		Usage:   ports.Usage{InputTokens: 11},
	}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithBudget(Budget{MaxInputTokens: 10}),
		WithEventSink(sink),
	)
	if err != nil {
		t.Fatalf("NewAgent() err = %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if !sink.hasStageEvent(EventStageFailed, "budget") {
		t.Fatalf("missing budget failure event: %#v", sink.events)
	}
}
```

If `recordingEventSink` lacks a stage-specific helper, add this helper near `hasEvent`:

```go
func (s *recordingEventSink) hasStageEvent(eventType EventType, stage string) bool {
	for _, event := range s.events {
		if event.Type == eventType && event.Stage == stage {
			return true
		}
	}
	return false
}
```

**Step 2: Run tests to verify they fail**

Run:

```bash
go test ./agentcore -run 'TestAgent.*Budget' -count=1
```

Expected: FAIL because `WithBudget`, `WithBudgetGuard`, and pipeline wiring do not exist.

**Step 3: Implement options and config**

Modify `agentcore/agent.go`:

```go
type Agent struct {
	// existing fields...
	budgetGuard BudgetGuard
}

func WithBudget(budget Budget) Option {
	return func(a *Agent) {
		a.budgetGuard = NewBudgetGuard(budget)
	}
}

func WithBudgetGuard(guard BudgetGuard) Option {
	return func(a *Agent) {
		a.budgetGuard = guard
	}
}
```

Pass the guard into `ReActConfig` in `Agent.Run`:

```go
BudgetGuard: a.budgetGuard,
```

Modify `agentcore/finalize_stage.go`:

```go
type ReActConfig struct {
	// existing fields...
	BudgetGuard BudgetGuard
}
```

Insert the new stage after `ThinkStage` and before `PolicyStage`:

```go
ThinkStage{LLM: config.LLM, ToolRegistry: registry},
BudgetStage{Guard: config.BudgetGuard},
PolicyStage{Engine: config.PolicyEngine, ToolRegistry: registry},
```

**Step 4: Run tests**

Run:

```bash
go test ./agentcore -run 'TestAgent.*Budget|TestBudgetStage|TestDefaultBudgetGuard' -count=1
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/agent.go agentcore/finalize_stage.go agentcore/agent_test.go
git commit -m "feat: wire budget guard into agent runs"
```

## Task 4: Document Budget Guard

**Files:**
- Modify: `README.md`

**Step 1: Add documentation**

Add a short `Budgets` section near the existing Agent configuration examples:

````markdown
### Budgets

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
````

**Step 2: Run targeted docs-adjacent checks**

Run:

```bash
go test ./agentcore -count=1
```

Expected: PASS.

**Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document budget guard"
```

## Task 5: Final Verification

**Files:**
- No new code changes expected.

**Step 1: Run full verification**

Run:

```bash
make verify
```

Expected: PASS for `go test ./...`, `go test -race ./...`, and `go run ./examples/basic`.

**Step 2: Inspect git status**

Run:

```bash
git status --short --branch
```

Expected: clean working tree, branch ahead of remote by the new commits.

**Step 3: Report completion**

Summarize:

- New public API: `Budget`, `BudgetGuard`, `WithBudget`, `WithBudgetGuard`, `ErrBudgetExceeded`.
- Runtime behavior: budget checked after `ThinkStage`, before policy/tool execution.
- Verification result: `make verify` passed.
