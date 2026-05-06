package llmkit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJSONLRecorderRecordsRouteEvent(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nested", "llmkit")
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}

	trace := RouteTrace{
		RouteID:               "route-123",
		TaskID:                "task-456",
		Attempt:               2,
		RecordedAt:            time.Date(2026, 5, 4, 10, 30, 0, 0, time.UTC),
		TaskType:              "summarize",
		TaskProfile:           &TaskProfile{TaskType: "summarize", Complexity: ComplexityHard, FailureCost: FailureCostHigh, Privacy: PrivacyCloudAllowed, NeedsReasoning: true},
		AccountAlias:          "primary-account",
		ModelAlias:            "fast-json",
		Provider:              "openai",
		Selected:              true,
		Reason:                "lowest latency available",
		Score:                 42,
		ScoreBreakdown:        map[string]int{"latency": 20, "price": 10},
		CandidateModelAliases: []string{"fast-json", "local-small"},
		Candidates: []CandidateScore{
			{
				Alias:          "fast-json",
				AccountAlias:   "primary-account",
				Available:      true,
				Score:          42,
				ScoreBreakdown: map[string]int{"latency": 20, "price": 10},
				Reason:         "lowest latency available",
			},
			{
				Alias:        "local-small",
				AccountAlias: "local-account",
				Available:    false,
				Reason:       "model does not match task requirements",
			},
		},
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
	if got.RouteID != trace.RouteID || got.TaskID != trace.TaskID || got.Attempt != trace.Attempt || !got.RecordedAt.Equal(trace.RecordedAt) {
		t.Fatalf("route event changed correlation fields: got=%+v want=%+v", got, trace)
	}
	if got.TaskProfile == nil || got.TaskProfile.Complexity != ComplexityHard || !got.TaskProfile.NeedsReasoning {
		t.Fatalf("route event did not preserve task profile: %+v", got.TaskProfile)
	}
	if got.ScoreBreakdown["latency"] != 20 || len(got.CandidateModelAliases) != 2 {
		t.Fatalf("route event did not preserve explainability fields: %+v", got)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("route event candidates len = %d, want 2: %+v", len(got.Candidates), got.Candidates)
	}
	if got.Candidates[0].Alias != "fast-json" || got.Candidates[0].ScoreBreakdown["price"] != 10 {
		t.Fatalf("selected candidate explanation not preserved: %+v", got.Candidates[0])
	}
	if got.Candidates[1].Available || got.Candidates[1].Reason == "" {
		t.Fatalf("filtered candidate explanation not preserved: %+v", got.Candidates[1])
	}
}

func TestJSONLRecorderRecordsTaskOutcome(t *testing.T) {
	home := t.TempDir()
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}

	outcome := TaskOutcome{
		RouteID:         "route-123",
		TaskID:          "task-456",
		Attempt:         2,
		RecordedAt:      time.Date(2026, 5, 4, 10, 31, 0, 0, time.UTC),
		TaskType:        "extract",
		AccountAlias:    "backup-account",
		ModelAlias:      "reliable-json",
		Provider:        "anthropic",
		Success:         false,
		BusinessOutcome: BusinessOutcomeFailure,
		SuccessSignal:   SuccessSignalHumanAccepted,
		FailureReason:   "operator rejected",
		ErrorCode:       "rate_limited",
		LatencyMillis:   1234,
		InputTokens:     300,
		OutputTokens:    120,
		EstimatedCents:  7,
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
	if got.RouteID != outcome.RouteID || got.TaskID != outcome.TaskID || got.Attempt != outcome.Attempt || !got.RecordedAt.Equal(outcome.RecordedAt) {
		t.Fatalf("task outcome changed correlation fields: got=%+v want=%+v", got, outcome)
	}
	if got.Success || got.ErrorCode != "rate_limited" || got.LatencyMillis != 1234 {
		t.Fatalf("task outcome did not preserve outcome fields: %+v", got)
	}
	if got.BusinessOutcome != BusinessOutcomeFailure || got.SuccessSignal != SuccessSignalHumanAccepted || got.FailureReason != "operator rejected" {
		t.Fatalf("task outcome did not preserve business outcome fields: %+v", got)
	}
}

func TestJSONLRecorderRestrictsDirectoryAndLogFilePermissions(t *testing.T) {
	home := filepath.Join(t.TempDir(), "llmkit")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	for _, name := range []string{"route-events.jsonl", "outcomes.jsonl"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte{}, 0o644); err != nil {
			t.Fatalf("WriteFile(%s) returned error: %v", name, err)
		}
	}

	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}
	if err := recorder.RecordRoute(context.Background(), RouteTrace{RouteID: "route-1", TaskID: "task-1"}); err != nil {
		t.Fatalf("RecordRoute returned error: %v", err)
	}
	if err := recorder.RecordOutcome(context.Background(), TaskOutcome{RouteID: "route-1", TaskID: "task-1"}); err != nil {
		t.Fatalf("RecordOutcome returned error: %v", err)
	}

	assertMode(t, home, 0o700)
	assertMode(t, filepath.Join(home, "route-events.jsonl"), 0o600)
	assertMode(t, filepath.Join(home, "outcomes.jsonl"), 0o600)
}

