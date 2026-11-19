package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/runkit/storetest"
)

// postgresDSN skips the test without RUNKIT_POSTGRES_TEST_DSN, or fails it
// when RUNKIT_REQUIRE_POSTGRES=1 so CI cannot silently skip PostgreSQL.
func postgresDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("RUNKIT_POSTGRES_TEST_DSN"))
	if dsn == "" && os.Getenv("RUNKIT_REQUIRE_POSTGRES") == "1" {
		t.Fatal("RUNKIT_POSTGRES_TEST_DSN is required")
	}
	if dsn == "" {
		t.Skip("set RUNKIT_POSTGRES_TEST_DSN to run PostgreSQL integration tests")
	}
	return dsn
}

var schemaSequence atomic.Uint64

// isolatedSchemaDSN creates a fresh schema, dropped at cleanup, and returns a
// DSN whose connections use it. The conformance suite uses fixed run ids, so
// every store it opens needs its own tables.
func isolatedSchemaDSN(t *testing.T) string {
	t.Helper()
	dsn := postgresDSN(t)
	schema := fmt.Sprintf("runkit_test_%d_%d", os.Getpid(), schemaSequence.Add(1))

	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open schema admin DB: %v", err)
	}
	if _, err := adminDB.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = adminDB.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := adminDB.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated schema: %v", err)
		}
		_ = adminDB.Close()
	})

	scopedURL, err := url.Parse(dsn)
	if err != nil || scopedURL.Scheme == "" {
		t.Fatalf("test DSN must be a PostgreSQL URL: %v", err)
	}
	query := scopedURL.Query()
	query.Set("search_path", schema)
	scopedURL.RawQuery = query.Encode()
	return scopedURL.String()
}

// openStore opens a Store with its own connection pool, standing in for one
// replica. It is closed before the schema is dropped.
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

func TestRunStoreConformance(t *testing.T) {
	postgresDSN(t)
	storetest.RunStoreConformance(t, func(t *testing.T) runkit.Store {
		return openStore(t, isolatedSchemaDSN(t))
	})
}

