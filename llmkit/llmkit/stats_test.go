package llmkit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRefreshModelStatsAggregatesOutcomesAndPendingRoutes(t *testing.T) {
	home := t.TempDir()
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}

	now := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	records := []struct {
		trace   RouteTrace
		outcome *TaskOutcome
	}{
		{
			trace: RouteTrace{
				RouteID:      "route-local-1",
				TaskID:       "task-1",
				Attempt:      1,
				RecordedAt:   now,
				TaskType:     "rewrite",
				AccountAlias: "local",
				ModelAlias:   "local-small",
				Provider:     "local",
				Selected:     true,
			},
			outcome: &TaskOutcome{
				RouteID:       "route-local-1",
				TaskID:        "task-1",
				Attempt:       1,
				RecordedAt:    now.Add(200 * time.Millisecond),
				TaskType:      "rewrite",
				AccountAlias:  "local",
				ModelAlias:    "local-small",
				Provider:      "local",
				Success:       true,
				LatencyMillis: 800,
				InputTokens:   100,
				OutputTokens:  20,
			},
		},
		{
			trace: RouteTrace{
				RouteID:      "route-local-2",
				TaskID:       "task-2",
				Attempt:      1,
				RecordedAt:   now.Add(time.Minute),
				TaskType:     "rewrite",
				AccountAlias: "local",
				ModelAlias:   "local-small",
				Provider:     "local",
				Selected:     true,
			},
			outcome: &TaskOutcome{
				RouteID:       "route-local-2",
				TaskID:        "task-2",
				Attempt:       1,
				RecordedAt:    now.Add(time.Minute + 150*time.Millisecond),
				TaskType:      "rewrite",
				AccountAlias:  "local",
				ModelAlias:    "local-small",
				Provider:      "local",
				Success:       false,
				ErrorCode:     "timeout",
				LatencyMillis: 1200,
				InputTokens:   140,
				OutputTokens:  0,
			},
		},
		{
			trace: RouteTrace{
				RouteID:      "route-local-3",
				TaskID:       "task-3",
				Attempt:      1,
				RecordedAt:   now.Add(2 * time.Minute),
				TaskType:     "rewrite",
				AccountAlias: "local",
				ModelAlias:   "local-small",
				Provider:     "local",
				Selected:     true,
			},
		},
		{
			trace: RouteTrace{
				RouteID:      "route-pro-1",
				TaskID:       "task-4",
				Attempt:      1,
				RecordedAt:   now.Add(3 * time.Minute),
				TaskType:     "reasoning",
				AccountAlias: "openai-prod",
				ModelAlias:   "gpt-pro",
				Provider:     "openai",
				Selected:     true,
			},
			outcome: &TaskOutcome{
				RouteID:        "route-pro-1",
				TaskID:         "task-4",
				Attempt:        1,
				RecordedAt:     now.Add(3*time.Minute + 900*time.Millisecond),
				TaskType:       "reasoning",
				AccountAlias:   "openai-prod",
				ModelAlias:     "gpt-pro",
				Provider:       "openai",
				Success:        true,
				LatencyMillis:  2500,
				InputTokens:    1000,
				OutputTokens:   400,
				EstimatedCents: 11,
			},
		},
	}

	for _, record := range records {
		if err := recorder.RecordRoute(context.Background(), record.trace); err != nil {
			t.Fatalf("RecordRoute returned error: %v", err)
		}
		if record.outcome != nil {
			if err := recorder.RecordOutcome(context.Background(), *record.outcome); err != nil {
				t.Fatalf("RecordOutcome returned error: %v", err)
			}
		}
	}

	stats, err := RefreshModelStats(home)
	if err != nil {
		t.Fatalf("RefreshModelStats returned error: %v", err)
	}

	if stats.GeneratedAt.IsZero() {
		t.Fatalf("GeneratedAt was not set")
	}
	if got := len(stats.Models); got != 2 {
		t.Fatalf("len(Models) = %d, want 2; stats=%+v", got, stats.Models)
	}

	local := stats.Models["rewrite|local|local-small|local"]
	if local.TaskType != "rewrite" || local.AccountAlias != "local" || local.ModelAlias != "local-small" || local.Provider != "local" {
		t.Fatalf("local identity fields were not preserved: %+v", local)
	}
	if local.RouteAttempts != 3 || local.OutcomeCount != 2 || local.PendingOutcomes != 1 {
		t.Fatalf("local attempt counts = route:%d outcomes:%d pending:%d, want 3/2/1", local.RouteAttempts, local.OutcomeCount, local.PendingOutcomes)
	}
	if local.Successes != 1 || local.Failures != 1 || local.SuccessRate != 0.5 || local.FailureRate != 0.5 {
		t.Fatalf("local success/failure stats not aggregated correctly: %+v", local)
	}
	if local.AvgLatencyMillis != 1000 || local.AvgInputTokens != 120 || local.AvgOutputTokens != 10 {
		t.Fatalf("local averages not aggregated correctly: %+v", local)
	}
	if local.LastSeen == nil || !local.LastSeen.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("local LastSeen = %v, want pending route time", local.LastSeen)
	}

	pro := stats.Models["reasoning|openai-prod|gpt-pro|openai"]
	if pro.RouteAttempts != 1 || pro.OutcomeCount != 1 || pro.SuccessRate != 1 || pro.AvgEstimatedCents != 11 {
		t.Fatalf("pro stats not aggregated correctly: %+v", pro)
	}

	raw, err := os.ReadFile(filepath.Join(home, modelStatsFile))
	if err != nil {
		t.Fatalf("ReadFile(model-stats.json) returned error: %v", err)
	}
	var fromDisk ModelStats
	if err := json.Unmarshal(raw, &fromDisk); err != nil {
		t.Fatalf("model-stats.json is not valid JSON: %v; raw=%s", err, string(raw))
	}
	if fromDisk.Models["rewrite|local|local-small|local"].PendingOutcomes != 1 {
		t.Fatalf("model-stats.json did not persist aggregated stats: %+v", fromDisk)
	}
}

