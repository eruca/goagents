package pgaudit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eruca/goagents/llmkit"
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
	schema := fmt.Sprintf("llmkit_audit_test_%d_%d", os.Getpid(), schemaSequence.Add(1))
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
func openStore(t *testing.T, dsn string) *Store {
	t.Helper()
	store, err := Open(context.Background(), dsn)
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

func TestOpenRequiresDSN(t *testing.T) {
	if _, err := Open(context.Background(), "  "); err == nil {
		t.Fatal("Open with an empty DSN succeeded")
	}
	if _, err := OpenDB(context.Background(), nil); err == nil {
		t.Fatal("OpenDB with a nil pool succeeded")
	}
}

// auditEvent is one write: a route or an outcome.
type auditEvent struct {
	route   *llmkit.RouteTrace
	outcome *llmkit.TaskOutcome
}

func routeEvent(trace llmkit.RouteTrace) auditEvent      { return auditEvent{route: &trace} }
func outcomeEvent(outcome llmkit.TaskOutcome) auditEvent { return auditEvent{outcome: &outcome} }

// equivalenceEvents covers the join and stats rules. Every record sets
// RecordedAt: a zero one is filled with the current time when it is written,
// which would differ between the two recorders.
func equivalenceEvents() []auditEvent {
	base := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	east := time.FixedZone("UTC+8", 8*3600)
	return []auditEvent{
		// task-a, attempt 1 fails over to attempt 2 on another model.
		routeEvent(llmkit.RouteTrace{
			RouteID: "route-a", TaskID: "task-a", Attempt: 1, RecordedAt: at(0),
			TaskType: "  code  ", TaskProfile: &llmkit.TaskProfile{TaskType: "code secret_key"},
			AccountAlias: "local", ModelAlias: "small", Provider: "ollama", Selected: true,
			Reason: "picked; key sk-abcdefghijklmnop leaked via Bearer header", Score: 7,
			ScoreBreakdown:        map[string]int{"cost": 3, "latency": 4},
			CandidateModelAliases: []string{"small", " big "},
			Candidates: []llmkit.CandidateScore{
				{Alias: "small", AccountAlias: "local", Available: true, Score: 7, Reason: "sk-zzzzzzzzzzzz"},
				{Alias: "big", AccountAlias: "cloud", Score: 5, ScoreBreakdown: map[string]int{"cost": -2}},
			},
			FallbackMaxAttempts: 2,
		}),
		outcomeEvent(llmkit.TaskOutcome{
			RouteID: "route-a", TaskID: "task-a", Attempt: 1, RecordedAt: at(1),
			TaskType: "code", AccountAlias: "local", ModelAlias: "small", Provider: "ollama",
			ErrorClass: llmkit.ErrorClassTimeout, ErrorCode: "timeout", LatencyMillis: 30000,
			FailureReason: "upstream said secret-key sk-1234567890abcdef\x00 is bad",
		}),
		routeEvent(llmkit.RouteTrace{
			RouteID: "route-a", TaskID: "task-a", Attempt: 2, RecordedAt: at(2).In(east),
			TaskType: "code", AccountAlias: "cloud", ModelAlias: "big", Provider: "openai", Selected: true,
		}),
		outcomeEvent(llmkit.TaskOutcome{
			RouteID: "route-a", TaskID: "task-a", Attempt: 2, RecordedAt: at(3),
			TaskType: "code", AccountAlias: "cloud", ModelAlias: "big", Provider: "openai",
			Success: true, LatencyMillis: 1200, InputTokens: 100, OutputTokens: 40, EstimatedCents: 3,
		}),
		// A later business outcome for the same attempt replaces the provider outcome.
		outcomeEvent(llmkit.TaskOutcome{
			RouteID: "route-a", TaskID: "task-a", Attempt: 2, RecordedAt: at(10),
			TaskType: "code", AccountAlias: "cloud", ModelAlias: "big", Provider: "openai",
			BusinessOutcome: llmkit.BusinessOutcomeFailure, ErrorClass: llmkit.ErrorClassInvalidJSON,
		}),
		// task-b has a route with no outcome yet.
		routeEvent(llmkit.RouteTrace{
			RouteID: "route-b", TaskID: "task-b", Attempt: 1, RecordedAt: at(-60 * 24 * 10),
			TaskType: "chat", AccountAlias: "local", ModelAlias: "small", Provider: "ollama",
		}),
		// Outcomes without a route_id all count.
		outcomeEvent(llmkit.TaskOutcome{
			TaskID: "task-c", RecordedAt: at(-60 * 24 * 3), TaskType: "chat",
			AccountAlias: "local", ModelAlias: "small", Provider: "ollama", Success: true, LatencyMillis: 50,
		}),
		outcomeEvent(llmkit.TaskOutcome{
			TaskID: "task-c", RecordedAt: at(5), TaskType: "chat",
			AccountAlias: "local", ModelAlias: "small", Provider: "ollama", ErrorClass: llmkit.ErrorClassCanceled,
		}),
		// An outcome whose route was never recorded.
		outcomeEvent(llmkit.TaskOutcome{
			RouteID: "route-orphan", TaskID: "task-a", Attempt: 1, RecordedAt: at(6),
			TaskType: "code", AccountAlias: "cloud", ModelAlias: "big", Provider: "openai", Success: true,
		}),
		// An outcome recorded before its route, then a repeat of it.
		outcomeEvent(llmkit.TaskOutcome{
			RouteID: "route-d", TaskID: "task-d", Attempt: 1, RecordedAt: at(7),
			TaskType: "code", AccountAlias: "local", ModelAlias: "small", Provider: "ollama", Success: true,
		}),
		routeEvent(llmkit.RouteTrace{
			RouteID: "route-d", TaskID: "task-d", Attempt: 1, RecordedAt: at(6),
			TaskType: "code", AccountAlias: "local", ModelAlias: "small", Provider: "ollama",
		}),
		outcomeEvent(llmkit.TaskOutcome{
			RouteID: "route-d", TaskID: "task-d", Attempt: 1, RecordedAt: at(8),
			TaskType: "code", AccountAlias: "local", ModelAlias: "small", Provider: "ollama",
			ErrorClass: llmkit.ErrorClassRateLimited,
		}),
		// IDs a TEXT column cannot hold as they are.
		routeEvent(llmkit.RouteTrace{
			RouteID: "route-\x00e", TaskID: "task-\x00e\xff", Attempt: 1, RecordedAt: at(9),
			TaskType: "code", AccountAlias: "local", ModelAlias: "small", Provider: "ollama",
		}),
		outcomeEvent(llmkit.TaskOutcome{
			RouteID: "route-\x00e", TaskID: "task-\x00e\xff", Attempt: 1, RecordedAt: at(9),
			TaskType: "code", AccountAlias: "local", ModelAlias: "small", Provider: "ollama", Success: true,
		}),
	}
}

func writeEvents(t *testing.T, recorder llmkit.Recorder, events []auditEvent) {
	t.Helper()
	ctx := context.Background()
	for i, event := range events {
		var err error
		if event.route != nil {
			err = recorder.RecordRoute(ctx, *event.route)
		} else {
			err = recorder.RecordOutcome(ctx, *event.outcome)
		}
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
}

func TestStoreMatchesJSONLRecorder(t *testing.T) {
	store := openStore(t, isolatedSchemaDSN(t))
	home := t.TempDir()
	jsonl, err := llmkit.NewJSONLRecorder(home)
	if err != nil {
		t.Fatalf("NewJSONLRecorder: %v", err)
	}
	events := equivalenceEvents()
	writeEvents(t, jsonl, events)
	writeEvents(t, store, events)

	ctx := context.Background()
	filters := []llmkit.AuditFilter{
		{},
		{TaskID: "task-a"},
		{TaskID: "task-b"},
		{TaskID: "task-c"},
		{TaskID: "task-d"},
		{TaskID: "task-\x00e\xff"},
		{TaskID: "task-\x00e\uFFFD"},
		{TaskID: "task-\uFFFDe\uFFFD"},
		{TaskID: " task-a"},
		{TaskID: "   "},
		{TaskID: "missing"},
	}
	for _, filter := range filters {
		want, err := llmkit.ReadRouteAudits(home, filter)
		if err != nil {
			t.Fatalf("file ReadRouteAudits(%q): %v", filter.TaskID, err)
		}
		got, err := store.ReadRouteAudits(ctx, filter)
		if err != nil {
			t.Fatalf("pg ReadRouteAudits(%q): %v", filter.TaskID, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ReadRouteAudits(%q):\n got %+v\nwant %+v", filter.TaskID, got, want)
		}
	}

	all, err := store.ReadRouteAudits(ctx, llmkit.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("records = %d, want 5", len(all))
	}
	first := all[0]
	if strings.Contains(first.Route.Reason, "sk-abc") || strings.Contains(strings.ToLower(first.Route.Reason), "bearer") ||
		first.Route.TaskType != "code" || first.Route.Candidates[0].Reason != "[redacted]" ||
		first.Outcome == nil || strings.Contains(first.Outcome.FailureReason, "sk-123") {
		t.Fatalf("record was not sanitized: %+v outcome %+v", first.Route, first.Outcome)
	}
	if all[1].Outcome == nil || all[1].Outcome.BusinessOutcome != llmkit.BusinessOutcomeFailure {
		t.Fatalf("fallback attempt joined %+v, want the later business outcome", all[1].Outcome)
	}

	want, err := llmkit.BuildModelStats(home)
	if err != nil {
		t.Fatalf("file BuildModelStats: %v", err)
	}
	got, err := store.BuildModelStats(ctx)
	if err != nil {
		t.Fatalf("pg BuildModelStats: %v", err)
	}
	assertStatsEqual(t, got, want)
}

// assertStatsEqual ignores GeneratedAt. The decayed counts depend on it, so
// they are compared with a tolerance far above the drift between two calls.
func assertStatsEqual(t *testing.T, got, want *llmkit.ModelStats) {
	t.Helper()
	if len(got.Models) != len(want.Models) {
		t.Fatalf("stats buckets = %d, want %d:\n got %+v\nwant %+v", len(got.Models), len(want.Models), got.Models, want.Models)
	}
	for key, w := range want.Models {
		g, ok := got.Models[key]
		if !ok {
			t.Errorf("missing stats bucket %q", key)
			continue
		}
		if !closeEnough(g.RecentOutcomes, w.RecentOutcomes) || !closeEnough(g.RecentFailures, w.RecentFailures) {
			t.Errorf("bucket %q recent = %v/%v, want %v/%v", key, g.RecentOutcomes, g.RecentFailures, w.RecentOutcomes, w.RecentFailures)
		}
		g.RecentOutcomes, g.RecentFailures = 0, 0
		w.RecentOutcomes, w.RecentFailures = 0, 0
		if !reflect.DeepEqual(g, w) {
			t.Errorf("bucket %q:\n got %+v\nwant %+v", key, g, w)
		}
	}
}

func closeEnough(a, b float64) bool {
	return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b))
}

func TestBuildModelStatsUsesClock(t *testing.T) {
	store := openStore(t, isolatedSchemaDSN(t))
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	events := equivalenceEvents()
	writeEvents(t, store, events)

	var routes []llmkit.RouteTrace
	var outcomes []llmkit.TaskOutcome
	for _, event := range events {
		if event.route != nil {
			routes = append(routes, llmkit.SanitizeRouteTrace(*event.route))
		} else {
			outcomes = append(outcomes, llmkit.SanitizeTaskOutcome(*event.outcome))
		}
	}
	got, err := store.BuildModelStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := llmkit.BuildModelStatsFromAudit(now, routes, outcomes)
	if !got.GeneratedAt.Equal(now) {
		t.Fatalf("GeneratedAt = %v, want %v", got.GeneratedAt, now)
	}
	assertStatsEqual(t, got, want)
}

func TestConcurrentWritesFromTwoPools(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	stores := []*Store{openStore(t, dsn), openStore(t, dsn)}
	const workers = 8
	const tasksPerWorker = 10
	base := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Go(func() {
			for i := range tasksPerWorker {
				routeID := fmt.Sprintf("route-%d-%d", w, i)
				first, second := stores[(w+i)%2], stores[(w+i+1)%2]
				route := llmkit.RouteTrace{
					RouteID: routeID, TaskID: fmt.Sprintf("task-%d", w), Attempt: 1, RecordedAt: base,
					TaskType: "code", AccountAlias: "local", ModelAlias: "small", Provider: "ollama",
				}
				provider := llmkit.TaskOutcome{
					RouteID: routeID, TaskID: route.TaskID, Attempt: 1, RecordedAt: base,
					TaskType: "code", AccountAlias: "local", ModelAlias: "small", Provider: "ollama",
					ErrorClass: llmkit.ErrorClassTimeout,
				}
				business := provider
				business.Success, business.ErrorClass = true, ""
				// Each outcome is written after the previous write returned,
				// through the other pool, so the business outcome is the
				// later one.
				if err := first.RecordRoute(ctx, route); err != nil {
					errs <- err
					return
				}
				if err := first.RecordOutcome(ctx, provider); err != nil {
					errs <- err
					return
				}
				if err := second.RecordOutcome(ctx, business); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	for _, store := range stores {
		records, err := store.ReadRouteAudits(ctx, llmkit.AuditFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != workers*tasksPerWorker {
			t.Fatalf("records = %d, want %d", len(records), workers*tasksPerWorker)
		}
		for _, record := range records {
			if record.Outcome == nil || !record.Outcome.Success {
				t.Fatalf("route %s joined %+v, want the later successful outcome", record.Route.RouteID, record.Outcome)
			}
		}
		perTask, err := store.ReadRouteAudits(ctx, llmkit.AuditFilter{TaskID: "task-3"})
		if err != nil {
			t.Fatal(err)
		}
		if len(perTask) != tasksPerWorker {
			t.Fatalf("task-3 records = %d, want %d", len(perTask), tasksPerWorker)
		}
		// One worker writes its routes in order, so they read back in order.
		for i, record := range perTask {
			if want := fmt.Sprintf("route-3-%d", i); record.Route.RouteID != want {
				t.Fatalf("task-3 record %d = %s, want %s", i, record.Route.RouteID, want)
			}
		}
		stats, err := store.BuildModelStats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		entry := stats.Models["code|local|small|ollama"]
		if entry.RouteAttempts != workers*tasksPerWorker || entry.OutcomeCount != workers*tasksPerWorker ||
			entry.Successes != workers*tasksPerWorker || entry.Failures != 0 {
			t.Fatalf("stats = %+v", entry)
		}
	}
}

func TestCanceledContextWritesNothing(t *testing.T) {
	store := openStore(t, isolatedSchemaDSN(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.RecordRoute(ctx, llmkit.RouteTrace{RouteID: "r", TaskID: "t"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecordRoute = %v, want context.Canceled", err)
	}
	if err := store.RecordOutcome(ctx, llmkit.TaskOutcome{RouteID: "r", TaskID: "t"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecordOutcome = %v, want context.Canceled", err)
	}
	if _, err := store.ReadRouteAudits(ctx, llmkit.AuditFilter{}); err == nil {
		t.Fatal("ReadRouteAudits with a canceled context succeeded")
	}
	records, err := store.ReadRouteAudits(context.Background(), llmkit.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := store.BuildModelStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 || len(stats.Models) != 0 {
		t.Fatalf("canceled writes stored records: %+v %+v", records, stats.Models)
	}
}

func TestMigrationIsIdempotentAndRejectsNewerVersion(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	// Concurrent first opens serialize on the advisory lock.
	var wg sync.WaitGroup
	openErrs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			store, err := OpenDB(ctx, db)
			if err == nil {
				err = store.Close()
			}
			openErrs <- err
		})
	}
	wg.Wait()
	close(openErrs)
	for err := range openErrs {
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
	}

	store, err := OpenDB(ctx, db)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := store.RecordRoute(ctx, llmkit.RouteTrace{RouteID: "r", TaskID: "t", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Close leaves a caller-owned pool open.
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("pool closed by Store.Close: %v", err)
	}
	store, err = OpenDB(ctx, db)
	if err != nil {
		t.Fatalf("reopen with data: %v", err)
	}
	records, err := store.ReadRouteAudits(ctx, llmkit.AuditFilter{})
	if err != nil || len(records) != 1 {
		t.Fatalf("records after reopen = %+v, %v", records, err)
	}
	var versions int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM llmkit_audit_schema_versions").Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("schema versions = %d, %v", versions, err)
	}

	if _, err := db.ExecContext(ctx, "INSERT INTO llmkit_audit_schema_versions(version, applied_at) VALUES (2, now())"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDB(ctx, db); err == nil || !strings.Contains(err.Error(), "unsupported schema version 2") {
		t.Fatalf("OpenDB on a newer schema = %v, want unsupported schema version", err)
	}
}
