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

func TestRefreshModelStatsUsesLatestOutcomePerRoute(t *testing.T) {
	home := t.TempDir()
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}
	trace := RouteTrace{
		RouteID:      "route-1",
		TaskID:       "task-1",
		TaskType:     "review",
		AccountAlias: "local",
		ModelAlias:   "local-small",
		Provider:     "local",
		Selected:     true,
	}
	if err := recorder.RecordRoute(context.Background(), trace); err != nil {
		t.Fatalf("RecordRoute returned error: %v", err)
	}
	if err := recorder.RecordOutcome(context.Background(), TaskOutcome{
		RouteID:      "route-1",
		TaskID:       "task-1",
		TaskType:     "review",
		AccountAlias: "local",
		ModelAlias:   "local-small",
		Provider:     "local",
		Success:      true,
		InputTokens:  10,
	}); err != nil {
		t.Fatalf("RecordOutcome provider returned error: %v", err)
	}
	if err := recorder.RecordOutcome(context.Background(), TaskOutcome{
		RouteID:         "route-1",
		TaskID:          "task-1",
		TaskType:        "review",
		AccountAlias:    "local",
		ModelAlias:      "local-small",
		Provider:        "local",
		Success:         true,
		BusinessOutcome: BusinessOutcomeSuccess,
		SuccessSignal:   SuccessSignalHumanAccepted,
		InputTokens:     10,
	}); err != nil {
		t.Fatalf("RecordOutcome business returned error: %v", err)
	}

	stats, err := RefreshModelStats(home)
	if err != nil {
		t.Fatalf("RefreshModelStats returned error: %v", err)
	}
	got := stats.Models["review|local|local-small|local"]
	if got.OutcomeCount != 1 || got.Successes != 1 || got.PendingOutcomes != 0 {
		t.Fatalf("stats = %+v, want latest outcome counted once", got)
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
	// Ten clean outcomes are smoothed toward the 5% prior: (0+0.5)/(10+10).
	if decision.ScoreBreakdown["reliability"] != -3 {
		t.Fatalf("selected cloud reliability = %d, want -3", decision.ScoreBreakdown["reliability"])
	}
	localScore := findCandidateScore(t, decision.Candidates, "local-balanced")
	if localScore.ScoreBreakdown["reliability"] >= 0 {
		t.Fatalf("local reliability score should be negative after stats enrichment: %+v", localScore.ScoreBreakdown)
	}
}

