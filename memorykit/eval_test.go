package memorykit

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode"
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

func TestEvaluateProjectMemoryV1Ablations(t *testing.T) {
	corpus := loadEvalCorpus(t)
	if corpus.Version != "project-memory-v1" {
		t.Fatalf("corpus version = %q", corpus.Version)
	}
	if len(corpus.Memories) != 6 || len(corpus.Cases) != 4 {
		t.Fatalf("corpus shape = %d memories, %d cases", len(corpus.Memories), len(corpus.Cases))
	}

	exact := runEvalPolicy(corpus, evalExact)
	fullText := runEvalPolicy(corpus, evalFullText)
	vector := runEvalPolicy(corpus, evalVector)
	fused := runEvalPolicy(corpus, evalExact|evalFullText|evalVector)
	reports := make(map[string]EvalReport, 4)
	for name, results := range map[string]map[string][]string{
		"exact-only": exact, "fts-only": fullText, "vector-only": vector, "fused": fused,
	} {
		report, err := Evaluate(results, corpus.Cases)
		if err != nil {
			t.Fatalf("Evaluate(%s): %v", name, err)
		}
		reports[name] = report
		t.Logf("%s: %+v", name, report)
	}

	if !containsEvalID(vector["semantic-synonym"], "m-pg-gate") ||
		!containsEvalID(fused["semantic-synonym"], "m-pg-gate") {
		t.Fatalf("semantic-only case was not recovered: vector=%v fused=%v", vector["semantic-synonym"], fused["semantic-synonym"])
	}
	if reports["fused"].RecallAtBudget != 1 || reports["fused"].FalseInjectionRate != 0 {
		t.Fatalf("fused report = %#v", reports["fused"])
	}
	for _, evalCase := range corpus.Cases {
		for _, forbidden := range evalCase.ForbiddenIDs {
			if containsEvalID(fused[evalCase.ID], forbidden) {
				t.Fatalf("fused case %q injected forbidden ID %q", evalCase.ID, forbidden)
			}
		}
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

type evalChannels uint8

const (
	evalExact evalChannels = 1 << iota
	evalFullText
	evalVector
)

func runEvalPolicy(corpus EvalCorpus, channels evalChannels) map[string][]string {
	results := make(map[string][]string, len(corpus.Cases))
	for _, evalCase := range corpus.Cases {
		seen := make(map[string]struct{})
		for _, memory := range corpus.Memories {
			if memory.ProjectID != "project-1" || memory.Status != "active" {
				continue
			}
			matched := channels&evalExact != 0 && matchesEvalKey(evalCase.Query, memory.Key)
			matched = matched || channels&evalFullText != 0 && containsAllEvalTokens(memory.Key+" "+memory.Content, evalCase.Query)
			matched = matched || channels&evalVector != 0 && cosineSimilarity(evalQueryVector(evalCase.Query), memory.Vector) >= 0.9
			if matched {
				seen[memory.ID] = struct{}{}
			}
		}
		for id := range seen {
			results[evalCase.ID] = append(results[evalCase.ID], id)
		}
		sort.Strings(results[evalCase.ID])
	}
	return results
}

func evalTokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func matchesEvalKey(query, key string) bool {
	leftSet := make(map[string]struct{})
	for _, token := range evalTokens(query) {
		leftSet[strings.TrimSuffix(token, "s")] = struct{}{}
	}
	matches := 0
	for _, token := range evalTokens(key) {
		if _, ok := leftSet[strings.TrimSuffix(token, "s")]; ok {
			matches++
		}
	}
	return matches >= 2
}

func containsAllEvalTokens(document, query string) bool {
	documentSet := make(map[string]struct{})
	for _, token := range evalTokens(document) {
		documentSet[token] = struct{}{}
	}
	for _, token := range evalTokens(query) {
		if _, ok := documentSet[token]; !ok {
			return false
		}
	}
	return true
}

func evalQueryVector(query string) []float32 {
	var vector [3]float32
	for _, token := range evalTokens(query) {
		switch token {
		case "command", "test", "tests", "deployment", "old":
			vector[0]++
		case "verify", "storage", "vector", "unreviewed", "lesson":
			vector[1]++
		case "documentation", "graphics":
			vector[2]++
		}
	}
	return vector[:]
}

func cosineSimilarity(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		dot += float64(left[index] * right[index])
		leftNorm += float64(left[index] * left[index])
		rightNorm += float64(right[index] * right[index])
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / math.Sqrt(leftNorm*rightNorm)
}

func containsEvalID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
