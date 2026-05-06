package llmkit

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestApplyProviderHealthSkipsQuotaExhaustedCooldownAndFullConcurrency(t *testing.T) {
	now := time.Date(2026, 5, 6, 3, 0, 0, 0, time.UTC)
	profile := DefaultTaskProfile()

	health := ProviderHealthSnapshot{
		Entries: map[string]ProviderHealthEntry{
			ProviderHealthKey("quota-account", "quota-model", "test"): {
				QuotaExhausted: true,
			},
			ProviderHealthKey("cooldown-account", "cooldown-model", "test"): {
				CooldownUntil: now.Add(time.Minute),
			},
			ProviderHealthKey("busy-account", "busy-model", "test"): {
				InFlight:       2,
				MaxConcurrency: 2,
			},
		},
	}

	decision, err := RoutePolicy{Now: func() time.Time { return now }}.Select(profile, ApplyProviderHealth(health, []Candidate{
		withAccount(candidate("quota-model", CapabilityBalanced, PriceLow, LatencyNormalClass, false), "quota-account"),
		withAccount(candidate("cooldown-model", CapabilityBalanced, PriceLow, LatencyNormalClass, false), "cooldown-account"),
		withAccount(candidate("busy-model", CapabilityBalanced, PriceLow, LatencyNormalClass, false), "busy-account"),
		candidate("available", CapabilityBalanced, PriceMedium, LatencyNormalClass, false),
	}))
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if decision.SelectedAlias != "available" {
		t.Fatalf("SelectedAlias = %q, want available; decision=%+v", decision.SelectedAlias, decision)
	}
	if !candidateExcludedFor(decision.Candidates, "quota-model", "provider quota is exhausted") {
		t.Fatalf("quota exhausted candidate should be excluded: %+v", decision.Candidates)
	}
	if !candidateExcludedFor(decision.Candidates, "cooldown-model", "provider is in cooldown") {
		t.Fatalf("cooldown candidate should be excluded: %+v", decision.Candidates)
	}
	if !candidateExcludedFor(decision.Candidates, "busy-model", "provider concurrency is full") {
		t.Fatalf("busy candidate should be excluded: %+v", decision.Candidates)
	}
}

func TestRoutePolicyPenalizesDegradedProviderButAllowsFallback(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.Complexity = ComplexitySimple
	profile.Latency = LatencyNone
	profile.FailureCost = FailureCostLow

	health := ProviderHealthSnapshot{
		Entries: map[string]ProviderHealthEntry{
			ProviderHealthKey("local-free-account", "local-free", "test"): {
				Availability:  ProviderDegraded,
				FailureStreak: 2,
			},
		},
	}

	decision, err := RoutePolicy{}.Select(profile, ApplyProviderHealth(health, []Candidate{
		candidate("local-free", CapabilitySimple, PriceFree, LatencyNormalClass, true),
		candidate("cloud-low", CapabilitySimple, PriceLow, LatencyNormalClass, false),
	}))
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if decision.SelectedAlias != "cloud-low" {
		t.Fatalf("SelectedAlias = %q, want cloud-low; decision=%+v", decision.SelectedAlias, decision)
	}
	if decision.ScoreBreakdown["health"] <= 0 {
		t.Fatalf("selected cloud candidate should get non-negative health score: %+v", decision.ScoreBreakdown)
	}
}

func TestRoutePolicyAllowsProviderAfterCooldownExpires(t *testing.T) {
	now := time.Date(2026, 5, 6, 3, 30, 0, 0, time.UTC)
	profile := DefaultTaskProfile()

	health := ProviderHealthSnapshot{
		Entries: map[string]ProviderHealthEntry{
			ProviderHealthKey("local-free-account", "local-free", "test"): {
				Availability:  ProviderUnavailable,
				CooldownUntil: now.Add(-time.Second),
				FailureStreak: 1,
			},
		},
	}

	decision, err := RoutePolicy{Now: func() time.Time { return now }}.Select(profile, ApplyProviderHealth(health, []Candidate{
		candidate("local-free", CapabilityBalanced, PriceFree, LatencyFastClass, true),
	}))
	if err != nil {
		t.Fatalf("Select returned error after expired cooldown: %v", err)
	}
	if decision.SelectedAlias != "local-free" {
		t.Fatalf("SelectedAlias = %q, want local-free; decision=%+v", decision.SelectedAlias, decision)
	}
}

