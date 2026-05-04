package llmkit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONLRecorderRecordsRouteEvent(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nested", "llmkit")
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}

	trace := RouteTrace{
		TaskType:              "summarize",
		AccountAlias:          "primary-account",
		ModelAlias:            "fast-json",
		Provider:              "openai",
		Selected:              true,
		Reason:                "lowest latency available",
		Score:                 42,
		ScoreBreakdown:        map[string]int{"latency": 20, "price": 10},
		CandidateModelAliases: []string{"fast-json", "local-small"},
	}
	if err := recorder.RecordRoute(context.Background(), trace); err != nil {
		t.Fatalf("RecordRoute returned error: %v", err)
	}
	if err := recorder.RecordRoute(context.Background(), trace); err != nil {
		t.Fatalf("RecordRoute append returned error: %v", err)
	}

	lines := readJSONLLines(t, filepath.Join(home, "route-events.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("route-events.jsonl line count = %d, want 2", len(lines))
	}

	var got RouteTrace
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("route event is not valid JSON: %v; line=%s", err, lines[0])
	}
	if got.TaskType != trace.TaskType || got.AccountAlias != trace.AccountAlias || got.ModelAlias != trace.ModelAlias || got.Provider != trace.Provider {
		t.Fatalf("route event changed audit identity fields: got=%+v want=%+v", got, trace)
	}
	if got.ScoreBreakdown["latency"] != 20 || len(got.CandidateModelAliases) != 2 {
		t.Fatalf("route event did not preserve explainability fields: %+v", got)
	}
}

func TestJSONLRecorderRecordsTaskOutcome(t *testing.T) {
	home := t.TempDir()
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}

	outcome := TaskOutcome{
		TaskType:       "extract",
		AccountAlias:   "backup-account",
		ModelAlias:     "reliable-json",
		Provider:       "anthropic",
		Success:        false,
		ErrorCode:      "rate_limited",
		LatencyMillis:  1234,
		InputTokens:    300,
		OutputTokens:   120,
		EstimatedCents: 7,
	}
	if err := recorder.RecordOutcome(context.Background(), outcome); err != nil {
		t.Fatalf("RecordOutcome returned error: %v", err)
	}

	lines := readJSONLLines(t, filepath.Join(home, "outcomes.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("outcomes.jsonl line count = %d, want 1", len(lines))
	}

	var got TaskOutcome
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("task outcome is not valid JSON: %v; line=%s", err, lines[0])
	}
	if got.TaskType != outcome.TaskType || got.AccountAlias != outcome.AccountAlias || got.ModelAlias != outcome.ModelAlias || got.Provider != outcome.Provider {
		t.Fatalf("task outcome changed audit identity fields: got=%+v want=%+v", got, outcome)
	}
	if got.Success || got.ErrorCode != "rate_limited" || got.LatencyMillis != 1234 {
		t.Fatalf("task outcome did not preserve outcome fields: %+v", got)
	}
}

func TestJSONLRecorderDoesNotWriteAPIKeyFields(t *testing.T) {
	home := t.TempDir()
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}

	if err := recorder.RecordRoute(context.Background(), RouteTrace{
		TaskType:     "chat",
		AccountAlias: "account-openai-prod",
		ModelAlias:   "gpt-prod",
		Provider:     "openai",
		Reason:       "secret value sk-test-should-not-appear must not be copied from host state",
	}); err != nil {
		t.Fatalf("RecordRoute returned error: %v", err)
	}
	if err := recorder.RecordOutcome(context.Background(), TaskOutcome{
		TaskType:     "chat",
		AccountAlias: "account-openai-prod",
		ModelAlias:   "gpt-prod",
		Provider:     "openai",
		ErrorCode:    "invalid_api_key",
	}); err != nil {
		t.Fatalf("RecordOutcome returned error: %v", err)
	}

	for _, name := range []string{"route-events.jsonl", "outcomes.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatalf("ReadFile(%s) returned error: %v", name, err)
		}
		lower := strings.ToLower(string(raw))
		for _, forbidden := range []string{"api_key", "apikey", "secret_key", "bearer", "sk-test-should-not-appear"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s contains forbidden sensitive marker %q: %s", name, forbidden, string(raw))
			}
		}
	}
}

func readJSONLLines(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) returned error: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}
