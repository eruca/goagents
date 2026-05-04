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

func budgetFromGuard(guard BudgetGuard) Budget {
	if typed, ok := guard.(budgetGuard); ok {
		return typed.budget
	}
	return Budget{}
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