func TestJSONLRecorderDoesNotWriteAPIKeyFields(t *testing.T) {
	home := t.TempDir()
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}

	if err := recorder.RecordRoute(context.Background(), RouteTrace{
		RouteID:      "route-secret",
		TaskID:       "task-secret",
		TaskType:     "chat",
		AccountAlias: "account-openai-prod",
		ModelAlias:   "gpt-prod",
		Provider:     "openai",
		Reason:       "secret value sk-test-should-not-appear must not be copied from host state",
	}); err != nil {
		t.Fatalf("RecordRoute returned error: %v", err)
	}
	if err := recorder.RecordOutcome(context.Background(), TaskOutcome{
		RouteID:      "route-secret",
		TaskID:       "task-secret",
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
		for _, forbidden := range []string{"secret_key", "bearer", "sk-test-should-not-appear"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s contains forbidden sensitive marker %q: %s", name, forbidden, string(raw))
			}
		}

		var fields map[string]any
		if err := json.Unmarshal(bytesBeforeNewline(raw), &fields); err != nil {
			t.Fatalf("%s first line is not a JSON object: %v; raw=%s", name, err, string(raw))
		}
		for _, forbidden := range []string{"api_key", "apiKey", "token", "authorization", "headers", "secret"} {
			if _, ok := fields[forbidden]; ok {
				t.Fatalf("%s contains forbidden top-level credential field %q: %v", name, forbidden, fields)
			}
		}
	}

	lines := readJSONLLines(t, filepath.Join(home, "outcomes.jsonl"))
	var outcome TaskOutcome
	if err := json.Unmarshal([]byte(lines[0]), &outcome); err != nil {
		t.Fatalf("outcome is not valid JSON: %v", err)
	}
	if outcome.ErrorCode != "invalid_api_key" {
		t.Fatalf("ErrorCode = %q, want invalid_api_key", outcome.ErrorCode)
	}
}

func TestReadRouteAuditsJoinsRoutesAndOutcomes(t *testing.T) {
	home := t.TempDir()
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}

	if err := recorder.RecordRoute(context.Background(), RouteTrace{
		RouteID:               "route-wf-1",
		TaskID:                "wf-1",
		Attempt:               1,
		TaskType:              "host_api_review",
		TaskProfile:           &TaskProfile{TaskType: "host_api_review", Complexity: ComplexitySimple, FailureCost: FailureCostLow, Privacy: PrivacyLocalPreferred},
		AccountAlias:          "local-dev",
		ModelAlias:            "local-free",
		Provider:              "local",
		Selected:              true,
		Reason:                "selected local-free",
		Score:                 70,
		ScoreBreakdown:        map[string]int{"price": 30, "local": 15},
		CandidateModelAliases: []string{"local-free", "cloud-advanced"},
	}); err != nil {
		t.Fatalf("RecordRoute returned error: %v", err)
	}
	if err := recorder.RecordOutcome(context.Background(), TaskOutcome{
		RouteID:        "route-wf-1",
		TaskID:         "wf-1",
		Attempt:        1,
		TaskType:       "host_api_review",
		AccountAlias:   "local-dev",
		ModelAlias:     "local-free",
		Provider:       "local",
		Success:        true,
		LatencyMillis:  12,
		InputTokens:    5,
		OutputTokens:   7,
		EstimatedCents: 0,
	}); err != nil {
		t.Fatalf("RecordOutcome returned error: %v", err)
	}

	records, err := ReadRouteAudits(home, AuditFilter{TaskID: "wf-1"})
	if err != nil {
		t.Fatalf("ReadRouteAudits returned error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records len = %d, want 1: %+v", len(records), records)
	}
	got := records[0]
	if got.Route.RouteID != "route-wf-1" || got.Route.ModelAlias != "local-free" {
		t.Fatalf("route record = %+v, want local-free route", got.Route)
	}
	if got.Route.TaskProfile == nil || got.Route.TaskProfile.Privacy != PrivacyLocalPreferred {
		t.Fatalf("route profile = %+v, want local preferred profile", got.Route.TaskProfile)
	}
	if got.Outcome == nil || !got.Outcome.Success || got.Outcome.LatencyMillis != 12 {
		t.Fatalf("outcome = %+v, want joined successful outcome", got.Outcome)
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

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) returned error: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %#o, want %#o", path, got, want)
	}
}

func bytesBeforeNewline(raw []byte) []byte {
	if index := strings.IndexByte(string(raw), '\n'); index >= 0 {
		return raw[:index]
	}
	return raw
}