func TestApplyModelStatsFeedsEstimatedCostToBudgetConstraint(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.TaskType = "review"
	profile.MaxEstimatedCents = 5

	stats := ModelStats{
		Models: map[string]ModelStatsEntry{
			"review|cloud-account|cloud-balanced|openai": {
				TaskType:          "review",
				AccountAlias:      "cloud-account",
				ModelAlias:        "cloud-balanced",
				Provider:          "openai",
				OutcomeCount:      3,
				AvgEstimatedCents: 9,
			},
		},
	}
	cloud := candidate("cloud-balanced", CapabilityBalanced, PriceMedium, LatencyNormalClass, false)
	cloud.AccountAlias = "cloud-account"
	cloud.Model.Provider = "openai"
	local := candidate("local-balanced", CapabilityBalanced, PriceFree, LatencyFastClass, true)
	local.AccountAlias = "local-account"
	local.Model.Provider = "local"

	candidates := ApplyModelStats(stats, profile, []Candidate{cloud, local})
	decision, err := RoutePolicy{}.Select(profile, candidates)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if decision.SelectedAlias != "local-balanced" {
		t.Fatalf("SelectedAlias = %q, want local-balanced; decision=%+v", decision.SelectedAlias, decision)
	}
	if !candidateExcludedFor(decision.Candidates, "cloud-balanced", "estimated cost exceeds task budget") {
		t.Fatalf("cloud should be excluded by stats-derived estimated cost: %+v", decision.Candidates)
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

func recordFallbackRoute(t *testing.T, home string) {
	t.Helper()
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}
	attempts := []struct {
		attempt int
		model   string
		success bool
		latency int
		cents   int
	}{
		{attempt: 1, model: "local-small", success: false, latency: 0, cents: 0},
		{attempt: 2, model: "cloud-large", success: true, latency: 800, cents: 12},
	}
	for _, attempt := range attempts {
		trace := RouteTrace{RouteID: "route-fallback", TaskID: "task-1", Attempt: attempt.attempt, TaskType: "review", AccountAlias: "acct", ModelAlias: attempt.model, Provider: "p", Selected: true}
		if err := recorder.RecordRoute(context.Background(), trace); err != nil {
			t.Fatalf("RecordRoute returned error: %v", err)
		}
		if err := recorder.RecordOutcome(context.Background(), TaskOutcome{
			RouteID: "route-fallback", TaskID: "task-1", Attempt: attempt.attempt, TaskType: "review",
			AccountAlias: "acct", ModelAlias: attempt.model, Provider: "p", Success: attempt.success,
			LatencyMillis: attempt.latency, EstimatedCents: attempt.cents,
		}); err != nil {
			t.Fatalf("RecordOutcome returned error: %v", err)
		}
	}
}

func TestRefreshModelStatsCountsEveryFallbackAttempt(t *testing.T) {
	home := t.TempDir()
	recordFallbackRoute(t, home)

	stats, err := RefreshModelStats(home)
	if err != nil {
		t.Fatalf("RefreshModelStats returned error: %v", err)
	}
	failed := stats.Models["review|acct|local-small|p"]
	if failed.OutcomeCount != 1 || failed.Failures != 1 || failed.PendingOutcomes != 0 {
		t.Fatalf("failed attempt stats = %+v, want one recorded failure", failed)
	}
	succeeded := stats.Models["review|acct|cloud-large|p"]
	if succeeded.OutcomeCount != 1 || succeeded.Successes != 1 {
		t.Fatalf("fallback attempt stats = %+v, want one success", succeeded)
	}
}

func TestReadRouteAuditsJoinsOutcomePerAttempt(t *testing.T) {
	home := t.TempDir()
	recordFallbackRoute(t, home)

	records, err := ReadRouteAudits(home, AuditFilter{TaskID: "task-1"})
	if err != nil {
		t.Fatalf("ReadRouteAudits returned error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %+v, want two attempts", records)
	}
	for _, record := range records {
		if record.Outcome == nil || record.Outcome.ModelAlias != record.Route.ModelAlias {
			t.Fatalf("route %s attempt %d joined to %+v", record.Route.ModelAlias, record.Route.Attempt, record.Outcome)
		}
	}
}

func TestModelStatsAverageCostAndLatencyOnlyOverReportedSamples(t *testing.T) {
	home := t.TempDir()
	recordFallbackRoute(t, home)
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}
	if err := recorder.RecordOutcome(context.Background(), TaskOutcome{
		RouteID: "route-unpriced", TaskID: "task-2", Attempt: 1, TaskType: "review",
		AccountAlias: "acct", ModelAlias: "cloud-large", Provider: "p", Success: true,
	}); err != nil {
		t.Fatalf("RecordOutcome returned error: %v", err)
	}

	stats, err := BuildModelStats(home)
	if err != nil {
		t.Fatalf("BuildModelStats returned error: %v", err)
	}
	cloud := stats.Models["review|acct|cloud-large|p"]
	if cloud.OutcomeCount != 2 || cloud.AvgEstimatedCents != 12 || cloud.AvgLatencyMillis != 800 {
		t.Fatalf("cloud stats = %+v, want averages over the one reported sample", cloud)
	}

	profile := DefaultTaskProfile()
	profile.TaskType = "review"
	local := candidate("local-small", CapabilitySimple, PriceFree, LatencyFastClass, true)
	local.AccountAlias = "acct"
	local.Model.Provider = "p"
	local.Model.EstimatedCents = 3
	local.Model.LatencyClass = LatencyFastClass
	enriched := ApplyModelStats(*stats, profile, []Candidate{local})
	if enriched[0].Model.EstimatedCents != 3 || enriched[0].Model.RecentLatencyMillis != 0 {
		t.Fatalf("enriched = %+v, want configured cost kept without cost or latency samples", enriched[0].Model)
	}
}

func TestModelStatsWeightRecentOutcomesOverOldOnes(t *testing.T) {
	home := t.TempDir()
	recorder, err := NewJSONLRecorder(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := func(model string, n int, success bool, at time.Time) {
		for i := range n {
			if err := recorder.RecordOutcome(context.Background(), TaskOutcome{
				RouteID: model + at.String() + string(rune('a'+i)), Attempt: 1, RecordedAt: at,
				TaskType: "rewrite", AccountAlias: "acct", ModelAlias: model, Provider: "p", Success: success,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	fourWeeksAgo := now.Add(-4 * ModelStatsHalfLife)
	// recovered failed a lot four weeks ago and works now; degrading is the reverse.
	record("recovered", 20, false, fourWeeksAgo)
	record("recovered", 5, true, now.Add(-time.Hour))
	record("degrading", 20, true, fourWeeksAgo)
	record("degrading", 5, false, now.Add(-time.Hour))

	stats, err := BuildModelStats(home)
	if err != nil {
		t.Fatal(err)
	}
	profile := TaskProfile{TaskType: "rewrite"}
	candidates := ApplyModelStats(*stats, profile, []Candidate{
		{AccountAlias: "acct", Model: ModelCapability{Alias: "recovered", Provider: "p"}},
		{AccountAlias: "acct", Model: ModelCapability{Alias: "degrading", Provider: "p"}},
	})
	recovered, degrading := reliabilityScore(candidates[0].Model), reliabilityScore(candidates[1].Model)
	// Raw totals rank them the other way: 20/25 failures against 5/25.
	if recovered <= degrading {
		t.Fatalf("reliability recovered/degrading = %d/%d, want the recently healthy model ahead", recovered, degrading)
	}
	entry := stats.Models[modelStatsKey("rewrite", "acct", "recovered", "p")]
	if entry.OutcomeCount != 25 || entry.RecentFailures > 1.3 || entry.RecentOutcomes < 6 {
		t.Fatalf("recovered entry = %+v, want 25 raw outcomes with old failures decayed to 1/16", entry)
	}
}
