// Package healthtest checks that an llmkit.HealthStore implementation admits
// calls and opens its circuit the way llmkit.MemoryHealthStore does.
package healthtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eruca/goagents/llmkit"
)

// NewHealthStore returns an empty store that reads time from policy.Now, and
// a seed function that stores an entry the way MemoryHealthStore.Set does:
// every health field but InFlight, which a store derives from its admitted
// calls. Stores sharing a database must give each call an isolated namespace.
type NewHealthStore func(t *testing.T, policy llmkit.HealthPolicy) (llmkit.HealthStore, func(llmkit.ProviderHealthEntry))

// RunHealthStoreConformance checks the admission and circuit-breaker
// behavior every llmkit.HealthStore must share: provider and account
// concurrency limits, the single half-open probe, exponential cooldown
// backoff, degraded recovery, ignoring caller-caused failures, and context
// cancellation. Each subtest uses a fresh store and a fake clock.
func RunHealthStoreConformance(t *testing.T, newStore NewHealthStore) {
	t.Helper()
	ctx := context.Background()

	t.Run("tracks in-flight calls and opens the circuit", func(t *testing.T) {
		clock := newClock()
		store, _ := newStore(t, clock.policy(2, 5*time.Minute, 0))
		c := testCandidate("acct", "model", 0)
		key := keyOf(c)

		mustAcquire(t, store, c)
		mustAcquire(t, store, c)
		if entry := store.Snapshot().Entries[key]; entry.InFlight != 2 {
			t.Fatalf("in flight after two Acquires = %d, want 2", entry.InFlight)
		}
		mustRecord(t, store, failure(c, llmkit.ErrorClassTimeout))
		if entry := store.Snapshot().Entries[key]; entry.InFlight != 1 || entry.Availability != llmkit.ProviderDegraded || entry.FailureStreak != 1 {
			t.Fatalf("entry after one failure = %+v, want degraded, streak 1, one in flight", entry)
		}
		mustRecord(t, store, failure(c, llmkit.ErrorClassTimeout))
		entry := store.Snapshot().Entries[key]
		if entry.InFlight != 0 || entry.FailureStreak != 2 || entry.Availability != llmkit.ProviderUnavailable {
			t.Fatalf("entry after two failures = %+v, want unavailable, streak 2, none in flight", entry)
		}
		if !entry.CooldownUntil.Equal(clock.now().Add(5 * time.Minute)) {
			t.Fatalf("cooldown until = %v, want %v", entry.CooldownUntil, clock.now().Add(5*time.Minute))
		}
		if entry.AccountAlias != "acct" || entry.ModelAlias != "model" || entry.Provider != "test" {
			t.Fatalf("entry identity = %q/%q/%q, want acct/model/test", entry.AccountAlias, entry.ModelAlias, entry.Provider)
		}

		clock.advance(5 * time.Minute)
		mustAcquire(t, store, c)
		mustRecord(t, store, success(c))
		entry = store.Snapshot().Entries[key]
		if entry.InFlight != 0 || entry.FailureStreak != 0 || entry.Availability != llmkit.ProviderAvailable || !entry.CooldownUntil.IsZero() {
			t.Fatalf("entry after a successful probe = %+v, want available with no streak or cooldown", entry)
		}
	})

	t.Run("release gives back the slot", func(t *testing.T) {
		clock := newClock()
		store, _ := newStore(t, clock.policy(0, 0, 0))
		c := testCandidate("acct", "model", 1)

		mustAcquire(t, store, c)
		if err := store.Release(ctx, c); err != nil {
			t.Fatalf("Release = %v", err)
		}
		if entry := store.Snapshot().Entries[keyOf(c)]; entry.InFlight != 0 || entry.FailureStreak != 0 {
			t.Fatalf("entry after Release = %+v, want nothing in flight and no streak", entry)
		}
		mustAcquire(t, store, c)
	})

	t.Run("provider concurrency limit", func(t *testing.T) {
		clock := newClock()
		store, _ := newStore(t, clock.policy(0, 0, 0))
		c := testCandidate("acct", "model", 2)

		if admitted, busy := ConcurrentAcquires(store, c, 20); admitted != 2 || busy != 18 {
			t.Fatalf("admitted/busy = %d/%d, want 2/18", admitted, busy)
		}
		if entry := store.Snapshot().Entries[keyOf(c)]; entry.InFlight != 2 {
			t.Fatalf("in flight after rejected Acquires = %d, want 2", entry.InFlight)
		}
		if err := store.Release(ctx, c); err != nil {
			t.Fatalf("Release = %v", err)
		}
		mustAcquire(t, store, c)
		wantBusy(t, store, c, "provider concurrency")
	})

	t.Run("entry max concurrency overrides the candidate's", func(t *testing.T) {
		clock := newClock()
		store, seed := newStore(t, clock.policy(0, 0, 0))
		c := testCandidate("acct", "model", 5)
		seed(llmkit.ProviderHealthEntry{AccountAlias: "acct", ModelAlias: "model", Provider: "test", MaxConcurrency: 1})

		mustAcquire(t, store, c)
		wantBusy(t, store, c, "provider concurrency")
	})

	t.Run("account concurrency spans its keys", func(t *testing.T) {
		clock := newClock()
		store, _ := newStore(t, clock.policy(0, 0, 0))
		first, second, other := testCandidate("shared", "model-a", 0), testCandidate("shared", "model-b", 0), testCandidate("other", "model-a", 0)
		first.AccountMaxConcurrency, second.AccountMaxConcurrency, other.AccountMaxConcurrency = 2, 2, 2

		mustAcquire(t, store, first)
		mustAcquire(t, store, first)
		wantBusy(t, store, second, "account concurrency")
		mustAcquire(t, store, other)
		mustRecord(t, store, success(first))
		mustAcquire(t, store, second)
		wantBusy(t, store, first, "account concurrency")
	})

	t.Run("host-set unavailability and exhausted quota reject calls", func(t *testing.T) {
		clock := newClock()
		store, seed := newStore(t, clock.policy(0, 0, 0))
		down, exhausted, overdrawn := testCandidate("acct", "down", 0), testCandidate("acct", "exhausted", 0), testCandidate("acct", "overdrawn", 0)
		seed(llmkit.ProviderHealthEntry{AccountAlias: "acct", ModelAlias: "down", Provider: "test", Availability: llmkit.ProviderUnavailable})
		seed(llmkit.ProviderHealthEntry{AccountAlias: "acct", ModelAlias: "exhausted", Provider: "test", QuotaExhausted: true})
		seed(llmkit.ProviderHealthEntry{AccountAlias: "acct", ModelAlias: "overdrawn", Provider: "test", QuotaRemaining: -1})

		wantBusy(t, store, down, "unavailable")
		wantBusy(t, store, exhausted, "quota")
		wantBusy(t, store, overdrawn, "quota")
		if entry := store.Snapshot().Entries[keyOf(down)]; entry.Availability != llmkit.ProviderUnavailable || entry.InFlight != 0 {
			t.Fatalf("rejected entry = %+v, want it unchanged", entry)
		}
	})

	t.Run("half-open admits exactly one probe", func(t *testing.T) {
		clock := newClock()
		store, _ := newStore(t, clock.policy(1, time.Minute, 0))
		c := testCandidate("acct", "model", 0)

		mustAcquire(t, store, c)
		mustRecord(t, store, failure(c, llmkit.ErrorClassTimeout))
		wantBusy(t, store, c, "cooldown")

		clock.advance(2 * time.Minute)
		if admitted, busy := ConcurrentAcquires(store, c, 20); admitted != 1 || busy != 19 {
			t.Fatalf("half-open admitted/busy = %d/%d, want exactly one probe", admitted, busy)
		}
		wantBusy(t, store, c, "probe")
	})

	t.Run("cooldown backs off exponentially", func(t *testing.T) {
		clock := newClock()
		store, _ := newStore(t, clock.policy(2, time.Minute, 5*time.Minute))
		c := testCandidate("acct", "model", 0)
		key := keyOf(c)

		want := []time.Duration{0, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
		for i, cooldown := range want {
			mustAcquire(t, store, c)
			mustRecord(t, store, failure(c, llmkit.ErrorClassTransient))
			entry := store.Snapshot().Entries[key]
			if entry.FailureStreak != i+1 {
				t.Fatalf("failure %d streak = %d, want %d", i+1, entry.FailureStreak, i+1)
			}
			if cooldown == 0 {
				if !entry.CooldownUntil.IsZero() {
					t.Fatalf("failure %d cooldown = %v, want none below threshold", i+1, entry.CooldownUntil)
				}
				continue
			}
			if !entry.CooldownUntil.Equal(clock.now().Add(cooldown)) {
				t.Fatalf("failure %d cooldown until = %v, want %v", i+1, entry.CooldownUntil, clock.now().Add(cooldown))
			}
			// The next failure is the recovery probe once the cooldown passed.
			clock.advance(cooldown)
		}
	})

	t.Run("degraded provider recovers after a quiet period", func(t *testing.T) {
		clock := newClock()
		store, _ := newStore(t, clock.policy(3, time.Minute, 0))
		c := testCandidate("acct", "model", 0)
		key := keyOf(c)
		fail := func() {
			t.Helper()
			mustAcquire(t, store, c)
			mustRecord(t, store, failure(c, llmkit.ErrorClassTimeout))
		}

		fail()
		fail()
		if entry := store.Snapshot().Entries[key]; entry.Availability != llmkit.ProviderDegraded || entry.FailureStreak != 2 {
			t.Fatalf("entry after two failures = %+v, want degraded with streak 2", entry)
		}
		clock.advance(time.Minute)
		if entry := store.Snapshot().Entries[key]; entry.Availability != llmkit.ProviderAvailable || entry.FailureStreak != 0 {
			t.Fatalf("entry after a quiet minute = %+v, want available with no streak", entry)
		}
		// A failure long after the earlier ones starts a new streak instead
		// of opening the circuit.
		fail()
		if entry := store.Snapshot().Entries[key]; entry.Availability != llmkit.ProviderDegraded || entry.FailureStreak != 1 || !entry.CooldownUntil.IsZero() {
			t.Fatalf("entry after a later failure = %+v, want degraded with a new streak of 1", entry)
		}
	})

	t.Run("a burst of failures opens one cooldown", func(t *testing.T) {
		clock := newClock()
		store, _ := newStore(t, clock.policy(3, time.Minute, time.Hour))
		c := testCandidate("acct", "model", 0)

		for range 10 {
			mustAcquire(t, store, c)
		}
		for range 10 {
			mustRecord(t, store, failure(c, llmkit.ErrorClassTimeout))
		}
		entry := store.Snapshot().Entries[keyOf(c)]
		if entry.Availability != llmkit.ProviderUnavailable || !entry.CooldownUntil.Equal(clock.now().Add(time.Minute)) || entry.FailureStreak != 3 || entry.InFlight != 0 {
			t.Fatalf("entry = %+v, want one base cooldown after ten concurrent failures", entry)
		}
	})

	t.Run("caller-caused failures are ignored", func(t *testing.T) {
		clock := newClock()
		store, _ := newStore(t, clock.policy(2, time.Minute, 0))
		c := testCandidate("acct", "model", 0)
		key := keyOf(c)

		for _, class := range []llmkit.ErrorClass{llmkit.ErrorClassCanceled, llmkit.ErrorClassRequestTooLarge, llmkit.ErrorClassBudgetExceeded, llmkit.ErrorClassCanceled} {
			mustAcquire(t, store, c)
			mustRecord(t, store, failure(c, class))
		}
		entry := store.Snapshot().Entries[key]
		if entry.InFlight != 0 || entry.FailureStreak != 0 || entry.Availability != llmkit.ProviderAvailable {
			t.Fatalf("entry after caller-caused failures = %+v, want available with no streak", entry)
		}
		for _, class := range []llmkit.ErrorClass{llmkit.ErrorClassTimeout, llmkit.ErrorClassRateLimited} {
			mustAcquire(t, store, c)
			mustRecord(t, store, failure(c, class))
		}
		if entry := store.Snapshot().Entries[key]; entry.FailureStreak != 2 || entry.Availability != llmkit.ProviderUnavailable {
			t.Fatalf("entry after provider failures = %+v, want unavailable", entry)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		clock := newClock()
		store, _ := newStore(t, clock.policy(0, 0, 0))
		c := testCandidate("acct", "model", 0)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()

		if err := store.Acquire(cancelled, c); !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire with a cancelled context = %v, want context.Canceled", err)
		}
		if err := store.RecordOutcome(cancelled, failure(c, llmkit.ErrorClassTimeout)); !errors.Is(err, context.Canceled) {
			t.Fatalf("RecordOutcome with a cancelled context = %v, want context.Canceled", err)
		}
		if entry := store.Snapshot().Entries[keyOf(c)]; entry.InFlight != 0 || entry.FailureStreak != 0 {
			t.Fatalf("entry after cancelled calls = %+v, want nothing recorded", entry)
		}
	})
}

// ConcurrentAcquires calls store.Acquire for c from callers goroutines at
// once and counts the admitted calls and the ErrProviderBusy rejections.
func ConcurrentAcquires(store llmkit.HealthStore, c llmkit.Candidate, callers int) (admitted, busy int32) {
	var admittedCount, busyCount atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range callers {
		wg.Go(func() {
			<-start
			switch err := store.Acquire(context.Background(), c); {
			case err == nil:
				admittedCount.Add(1)
			case errors.Is(err, llmkit.ErrProviderBusy):
				busyCount.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	return admittedCount.Load(), busyCount.Load()
}

type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock {
	return &clock{at: time.Date(2026, 5, 6, 4, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func (c *clock) policy(threshold int, cooldown, maxCooldown time.Duration) llmkit.HealthPolicy {
	return llmkit.HealthPolicy{
		FailureCooldownThreshold: threshold,
		CooldownDuration:         cooldown,
		MaxCooldownDuration:      maxCooldown,
		Now:                      c.now,
	}
}

func testCandidate(account, model string, maxConcurrency int) llmkit.Candidate {
	return llmkit.Candidate{
		AccountAlias: account,
		Model:        llmkit.ModelCapability{Alias: model, Provider: "test", MaxConcurrency: maxConcurrency},
	}
}

func keyOf(c llmkit.Candidate) string {
	return llmkit.ProviderHealthKey(c.AccountAlias, c.Model.Alias, c.Model.Provider)
}

func success(c llmkit.Candidate) llmkit.TaskOutcome {
	return llmkit.TaskOutcome{AccountAlias: c.AccountAlias, ModelAlias: c.Model.Alias, Provider: c.Model.Provider, Success: true}
}

func failure(c llmkit.Candidate, class llmkit.ErrorClass) llmkit.TaskOutcome {
	return llmkit.TaskOutcome{AccountAlias: c.AccountAlias, ModelAlias: c.Model.Alias, Provider: c.Model.Provider, ErrorClass: class}
}

func mustAcquire(t *testing.T, store llmkit.HealthStore, c llmkit.Candidate) {
	t.Helper()
	if err := store.Acquire(context.Background(), c); err != nil {
		t.Fatalf("Acquire(%s) = %v, want admitted", keyOf(c), err)
	}
}

func mustRecord(t *testing.T, store llmkit.HealthStore, outcome llmkit.TaskOutcome) {
	t.Helper()
	if err := store.RecordOutcome(context.Background(), outcome); err != nil {
		t.Fatalf("RecordOutcome = %v", err)
	}
}

func wantBusy(t *testing.T, store llmkit.HealthStore, c llmkit.Candidate, reason string) {
	t.Helper()
	if err := store.Acquire(context.Background(), c); !errors.Is(err, llmkit.ErrProviderBusy) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("Acquire(%s) = %v, want ErrProviderBusy mentioning %q", keyOf(c), err, reason)
	}
}
