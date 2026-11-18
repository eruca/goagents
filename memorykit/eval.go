package memorykit

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const EvalCorpusVersionV2 = "project-memory-retrieval-v2"

type EvalMemory struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	ProjectID  string    `json:"project_id"`
	Status     string    `json:"status"`
	Kind       string    `json:"kind"`
	Key        string    `json:"key"`
	Content    string    `json:"content"`
	ValidFrom  time.Time `json:"valid_from"`
	ValidUntil time.Time `json:"valid_until,omitempty"`
	// Vector is a hand-made embedding used only by the small vector-ablation
	// corpus (testdata/recall_cases_vector_v1.json). The v2 corpus is lexical
	// and must not carry vectors.
	Vector []float32 `json:"vector,omitempty"`
}

type EvalCase struct {
	ID           string    `json:"id"`
	Category     string    `json:"category"`
	TenantID     string    `json:"tenant_id"`
	ProjectID    string    `json:"project_id"`
	Query        string    `json:"query"`
	Keys         []string  `json:"keys,omitempty"`
	Now          time.Time `json:"now"`
	ExpectedIDs  []string  `json:"expected_ids"`
	ForbiddenIDs []string  `json:"forbidden_ids"`
	Rationale    string    `json:"rationale"`
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

// ValidateEvalCorpus rejects malformed fixtures before they can produce
// misleading recall metrics. It validates generic structure and lifecycle
// eligibility without coupling production code to individual fixture IDs.
func ValidateEvalCorpus(corpus EvalCorpus) error {
	if corpus.Version != EvalCorpusVersionV2 {
		return fmt.Errorf("evaluation corpus version %q is unsupported", corpus.Version)
	}
	if len(corpus.Memories) != 36 || len(corpus.Cases) != 24 {
		return fmt.Errorf("evaluation corpus must contain 36 memories and 24 cases")
	}

	memories := make(map[string]EvalMemory, len(corpus.Memories))
	for _, memory := range corpus.Memories {
		if !validEvalLabel(memory.ID) {
			return fmt.Errorf("evaluation memory has an invalid ID")
		}
		if _, exists := memories[memory.ID]; exists {
			return fmt.Errorf("duplicate evaluation memory ID %q", memory.ID)
		}
		scope := Scope{
			TenantID:    memory.TenantID,
			SubjectType: SubjectProject,
			SubjectID:   memory.ProjectID,
		}
		if !validEvalLabel(memory.TenantID) || !validEvalLabel(memory.ProjectID) || scope.Validate() != nil {
			return fmt.Errorf("evaluation memory %q has an invalid scope", memory.ID)
		}
		if !Status(memory.Status).IsValid() || !Kind(memory.Kind).IsValid() ||
			!utf8.ValidString(memory.Key) || !validKey(memory.Key, 128) ||
			!utf8.ValidString(memory.Content) || !validContent(memory.Content, 4096) ||
			memory.ValidFrom.IsZero() || !validTimeRange(memory.ValidFrom, memory.ValidUntil) ||
			len(memory.Vector) != 0 {
			return fmt.Errorf("evaluation memory %q is invalid", memory.ID)
		}
		memories[memory.ID] = memory
	}

	approvedCategories := map[string]int{
		"chinese_phrase": 0,
		"synonym":        0,
		"english":        0,
		"lifecycle":      0,
		"scope":          0,
		"hard_negative":  0,
	}
	caseIDs := make(map[string]struct{}, len(corpus.Cases))
	for _, evalCase := range corpus.Cases {
		if !validEvalLabel(evalCase.ID) {
			return fmt.Errorf("evaluation case has an invalid ID")
		}
		if _, exists := caseIDs[evalCase.ID]; exists {
			return fmt.Errorf("duplicate evaluation case ID %q", evalCase.ID)
		}
		caseIDs[evalCase.ID] = struct{}{}
		if _, approved := approvedCategories[evalCase.Category]; !approved {
			return fmt.Errorf("evaluation case %q has an invalid category", evalCase.ID)
		}
		approvedCategories[evalCase.Category]++
		scope := Scope{
			TenantID:    evalCase.TenantID,
			SubjectType: SubjectProject,
			SubjectID:   evalCase.ProjectID,
		}
		if !validEvalLabel(evalCase.TenantID) || !validEvalLabel(evalCase.ProjectID) || scope.Validate() != nil ||
			!validEvalText(evalCase.Query, 4096) || !validEvalText(evalCase.Rationale, 4096) ||
			evalCase.Now.IsZero() {
			return fmt.Errorf("evaluation case %q is invalid", evalCase.ID)
		}
		if err := validateEvalKeys(evalCase); err != nil {
			return err
		}
		if err := validateEvalReferences(evalCase, memories); err != nil {
			return err
		}
	}
	for category, count := range approvedCategories {
		if count != 4 {
			return fmt.Errorf("evaluation category %q must contain four cases", category)
		}
	}
	return nil
}

func validateEvalKeys(evalCase EvalCase) error {
	seen := make(map[string]struct{}, len(evalCase.Keys))
	for _, key := range evalCase.Keys {
		if !utf8.ValidString(key) || !validKey(key, 128) {
			return fmt.Errorf("evaluation case %q has an invalid key", evalCase.ID)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("evaluation case %q has duplicate key %q", evalCase.ID, key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateEvalReferences(evalCase EvalCase, memories map[string]EvalMemory) error {
	expected := make(map[string]struct{}, len(evalCase.ExpectedIDs))
	for _, id := range evalCase.ExpectedIDs {
		memory, exists := memories[id]
		if !validEvalLabel(id) || !exists {
			return fmt.Errorf("evaluation case %q references unknown expected ID %q", evalCase.ID, id)
		}
		if _, duplicate := expected[id]; duplicate {
			return fmt.Errorf("evaluation case %q has duplicate expected ID %q", evalCase.ID, id)
		}
		expected[id] = struct{}{}
		if memory.TenantID != evalCase.TenantID || memory.ProjectID != evalCase.ProjectID ||
			Status(memory.Status) != StatusActive || evalCase.Now.Before(memory.ValidFrom) ||
			(!memory.ValidUntil.IsZero() && !evalCase.Now.Before(memory.ValidUntil)) {
			return fmt.Errorf("evaluation case %q expects ineligible memory %q", evalCase.ID, id)
		}
	}

	forbidden := make(map[string]struct{}, len(evalCase.ForbiddenIDs))
	for _, id := range evalCase.ForbiddenIDs {
		if !validEvalLabel(id) {
			return fmt.Errorf("evaluation case %q has an invalid forbidden ID", evalCase.ID)
		}
		if _, exists := memories[id]; !exists {
			return fmt.Errorf("evaluation case %q references unknown forbidden ID %q", evalCase.ID, id)
		}
		if _, duplicate := forbidden[id]; duplicate {
			return fmt.Errorf("evaluation case %q has duplicate forbidden ID %q", evalCase.ID, id)
		}
		if _, overlaps := expected[id]; overlaps {
			return fmt.Errorf("evaluation case %q expects and forbids ID %q", evalCase.ID, id)
		}
		forbidden[id] = struct{}{}
	}
	return nil
}

func validEvalLabel(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || !utf8.ValidString(value) ||
		utf8.RuneCountInString(value) > 128 {
		return false
	}
	for _, r := range value {
		if r <= 0x1f || r == 0x7f {
			return false
		}
	}
	return true
}

func validEvalText(value string, maxRunes int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" &&
		!strings.ContainsRune(value, 0) && utf8.RuneCountInString(value) <= maxRunes
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
