package llmkit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
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

	if err := store.Acquire(context.Background(), c); err != nil {
		t.Fatalf("Acquire returned error: %v", err)
	}
	snapshot := store.Snapshot()
	entry := snapshot.Entries[ProviderHealthKey(c.AccountAlias, c.Model.Alias, c.Model.Provider)]
	if entry.InFlight != 1 {
		t.Fatalf("in flight after Acquire = %d, want 1", entry.InFlight)
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

	if err := store.Acquire(ctx, candidate("local", CapabilitySimple, PriceFree, LatencyNormalClass, true)); err != context.Canceled {
		t.Fatalf("Acquire cancelled error = %v, want context.Canceled", err)
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

func TestMemoryHealthStoreIgnoresCallerCausedFailures(t *testing.T) {
	store := NewMemoryHealthStore(HealthPolicy{FailureCooldownThreshold: 2, CooldownDuration: time.Minute})
	c := candidate("local-free", CapabilitySimple, PriceFree, LatencyNormalClass, true)
	key := ProviderHealthKey(c.AccountAlias, c.Model.Alias, c.Model.Provider)
	outcome := func(class ErrorClass) TaskOutcome {
		return TaskOutcome{AccountAlias: c.AccountAlias, ModelAlias: c.Model.Alias, Provider: c.Model.Provider, ErrorClass: class}
	}

	for _, class := range []ErrorClass{ErrorClassCanceled, ErrorClassRequestTooLarge, ErrorClassBudgetExceeded, ErrorClassCanceled} {
		if err := store.Acquire(context.Background(), c); err != nil {
			t.Fatalf("Acquire returned error: %v", err)
		}
		if err := store.RecordOutcome(context.Background(), outcome(class)); err != nil {
			t.Fatalf("RecordOutcome(%s) returned error: %v", class, err)
		}
	}
	entry := store.Snapshot().Entries[key]
	if entry.InFlight != 0 || entry.FailureStreak != 0 || entry.Availability != ProviderAvailable {
		t.Fatalf("entry after caller-caused failures = %+v, want available with no streak", entry)
	}

	for _, class := range []ErrorClass{ErrorClassTimeout, ErrorClassRateLimited} {
		if err := store.RecordOutcome(context.Background(), outcome(class)); err != nil {
			t.Fatalf("RecordOutcome(%s) returned error: %v", class, err)
		}
	}
	entry = store.Snapshot().Entries[key]
	if entry.FailureStreak != 2 || entry.Availability != ProviderUnavailable {
		t.Fatalf("entry after provider failures = %+v, want unavailable", entry)
	}
}

func TestMemoryHealthStoreBacksOffCooldownExponentially(t *testing.T) {
	now := time.Date(2026, 5, 6, 4, 0, 0, 0, time.UTC)
	store := NewMemoryHealthStore(HealthPolicy{
		FailureCooldownThreshold: 2,
		CooldownDuration:         time.Minute,
		MaxCooldownDuration:      5 * time.Minute,
		Now:                      func() time.Time { return now },
	})
	c := candidate("local-free", CapabilitySimple, PriceFree, LatencyNormalClass, true)
	key := ProviderHealthKey(c.AccountAlias, c.Model.Alias, c.Model.Provider)
	failed := TaskOutcome{AccountAlias: c.AccountAlias, ModelAlias: c.Model.Alias, Provider: c.Model.Provider, ErrorClass: ErrorClassTransient}

	want := []time.Duration{0, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute}
	for i, cooldown := range want {
		if err := store.RecordOutcome(context.Background(), failed); err != nil {
			t.Fatalf("RecordOutcome returned error: %v", err)
		}
		entry := store.Snapshot().Entries[key]
		if cooldown == 0 {
			if !entry.CooldownUntil.IsZero() {
				t.Fatalf("failure %d cooldown = %v, want none below threshold", i+1, entry.CooldownUntil)
			}
			continue
		}
		if !entry.CooldownUntil.Equal(now.Add(cooldown)) {
			t.Fatalf("failure %d cooldown until = %v, want %v", i+1, entry.CooldownUntil, now.Add(cooldown))
		}
	}
}

func concurrentAcquires(store HealthStore, c Candidate, callers int) (admitted, busy int32) {
	var admittedCount, busyCount atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch err := store.Acquire(context.Background(), c); {
			case err == nil:
				admittedCount.Add(1)
			case errors.Is(err, ErrProviderBusy):
				busyCount.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	return admittedCount.Load(), busyCount.Load()
}

func TestMemoryHealthStoreAcquireEnforcesConfiguredConcurrency(t *testing.T) {
	store := NewMemoryHealthStore(HealthPolicy{})
	c := candidate("local", CapabilitySimple, PriceFree, LatencyNormalClass, true)
	c.Model.MaxConcurrency = 2

	if admitted, busy := concurrentAcquires(store, c, 20); admitted != 2 || busy != 18 {
		t.Fatalf("admitted/busy = %d/%d, want 2/18", admitted, busy)
	}
	if err := store.Release(context.Background(), c); err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if err := store.Acquire(context.Background(), c); err != nil {
		t.Fatalf("Acquire after Release = %v, want admitted", err)
	}
}

func TestMemoryHealthStoreAcquireEnforcesAccountConcurrency(t *testing.T) {
	store := NewMemoryHealthStore(HealthPolicy{})
	first := withAccount(candidate("model-a", CapabilitySimple, PriceFree, LatencyNormalClass, true), "shared")
	second := withAccount(candidate("model-b", CapabilitySimple, PriceFree, LatencyNormalClass, true), "shared")
	first.AccountMaxConcurrency, second.AccountMaxConcurrency = 1, 1

	if err := store.Acquire(context.Background(), first); err != nil {
		t.Fatalf("Acquire(first) = %v", err)
	}
	if err := store.Acquire(context.Background(), second); !errors.Is(err, ErrProviderBusy) || !strings.Contains(err.Error(), "account concurrency") {
		t.Fatalf("Acquire(second) = %v, want account concurrency rejection", err)
	}
}

func TestMemoryHealthStoreHalfOpenAdmitsExactlyOneProbe(t *testing.T) {
	now := time.Date(2026, 5, 6, 4, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	store := NewMemoryHealthStore(HealthPolicy{
		FailureCooldownThreshold: 1,
		CooldownDuration:         time.Minute,
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
	})
	c := candidate("local", CapabilitySimple, PriceFree, LatencyNormalClass, true)
	c.Model.MaxConcurrency = 0
	failed := TaskOutcome{AccountAlias: c.AccountAlias, ModelAlias: c.Model.Alias, Provider: c.Model.Provider, ErrorClass: ErrorClassTimeout}
	if err := store.Acquire(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOutcome(context.Background(), failed); err != nil {
		t.Fatal(err)
	}

	if err := store.Acquire(context.Background(), c); !errors.Is(err, ErrProviderBusy) || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("Acquire during cooldown = %v, want cooldown rejection", err)
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	if admitted, busy := concurrentAcquires(store, c, 20); admitted != 1 || busy != 19 {
		t.Fatalf("half-open admitted/busy = %d/%d, want exactly one probe", admitted, busy)
	}
}
