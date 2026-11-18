package pghealth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eruca/goagents/llmkit"
	"github.com/eruca/goagents/llmkit/healthtest"
)

func postgresDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("LLMKIT_POSTGRES_TEST_DSN"))
	if dsn == "" && os.Getenv("LLMKIT_REQUIRE_POSTGRES") == "1" {
		t.Fatal("LLMKIT_POSTGRES_TEST_DSN is required")
	}
	if dsn == "" {
		t.Skip("set LLMKIT_POSTGRES_TEST_DSN")
	}
	return dsn
}

var schemaSequence atomic.Uint64

// isolatedSchemaDSN creates a schema dropped at cleanup and returns a DSN
// whose search_path puts it first, so each test sees empty tables.
func isolatedSchemaDSN(t *testing.T) string {
	t.Helper()
	dsn := postgresDSN(t)
	schema := fmt.Sprintf("llmkit_health_test_%d_%d", os.Getpid(), schemaSequence.Add(1))
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin DB: %v", err)
	}
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		_ = admin.Close()
	})
	scoped, err := url.Parse(dsn)
	if err != nil || scoped.Scheme == "" {
		t.Fatalf("test DSN must be a PostgreSQL URL: %v", err)
	}
	query := scoped.Query()
	query.Set("search_path", schema)
	scoped.RawQuery = query.Encode()
	return scoped.String()
}