func TestApplyModelStatsMakesPolicyPreferReliableModelForHighFailureCostTask(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.TaskType = "rewrite"
	profile.FailureCost = FailureCostHigh

	stats := ModelStats{
		Models: map[string]ModelStatsEntry{
			"rewrite|local-account|local-balanced|local": {
				TaskType:         "rewrite",
				AccountAlias:     "local-account",
				ModelAlias:       "local-balanced",
				Provider:         "local",
				OutcomeCount:     10,
				Failures:         8,
				FailureRate:      0.8,
				AvgLatencyMillis: 200,
			},
			"rewrite|cloud-account|cloud-balanced|openai": {
				TaskType:         "rewrite",
				AccountAlias:     "cloud-account",
				ModelAlias:       "cloud-balanced",
				Provider:         "openai",
				OutcomeCount:     10,
				Failures:         0,
				FailureRate:      0,
				AvgLatencyMillis: 900,
			},
		},
	}
	local := candidate("local-balanced", CapabilityBalanced, PriceFree, LatencyFastClass, true)
	local.AccountAlias = "local-account"
	local.Model.Provider = "local"
	cloud := candidate("cloud-balanced", CapabilityBalanced, PriceLow, LatencyNormalClass, false)
	cloud.AccountAlias = "cloud-account"
	cloud.Model.Provider = "openai"

	candidates := ApplyModelStats(stats, profile, []Candidate{local, cloud})
	decision, err := RoutePolicy{}.Select(profile, candidates)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}

	if decision.SelectedAlias != "cloud-balanced" {
		t.Fatalf("SelectedAlias = %q, want cloud-balanced; decision=%+v", decision.SelectedAlias, decision)
	}
	if decision.ScoreBreakdown["reliability"] != 0 {
		t.Fatalf("selected cloud reliability = %d, want 0", decision.ScoreBreakdown["reliability"])
	}
	localScore := findCandidateScore(t, decision.Candidates, "local-balanced")
	if localScore.ScoreBreakdown["reliability"] >= 0 {
		t.Fatalf("local reliability score should be negative after stats enrichment: %+v", localScore.ScoreBreakdown)
	}
}

func TestApplyModelStatsKeepsLocalPreferenceForSimpleTaskWithAcceptableHistory(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.TaskType = "rewrite"
	profile.Complexity = ComplexitySimple
	profile.Latency = LatencyNone
	profile.FailureCost = FailureCostLow
	profile.Privacy = PrivacyLocalPreferred

	stats := ModelStats{
		Models: map[string]ModelStatsEntry{
			"rewrite|local-account|local-simple|local": {
				TaskType:         "rewrite",
				AccountAlias:     "local-account",
				ModelAlias:       "local-simple",
				Provider:         "local",
				OutcomeCount:     20,
				Failures:         2,
				FailureRate:      0.1,
				AvgLatencyMillis: 250,
			},
		},
	}
	local := candidate("local-simple", CapabilitySimple, PriceFree, LatencyNormalClass, true)
	local.AccountAlias = "local-account"
	local.Model.Provider = "local"
	cloud := candidate("cloud-balanced", CapabilityBalanced, PriceLow, LatencyNormalClass, false)
	cloud.AccountAlias = "cloud-account"
	cloud.Model.Provider = "openai"

	candidates := ApplyModelStats(stats, profile, []Candidate{local, cloud})
	decision, err := RoutePolicy{}.Select(profile, candidates)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}

	if decision.SelectedAlias != "local-simple" {
		t.Fatalf("SelectedAlias = %q, want local-simple; decision=%+v", decision.SelectedAlias, decision)
	}
	if decision.ScoreBreakdown["reliability"] >= 0 {
		t.Fatalf("selected local should carry a small history penalty: %+v", decision.ScoreBreakdown)
	}
}

func TestLoadModelStatsReadsGeneratedStatsFile(t *testing.T) {
	home := t.TempDir()
	written := ModelStats{
		GeneratedAt: time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC),
		Models: map[string]ModelStatsEntry{
			"rewrite|local|local-small|local": {
				TaskType:      "rewrite",
				AccountAlias:  "local",
				ModelAlias:    "local-small",
				Provider:      "local",
				OutcomeCount:  4,
				Failures:      1,
				FailureRate:   0.25,
				SuccessRate:   0.75,
				RouteAttempts: 4,
			},
		},
	}
	if err := WriteModelStats(home, written); err != nil {
		t.Fatalf("WriteModelStats returned error: %v", err)
	}

	loaded, err := LoadModelStats(home)
	if err != nil {
		t.Fatalf("LoadModelStats returned error: %v", err)
	}
	got := loaded.Models["rewrite|local|local-small|local"]
	if got.FailureRate != 0.25 || got.SuccessRate != 0.75 || got.RouteAttempts != 4 {
		t.Fatalf("loaded stats entry changed: %+v", got)
	}
}

func findCandidateScore(t *testing.T, scores []CandidateScore, alias string) CandidateScore {
	t.Helper()
	for _, score := range scores {
		if score.Alias == alias {
			return score
		}
	}
	t.Fatalf("candidate score %q not found in %+v", alias, scores)
	return CandidateScore{}
}
