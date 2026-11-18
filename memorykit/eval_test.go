package memorykit

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEvalCorpusV2Contract(t *testing.T) {
	corpus := loadEvalCorpus(t)
	if err := ValidateEvalCorpus(corpus); err != nil {
		t.Fatalf("ValidateEvalCorpus(): %v", err)
	}
	if corpus.Version != EvalCorpusVersionV2 {
		t.Fatalf("version = %q", corpus.Version)
	}
	if len(corpus.Memories) != 36 || len(corpus.Cases) != 24 {
		t.Fatalf("shape = %d memories, %d cases", len(corpus.Memories), len(corpus.Cases))
	}
	counts := map[string]int{}
	for _, evalCase := range corpus.Cases {
		counts[evalCase.Category]++
	}
	want := map[string]int{
		"chinese_phrase": 4,
		"synonym":        4,
		"english":        4,
		"lifecycle":      4,
		"scope":          4,
		"hard_negative":  4,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("category counts = %#v, want %#v", counts, want)
	}
}

func TestValidateEvalCorpusRejectsMalformedCorpus(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*EvalCorpus)
	}{
		{name: "wrong version", mutate: func(c *EvalCorpus) { c.Version = "project-memory-retrieval-v1" }},
		{name: "wrong memory count", mutate: func(c *EvalCorpus) { c.Memories = c.Memories[:35] }},
		{name: "duplicate memory ID", mutate: func(c *EvalCorpus) { c.Memories[1].ID = c.Memories[0].ID }},
		{name: "blank memory ID", mutate: func(c *EvalCorpus) { c.Memories[0].ID = " " }},
		{name: "blank memory tenant", mutate: func(c *EvalCorpus) { c.Memories[0].TenantID = "" }},
		{name: "blank memory project", mutate: func(c *EvalCorpus) { c.Memories[0].ProjectID = "" }},
		{name: "invalid memory status", mutate: func(c *EvalCorpus) { c.Memories[0].Status = "deleted" }},
		{name: "invalid memory kind", mutate: func(c *EvalCorpus) { c.Memories[0].Kind = "preference" }},
		{name: "blank memory key", mutate: func(c *EvalCorpus) { c.Memories[0].Key = "" }},
		{name: "invalid memory content", mutate: func(c *EvalCorpus) { c.Memories[0].Content = "\xff" }},
		{name: "zero valid from", mutate: func(c *EvalCorpus) { c.Memories[0].ValidFrom = time.Time{} }},
		{name: "memory carries vector", mutate: func(c *EvalCorpus) { c.Memories[0].Vector = []float32{1} }},
		{name: "invalid valid until", mutate: func(c *EvalCorpus) { c.Memories[0].ValidUntil = c.Memories[0].ValidFrom }},
		{name: "wrong case count", mutate: func(c *EvalCorpus) { c.Cases = c.Cases[:23] }},
		{name: "duplicate case ID", mutate: func(c *EvalCorpus) { c.Cases[1].ID = c.Cases[0].ID }},
		{name: "blank case ID", mutate: func(c *EvalCorpus) { c.Cases[0].ID = "" }},
		{name: "unknown category", mutate: func(c *EvalCorpus) { c.Cases[0].Category = "other" }},
		{name: "wrong category count", mutate: func(c *EvalCorpus) { c.Cases[0].Category = "synonym" }},
		{name: "blank case tenant", mutate: func(c *EvalCorpus) { c.Cases[0].TenantID = "" }},
		{name: "blank case project", mutate: func(c *EvalCorpus) { c.Cases[0].ProjectID = "" }},
		{name: "blank query", mutate: func(c *EvalCorpus) { c.Cases[0].Query = "" }},
		{name: "blank rationale", mutate: func(c *EvalCorpus) { c.Cases[0].Rationale = "" }},
		{name: "zero case time", mutate: func(c *EvalCorpus) { c.Cases[0].Now = time.Time{} }},
		{name: "blank key", mutate: func(c *EvalCorpus) { c.Cases[8].Keys = []string{""} }},
		{name: "duplicate key", mutate: func(c *EvalCorpus) {
			c.Cases[8].Keys = append(c.Cases[8].Keys, c.Cases[8].Keys[0])
		}},
		{name: "missing expected ID", mutate: func(c *EvalCorpus) { c.Cases[0].ExpectedIDs = []string{"missing"} }},
		{name: "missing forbidden ID", mutate: func(c *EvalCorpus) { c.Cases[0].ForbiddenIDs = []string{"missing"} }},
		{name: "duplicate expected ID", mutate: func(c *EvalCorpus) {
			c.Cases[0].ExpectedIDs = append(c.Cases[0].ExpectedIDs, c.Cases[0].ExpectedIDs[0])
		}},
		{name: "duplicate forbidden ID", mutate: func(c *EvalCorpus) {
			c.Cases[0].ForbiddenIDs = append(c.Cases[0].ForbiddenIDs, c.Cases[0].ForbiddenIDs[0])
		}},
		{name: "expected forbidden overlap", mutate: func(c *EvalCorpus) {
			c.Cases[0].ForbiddenIDs = append(c.Cases[0].ForbiddenIDs, c.Cases[0].ExpectedIDs[0])
		}},
		{name: "expected ID from another project", mutate: func(c *EvalCorpus) {
			c.Cases[0].ExpectedIDs = []string{"m-scope-command-p2"}
		}},
		{name: "expected inactive ID", mutate: func(c *EvalCorpus) {
			c.Cases[0].ExpectedIDs = []string{"m-life-deploy-old"}
		}},
		{name: "expected expired ID", mutate: func(c *EvalCorpus) {
			c.Cases[0].ExpectedIDs = []string{"m-life-expired"}
		}},
		{name: "expected future ID", mutate: func(c *EvalCorpus) {
			c.Cases[0].ExpectedIDs = []string{"m-life-future"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			corpus := cloneEvalCorpus(t, loadEvalCorpus(t))
			test.mutate(&corpus)
			if err := ValidateEvalCorpus(corpus); err == nil {
				t.Fatal("ValidateEvalCorpus() error = nil")
			}
		})
	}
}

func loadEvalCorpus(t *testing.T) EvalCorpus {
	t.Helper()
	data, err := os.ReadFile("testdata/recall_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus EvalCorpus
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&corpus); err != nil {
		t.Fatal(err)
	}
	return corpus
}

func cloneEvalCorpus(t *testing.T, corpus EvalCorpus) EvalCorpus {
	t.Helper()
	data, err := json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	var cloned EvalCorpus
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

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
