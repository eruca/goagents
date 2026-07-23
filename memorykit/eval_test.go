package memorykit

import (
	"reflect"
	"testing"
)

func TestEvaluateComputesExactSetMetrics(t *testing.T) {
	cases := []EvalCase{
		{ID: "one", ExpectedIDs: []string{"m-a", "m-b"}, ForbiddenIDs: []string{"m-x"}},
		{ID: "two", ExpectedIDs: []string{"m-c"}, ForbiddenIDs: []string{"m-y", "m-z"}},
		{ID: "empty"},
	}
	got, err := Evaluate(map[string][]string{
		"one": {"m-a", "m-a", "m-x", "unscored"},
		"two": {"m-y"},
	}, cases)
	if err != nil {
		t.Fatal(err)
	}
	want := EvalReport{
		Cases: 3, ExpectedFound: 1, ExpectedTotal: 3,
		ForbiddenInjected: 2, ForbiddenTotal: 3,
		RecallAtBudget: 1.0 / 3.0, FalseInjectionRate: 2.0 / 3.0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate() = %#v, want %#v", got, want)
	}

	zero, err := Evaluate(nil, []EvalCase{{ID: "zero"}})
	if err != nil {
		t.Fatal(err)
	}
	if zero.RecallAtBudget != 0 || zero.FalseInjectionRate != 0 {
		t.Fatalf("zero denominators = %#v", zero)
	}
}

func TestEvaluateRejectsAmbiguousCases(t *testing.T) {
	tests := []struct {
		name  string
		cases []EvalCase
	}{
		{name: "duplicate case ID", cases: []EvalCase{{ID: "same"}, {ID: "same"}}},
		{name: "expected forbidden overlap", cases: []EvalCase{{ID: "case", ExpectedIDs: []string{"m-a"}, ForbiddenIDs: []string{"m-a"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Evaluate(nil, test.cases); err == nil {
				t.Fatal("Evaluate() error = nil")
			}
		})
	}
}
