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