func TestRunStoreRoundTripsTimesAndRecreate(t *testing.T) {
	store := openStore(t, isolatedSchemaDSN(t))
	ctx := context.Background()
	created := time.Date(2026, 5, 6, 1, 2, 3, 456789123, time.FixedZone("x", 3600))
	if err := store.Create(ctx, runkit.RunRecord{RunID: "run-1", WorkflowID: "wf", Status: runkit.StatusRunning, CreatedAt: created}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	recorded := time.Date(2026, 5, 6, 2, 0, 0, 999, time.UTC)
	if err := store.AppendEvent(ctx, runkit.RunEvent{RunID: "run-1", Type: "x", RecordedAt: recorded}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	got, err := store.Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if want := created.UTC().Truncate(time.Microsecond); !got.CreatedAt.Equal(want) || got.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt = %v, want %v in UTC", got.CreatedAt, want)
	}
	events, err := store.Events(ctx, "run-1")
	if err != nil || len(events) != 1 {
		t.Fatalf("Events = %+v, %v", events, err)
	}
	if want := recorded.Truncate(time.Microsecond); !events[0].RecordedAt.Equal(want) || events[0].RecordedAt.Location() != time.UTC {
		t.Fatalf("RecordedAt = %v, want %v in UTC", events[0].RecordedAt, want)
	}

	// Complete twice: the second summary replaces the first.
	for _, status := range []runkit.Status{runkit.StatusFailed, runkit.StatusSucceeded} {
		if err := store.Complete(ctx, "run-1", runkit.TerminalSummary{Status: status}); err != nil {
			t.Fatalf("Complete(%s): %v", status, err)
		}
	}
	// Create again replaces the record but keeps its events.
	if err := store.Create(ctx, runkit.RunRecord{RunID: "run-1", WorkflowID: "wf-2", Status: runkit.StatusRunning, CreatedAt: created}); err != nil {
		t.Fatalf("Create again: %v", err)
	}
	got, err = store.Get(ctx, "run-1")
	if err != nil || got.WorkflowID != "wf-2" || got.Status != runkit.StatusRunning || got.Summary.Status != "" {
		t.Fatalf("recreated = %+v, %v", got, err)
	}
	if events, err := store.Events(ctx, "run-1"); err != nil || len(events) != 1 {
		t.Fatalf("Events after recreate = %+v, %v", events, err)
	}
}

func TestRunStoreConcurrentAppendFromTwoPools(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	first := openStore(t, dsn)
	second := openStore(t, dsn)
	ctx := context.Background()
	for _, runID := range []string{"shared-run", "other-run"} {
		if err := first.Create(ctx, runkit.RunRecord{RunID: runID, WorkflowID: "wf", Status: runkit.StatusRunning}); err != nil {
			t.Fatalf("Create(%s): %v", runID, err)
		}
	}

	const perWriter = 25
	type writer struct {
		store *Store
		name  string
	}
	writers := []writer{{first, "a"}, {second, "b"}, {first, "c"}, {second, "d"}}
	errs := make(chan error, len(writers)*perWriter*2)
	var wg sync.WaitGroup
	for _, w := range writers {
		wg.Add(1)
		go func(w writer) {
			defer wg.Done()
			for i := range perWriter {
				msg := fmt.Sprintf("%s-%d", w.name, i)
				errs <- w.store.AppendEvent(ctx, runkit.RunEvent{RunID: "shared-run", Type: "step", Message: msg})
				errs <- w.store.AppendEvent(ctx, runkit.RunEvent{RunID: "other-run", Type: "step", Message: msg})
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	for _, runID := range []string{"shared-run", "other-run"} {
		// Read through the other pool from the one that created the run.
		events, err := second.Events(ctx, runID)
		if err != nil {
			t.Fatalf("Events(%s): %v", runID, err)
		}
		if len(events) != len(writers)*perWriter {
			t.Fatalf("%s events = %d, want %d", runID, len(events), len(writers)*perWriter)
		}
		seen := map[string]bool{}
		lastByWriter := map[string]int{}
		for i, event := range events {
			if event.Sequence != i+1 {
				t.Fatalf("%s event %d sequence = %d, want gap-free %d", runID, i, event.Sequence, i+1)
			}
			if seen[event.Message] {
				t.Fatalf("%s duplicate event %q", runID, event.Message)
			}
			seen[event.Message] = true
			// Each writer appends serially, so its own events keep their order.
			var name string
			var n int
			if _, err := fmt.Sscanf(strings.Replace(event.Message, "-", " ", 1), "%s %d", &name, &n); err != nil {
				t.Fatalf("parse %q: %v", event.Message, err)
			}
			if last, ok := lastByWriter[name]; ok && n != last+1 {
				t.Fatalf("%s writer %s order: %d after %d", runID, name, n, last)
			}
			lastByWriter[name] = n
		}
	}
}

func TestRunStoreWriteOnePoolReadAnother(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	writer := openStore(t, dsn)
	reader := openStore(t, dsn)
	ctx := context.Background()

	if err := writer.Create(ctx, runkit.RunRecord{RunID: "run-1", WorkflowID: "wf-1", TaskID: "task-1", Status: runkit.StatusRunning, Metadata: map[string]any{"k": "v"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := writer.AppendEvent(ctx, runkit.RunEvent{RunID: "run-1", Type: "stage.started", Stage: "think", Iteration: 2, Message: "m", Metadata: map[string]any{"n": 1}}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := writer.Complete(ctx, "run-1", runkit.TerminalSummary{Status: runkit.StatusSucceeded, ContentRef: "ref", AbortReason: "none", InputTokens: 1, OutputTokens: 2, LLMCalls: 3, ToolCalls: 4, UsedTools: []string{"t"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, err := reader.Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != runkit.StatusSucceeded || got.TaskID != "task-1" || got.Metadata["k"] != "v" ||
		got.Summary.ContentRef != "ref" || got.Summary.AbortReason != "none" || got.Summary.InputTokens != 1 ||
		got.Summary.OutputTokens != 2 || got.Summary.LLMCalls != 3 || got.Summary.ToolCalls != 4 ||
		len(got.Summary.UsedTools) != 1 || got.Summary.UsedTools[0] != "t" {
		t.Fatalf("reader Get = %+v", got)
	}
	events, err := reader.Events(ctx, "run-1")
	if err != nil || len(events) != 1 {
		t.Fatalf("reader Events = %+v, %v", events, err)
	}
	if e := events[0]; e.Sequence != 1 || e.Stage != "think" || e.Iteration != 2 || e.Message != "m" || e.Metadata["n"] != float64(1) {
		t.Fatalf("reader event = %+v", e)
	}
	runs, err := reader.FindByWorkflowID(ctx, "wf-1")
	if err != nil || len(runs) != 1 || runs[0].RunID != "run-1" {
		t.Fatalf("reader FindByWorkflowID = %+v, %v", runs, err)
	}
}

func TestRunStoreMigratesCheckpointOnlyDatabase(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	// The schema the checkpoint-only pgstore created.
	if _, err := db.ExecContext(ctx, `
CREATE TABLE approval_checkpoints (
 checkpoint_id TEXT PRIMARY KEY,
 run_id TEXT NOT NULL,
 tenant_id TEXT NOT NULL,
 definition_hash TEXT NOT NULL,
 ciphertext BYTEA NOT NULL,
 status TEXT NOT NULL CHECK (status IN ('pending', 'leased', 'consumed', 'rejected', 'failed', 'expired')),
 failure_code TEXT NOT NULL DEFAULT '',
 lease_owner TEXT NOT NULL DEFAULT '',
 lease_until TIMESTAMPTZ,
 expires_at TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_approval_checkpoints_tenant_status_expiry ON approval_checkpoints (tenant_id, status, expires_at);
CREATE TABLE approval_decisions (
 checkpoint_id TEXT PRIMARY KEY REFERENCES approval_checkpoints(checkpoint_id) ON DELETE CASCADE,
 approver_id TEXT NOT NULL,
 approved BOOLEAN NOT NULL,
 audit_ref TEXT NOT NULL,
 reason_code TEXT NOT NULL DEFAULT '',
 decided_at TIMESTAMPTZ NOT NULL
);
INSERT INTO approval_checkpoints (checkpoint_id, run_id, tenant_id, definition_hash, ciphertext, status, expires_at, created_at, updated_at)
VALUES ('cp-legacy', 'run-legacy', 'tenant', 'hash', 'secret'::bytea, 'pending', now() + interval '1 hour', now(), now());`); err != nil {
		t.Fatalf("create checkpoint-only schema: %v", err)
	}

	store := openStore(t, dsn)
	checkpoint, err := store.GetCheckpoint(ctx, "cp-legacy", "tenant")
	if err != nil || checkpoint.Status != runkit.CheckpointPending || string(checkpoint.Ciphertext) != "secret" {
		t.Fatalf("legacy checkpoint = %+v, %v", checkpoint, err)
	}
	if err := store.Create(ctx, runkit.RunRecord{RunID: "run-legacy", WorkflowID: "wf", Status: runkit.StatusRunning}); err != nil {
		t.Fatalf("Create after migration: %v", err)
	}
	if err := store.AppendEvent(ctx, runkit.RunEvent{RunID: "run-legacy", Type: "x"}); err != nil {
		t.Fatalf("AppendEvent after migration: %v", err)
	}
	// Opening again is a no-op migration.
	again := openStore(t, dsn)
	if events, err := again.Events(ctx, "run-legacy"); err != nil || len(events) != 1 {
		t.Fatalf("Events after reopen = %+v, %v", events, err)
	}
}

func TestOpenDBSharesCallerPool(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := OpenDB(context.Background(), nil); err == nil {
		t.Fatal("OpenDB accepted a nil pool")
	}
	store, err := OpenDB(context.Background(), db)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := store.Create(context.Background(), runkit.RunRecord{RunID: "run-shared-pool", WorkflowID: "wf"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("Close closed the caller's pool: %v", err)
	}
}
