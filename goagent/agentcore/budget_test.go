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
