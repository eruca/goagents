package memorykit

import "fmt"

type EvalMemory struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Status    string    `json:"status"`
	Kind      string    `json:"kind"`
	Key       string    `json:"key"`
	Content   string    `json:"content"`
	Vector    []float32 `json:"vector"`
}

type EvalCase struct {
	ID           string   `json:"id"`
	Query        string   `json:"query"`
	ExpectedIDs  []string `json:"expected_ids"`
	ForbiddenIDs []string `json:"forbidden_ids"`
}

type EvalCorpus struct {
	Version  string       `json:"version"`
	Memories []EvalMemory `json:"memories"`
	Cases    []EvalCase   `json:"cases"`
}

type EvalReport struct {
	Cases              int
	ExpectedFound      int
	ExpectedTotal      int
	ForbiddenInjected  int
	ForbiddenTotal     int
	RecallAtBudget     float64
	FalseInjectionRate float64
}

// Evaluate scores result IDs by exact set membership. Duplicate returned IDs
// count once, and missing result entries are treated as empty result sets.
func Evaluate(results map[string][]string, cases []EvalCase) (EvalReport, error) {
	report := EvalReport{Cases: len(cases)}
	caseIDs := make(map[string]struct{}, len(cases))
	for _, evalCase := range cases {
		if _, exists := caseIDs[evalCase.ID]; exists {
			return EvalReport{}, fmt.Errorf("duplicate evaluation case ID %q", evalCase.ID)
		}
		caseIDs[evalCase.ID] = struct{}{}

		expected := stringSet(evalCase.ExpectedIDs)
		forbidden := stringSet(evalCase.ForbiddenIDs)
		for id := range expected {
			if _, overlaps := forbidden[id]; overlaps {
				return EvalReport{}, fmt.Errorf("evaluation case %q expects and forbids ID %q", evalCase.ID, id)
			}
		}
		returned := stringSet(results[evalCase.ID])
		report.ExpectedTotal += len(expected)
		report.ForbiddenTotal += len(forbidden)
		for id := range expected {
			if _, found := returned[id]; found {
				report.ExpectedFound++
			}
		}
		for id := range forbidden {
			if _, found := returned[id]; found {
				report.ForbiddenInjected++
			}
		}
	}
	if report.ExpectedTotal != 0 {
		report.RecallAtBudget = float64(report.ExpectedFound) / float64(report.ExpectedTotal)
	}
	if report.ForbiddenTotal != 0 {
		report.FalseInjectionRate = float64(report.ForbiddenInjected) / float64(report.ForbiddenTotal)
	}
	return report, nil
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}