// openStore opens a Store with its own pool, as a separate replica would.
func openStore(t *testing.T, dsn string, cfg Config) *Store {
	t.Helper()
	store, err := Open(context.Background(), dsn, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func TestStoreConformance(t *testing.T) {
	postgresDSN(t)
	healthtest.RunHealthStoreConformance(t, func(t *testing.T, policy llmkit.HealthPolicy) (llmkit.HealthStore, func(llmkit.ProviderHealthEntry)) {
		store := openStore(t, isolatedSchemaDSN(t), Config{Policy: policy, LeaseTTL: time.Hour})
		return store, func(entry llmkit.ProviderHealthEntry) {
			t.Helper()
			if err := store.Set(context.Background(), entry); err != nil {
				t.Fatalf("Set: %v", err)
			}
		}
	})
}

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Date(2026, 5, 6, 4, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func testCandidate(account, model string, maxConcurrency int) llmkit.Candidate {
	return llmkit.Candidate{
		AccountAlias: account,
		Model:        llmkit.ModelCapability{Alias: model, Provider: "test", MaxConcurrency: maxConcurrency},
	}
}

// acquireAcross makes callers concurrent Acquires spread over stores and
// counts the admitted ones; any error other than ErrProviderBusy fails t.
func acquireAcross(t *testing.T, stores []*Store, candidates []llmkit.Candidate, callers int) int {
	t.Helper()
	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, callers)
	for i := range callers {
		store, c := stores[i%len(stores)], candidates[i%len(candidates)]
		wg.Go(func() {
			<-start
			switch err := store.Acquire(context.Background(), c); {
			case err == nil:
				admitted.Add(1)
			case !errors.Is(err, llmkit.ErrProviderBusy):
				errs <- err
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Acquire = %v", err)
	}
	return int(admitted.Load())
}

func TestStoreReplicasRacingAcquireNeverExceedLimits(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	clock := newTestClock()
	cfg := Config{Policy: llmkit.HealthPolicy{Now: clock.now}, LeaseTTL: time.Hour}
	stores := []*Store{openStore(t, dsn, cfg), openStore(t, dsn, cfg)}

	provider := testCandidate("acct", "model", 3)
	if admitted := acquireAcross(t, stores, []llmkit.Candidate{provider}, 40); admitted != 3 {
		t.Fatalf("admitted across replicas = %d, want the provider limit of 3", admitted)
	}

	first, second := testCandidate("shared", "model-a", 0), testCandidate("shared", "model-b", 0)
	first.AccountMaxConcurrency, second.AccountMaxConcurrency = 4, 4
	if admitted := acquireAcross(t, stores, []llmkit.Candidate{first, second}, 40); admitted != 4 {
		t.Fatalf("admitted across replicas and keys = %d, want the account limit of 4", admitted)
	}

	// After a failure opens the circuit and it cools down, the replicas
	// admit one probe between them.
	probe := testCandidate("probe-acct", "model", 0)
	if err := stores[0].Acquire(context.Background(), probe); err != nil {
		t.Fatal(err)
	}
	if err := stores[0].RecordOutcome(context.Background(), llmkit.TaskOutcome{
		AccountAlias: "probe-acct", ModelAlias: "model", Provider: "test", ErrorClass: llmkit.ErrorClassTimeout,
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := stores[1].Acquire(context.Background(), probe); err != nil {
			t.Fatal(err)
		}
		if err := stores[1].RecordOutcome(context.Background(), llmkit.TaskOutcome{
			AccountAlias: "probe-acct", ModelAlias: "model", Provider: "test", ErrorClass: llmkit.ErrorClassTimeout,
		}); err != nil {
			t.Fatal(err)
		}
	}
	clock.advance(time.Hour)
	if admitted := acquireAcross(t, stores, []llmkit.Candidate{probe}, 40); admitted != 1 {
		t.Fatalf("half-open probes admitted across replicas = %d, want 1", admitted)
	}
}

func TestStoreReclaimsClosedReplicasLeasesAfterTTL(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	clock := newTestClock()
	cfg := Config{Policy: llmkit.HealthPolicy{Now: clock.now}, LeaseTTL: time.Minute}
	crashed, err := Open(context.Background(), dsn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	survivor := openStore(t, dsn, cfg)
	c := testCandidate("acct", "model", 1)

	if err := crashed.Acquire(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := crashed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := survivor.Acquire(context.Background(), c); !errors.Is(err, llmkit.ErrProviderBusy) {
		t.Fatalf("Acquire while the closed replica's lease is live = %v, want ErrProviderBusy", err)
	}
	clock.advance(time.Minute)
	if err := survivor.Acquire(context.Background(), c); err != nil {
		t.Fatalf("Acquire after the lease TTL = %v, want admitted", err)
	}
	key := llmkit.ProviderHealthKey("acct", "model", "test")
	if entry := survivor.Snapshot().Entries[key]; entry.InFlight != 1 {
		t.Fatalf("in flight after reclaiming = %d, want 1", entry.InFlight)
	}
}

func TestStoreRenewalKeepsLongCallsLease(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	cfg := Config{LeaseTTL: 600 * time.Millisecond, RefreshInterval: 50 * time.Millisecond}
	caller, other := openStore(t, dsn, cfg), openStore(t, dsn, cfg)
	c := testCandidate("acct", "model", 1)

	if err := caller.Acquire(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * cfg.LeaseTTL)
	if err := other.Acquire(context.Background(), c); !errors.Is(err, llmkit.ErrProviderBusy) {
		t.Fatalf("Acquire during a renewed long call = %v, want ErrProviderBusy", err)
	}
	if err := caller.RecordOutcome(context.Background(), llmkit.TaskOutcome{
		AccountAlias: "acct", ModelAlias: "model", Provider: "test", Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := other.Acquire(context.Background(), c); err != nil {
		t.Fatalf("Acquire after the long call finished = %v, want admitted", err)
	}
}

func TestStoreSnapshotShowsOtherReplicasWrites(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	clock := newTestClock()
	cfg := Config{
		Policy:          llmkit.HealthPolicy{FailureCooldownThreshold: 2, CooldownDuration: time.Minute, Now: clock.now},
		LeaseTTL:        time.Hour,
		RefreshInterval: 20 * time.Millisecond,
	}
	reader, writer := openStore(t, dsn, cfg), openStore(t, dsn, cfg)
	c := testCandidate("acct", "model", 0)
	key := llmkit.ProviderHealthKey("acct", "model", "test")

	for range 3 {
		if err := writer.Acquire(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := writer.RecordOutcome(context.Background(), llmkit.TaskOutcome{
			AccountAlias: "acct", ModelAlias: "model", Provider: "test", ErrorClass: llmkit.ErrorClassTimeout,
		}); err != nil {
			t.Fatal(err)
		}
	}
	want := func(entry llmkit.ProviderHealthEntry) bool {
		return entry.InFlight == 1 && entry.FailureStreak == 2 && entry.Availability == llmkit.ProviderUnavailable &&
			entry.CooldownUntil.Equal(clock.now().Add(time.Minute))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		entry := reader.Snapshot().Entries[key]
		if want(entry) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reader snapshot entry = %+v, want the writer's open circuit with one call in flight", entry)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !want(writer.Snapshot().Entries[key]) {
		t.Fatalf("writer snapshot = %+v, want its own writes immediately", writer.Snapshot().Entries[key])
	}
}

func TestOpenRejectsNegativeDurations(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://unused.invalid/db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := OpenDB(context.Background(), db, Config{LeaseTTL: -time.Second}); err == nil {
		t.Fatal("OpenDB with a negative LeaseTTL succeeded")
	}
	if _, err := OpenDB(context.Background(), nil, Config{}); err == nil {
		t.Fatal("OpenDB with a nil database succeeded")
	}
	if _, err := Open(context.Background(), " ", Config{}); err == nil {
		t.Fatal("Open with an empty DSN succeeded")
	}
}

// TestStoreReleasesLeaseLockedByAnotherTransaction holds this Store's only
// lease for a key locked, as a renewal does, while Release or RecordOutcome
// runs: the call must wait for the lock and delete the lease, not skip it.
func TestStoreReleasesLeaseLockedByAnotherTransaction(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		close func(*Store, llmkit.Candidate) error
	}{
		{"release", func(s *Store, c llmkit.Candidate) error { return s.Release(ctx, c) }},
		{"record outcome", func(s *Store, c llmkit.Candidate) error {
			return s.RecordOutcome(ctx, llmkit.TaskOutcome{
				AccountAlias: c.AccountAlias, ModelAlias: c.Model.Alias, Provider: c.Model.Provider, ErrorClass: llmkit.ErrorClassTimeout,
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := isolatedSchemaDSN(t)
			clock := newTestClock()
			store := openStore(t, dsn, Config{Policy: llmkit.HealthPolicy{Now: clock.now}, LeaseTTL: time.Hour})
			c := testCandidate("acct", "model", 1)
			key := llmkit.ProviderHealthKey("acct", "model", "test")
			if err := store.Acquire(ctx, c); err != nil {
				t.Fatal(err)
			}

			locker, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer locker.Close()
			tx, err := locker.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			var leaseID string
			if err := tx.QueryRowContext(ctx, `SELECT lease_id FROM llmkit_provider_health_leases WHERE key = $1 FOR UPDATE`, key).Scan(&leaseID); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() { done <- tc.close(store, c) }()
			time.Sleep(200 * time.Millisecond)
			select {
			case err := <-done:
				t.Fatalf("call returned %v while its only lease was locked, want it to wait", err)
			default:
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("call after the lock was released = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("call did not return after the lock was released")
			}
			var live int
			if err := locker.QueryRowContext(ctx, `SELECT count(*) FROM llmkit_provider_health_leases WHERE key = $1`, key).Scan(&live); err != nil {
				t.Fatal(err)
			}
			if live != 0 {
				t.Fatalf("leases left for the key = %d, want 0", live)
			}
			if err := store.Acquire(ctx, c); err != nil {
				t.Fatalf("Acquire after the slot was given back = %v, want admitted", err)
			}
		})
	}
}