func TestMemoryHealthStoreTracksInFlightAndOutcomeCooldown(t *testing.T) {
	now := time.Date(2026, 5, 6, 4, 0, 0, 0, time.UTC)
	store := NewMemoryHealthStore(HealthPolicy{
		FailureCooldownThreshold: 2,
		CooldownDuration:         5 * time.Minute,
		Now:                      func() time.Time { return now },
	})
	c := candidate("local-free", CapabilitySimple, PriceFree, LatencyNormalClass, true)

	if err := store.Begin(context.Background(), c); err != nil {
		t.Fatalf("Begin returned error: %v", err)
	}
	snapshot := store.Snapshot()
	entry := snapshot.Entries[ProviderHealthKey(c.AccountAlias, c.Model.Alias, c.Model.Provider)]
	if entry.InFlight != 1 {
		t.Fatalf("in flight after Begin = %d, want 1", entry.InFlight)
	}

	failed := TaskOutcome{
		AccountAlias: c.AccountAlias,
		ModelAlias:   c.Model.Alias,
		Provider:     c.Model.Provider,
		Success:      false,
	}
	if err := store.RecordOutcome(context.Background(), failed); err != nil {
		t.Fatalf("RecordOutcome first failure returned error: %v", err)
	}
	if err := store.RecordOutcome(context.Background(), failed); err != nil {
		t.Fatalf("RecordOutcome second failure returned error: %v", err)
	}
	entry = store.Snapshot().Entries[ProviderHealthKey(c.AccountAlias, c.Model.Alias, c.Model.Provider)]
	if entry.InFlight != 0 {
		t.Fatalf("in flight after outcomes = %d, want 0", entry.InFlight)
	}
	if entry.FailureStreak != 2 || entry.Availability != ProviderUnavailable {
		t.Fatalf("entry after failures = %+v, want failure streak 2 and unavailable", entry)
	}
	if !entry.CooldownUntil.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("cooldown until = %v, want %v", entry.CooldownUntil, now.Add(5*time.Minute))
	}

	success := TaskOutcome{
		AccountAlias: c.AccountAlias,
		ModelAlias:   c.Model.Alias,
		Provider:     c.Model.Provider,
		Success:      true,
	}
	if err := store.RecordOutcome(context.Background(), success); err != nil {
		t.Fatalf("RecordOutcome success returned error: %v", err)
	}
	entry = store.Snapshot().Entries[ProviderHealthKey(c.AccountAlias, c.Model.Alias, c.Model.Provider)]
	if entry.FailureStreak != 0 || entry.Availability != ProviderAvailable {
		t.Fatalf("entry after success = %+v, want reset available state", entry)
	}
}

func TestMemoryHealthStoreHonorsContextCancellation(t *testing.T) {
	store := NewMemoryHealthStore(HealthPolicy{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Begin(ctx, candidate("local", CapabilitySimple, PriceFree, LatencyNormalClass, true)); err != context.Canceled {
		t.Fatalf("Begin cancelled error = %v, want context.Canceled", err)
	}
	if err := store.RecordOutcome(ctx, TaskOutcome{AccountAlias: "a", ModelAlias: "m", Provider: "p"}); err != context.Canceled {
		t.Fatalf("RecordOutcome cancelled error = %v, want context.Canceled", err)
	}
}

func withAccount(c Candidate, accountAlias string) Candidate {
	c.AccountAlias = accountAlias
	return c
}

func candidateExcludedFor(candidates []CandidateScore, alias, reasonPart string) bool {
	for _, candidate := range candidates {
		if candidate.Alias == alias {
			return !candidate.Available && strings.Contains(candidate.Reason, reasonPart)
		}
	}
	return false
}
