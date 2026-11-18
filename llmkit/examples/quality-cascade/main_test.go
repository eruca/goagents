package main

import (
	"context"
	"testing"
)

func TestOfflineDemoEvaluatesThenEscalates(t *testing.T) {
	r, err := demo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Response.Content != "20" || len(r.Attempts) != 2 || r.Attempts[0].ModelAlias != "fixture-cheap" || r.Attempts[0].TestsPassed || r.Attempts[1].ModelAlias != "fixture-strong" || !r.Attempts[1].TestsPassed || r.EstimatedCents != 1 {
		t.Fatalf("unexpected demo result: %+v", r)
	}
}
