package main

import (
	"context"
	"testing"
)

func TestOfflineEvaluationReportsRescueAndHoldout(t *testing.T) {
	r, err := evaluationDemo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Verifier.Passed || r.Verifier.FalseAccepts != 0 || r.Verifier.FalseRejects != 0 || r.Chain.Solved != 20 || r.Chain.TotalAttempts != 22 || r.Chain.TotalEstimatedCents != 4 || r.TotalEstimatedCents != 304 {
		t.Fatalf("unexpected fixture report: %+v", r)
	}
}
