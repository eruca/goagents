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
