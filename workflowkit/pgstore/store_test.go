package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/workflowkit"
	"github.com/eruca/goagents/workflowkit/storetest"
)

func newConformanceStore(t *testing.T) workflowkit.Store {
	return openIsolatedStore(t)
}

func TestQueueRecoveryStoreConformance(t *testing.T) {
	postgresDSN(t)
	storetest.RunQueueRecoveryStoreConformance(t, newConformanceStore)
}

func TestWorkflowQueryStoreConformance(t *testing.T) {
	postgresDSN(t)
	storetest.RunWorkflowQueryStoreConformance(t, newConformanceStore)
}

func TestOpenRequiresDSNAndDB(t *testing.T) {
	if _, err := Open(context.Background(), " "); err == nil {
		t.Fatal("Open with an empty DSN returned no error")
	}
	if _, err := OpenDB(context.Background(), nil); err == nil {
		t.Fatal("OpenDB with a nil pool returned no error")
	}
}

func TestStoreRoundTripsRunWithMicrosecondTimes(t *testing.T) {
	store := openIsolatedStore(t)
	ctx := t.Context()
	created := time.Date(2026, 5, 3, 10, 0, 0, 123456789, time.FixedZone("UTC+9", 9*3600))
	run := workflowkit.WorkflowRun{
		ID:             "wf-roundtrip",
		Status:         workflowkit.StatusWaitingApproval,
		InputRef:       "artifact:in",
		ApprovalRef:    "approval:req-1",
		CompletedSteps: []string{"prepare", "agent"},
		StepAttempts:   map[string]int{"prepare": 1, "agent": 1},
		StepRetries:    2,
		NextAttemptAt:  created.Add(time.Hour),
		CreatedAt:      created,
		StepRecords: []workflowkit.StepRecord{{
			Name:      "agent",
			Status:    workflowkit.StatusWaitingApproval,
			Attempt:   1,
			StartedAt: created,
			Metadata:  map[string]any{"tool": "write_file"},
		}},
		Metadata: map[string]any{"tenant": "demo"},
	}
	if err := store.Create(ctx, run); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	loaded, err := store.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if want := created.Truncate(time.Microsecond); !loaded.CreatedAt.Equal(want) || loaded.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt = %s, want %s in UTC", loaded.CreatedAt, want)
	}
	if !loaded.NextAttemptAt.Equal(run.NextAttemptAt.Truncate(time.Microsecond)) || !loaded.LeaseUntil.IsZero() {
		t.Fatalf("NextAttemptAt = %s, LeaseUntil = %s", loaded.NextAttemptAt, loaded.LeaseUntil)
	}
	if loaded.StepRetries != 2 || loaded.Metadata["tenant"] != "demo" || loaded.StepAttempts["agent"] != 1 || len(loaded.CompletedSteps) != 2 {
		t.Fatalf("loaded = %+v", loaded)
	}
	if len(loaded.StepRecords) != 1 || loaded.StepRecords[0].Metadata["tool"] != "write_file" || !loaded.StepRecords[0].StartedAt.Equal(created) {
		t.Fatalf("step records = %+v", loaded.StepRecords)
	}

	loaded.StepRecords = append(loaded.StepRecords, workflowkit.StepRecord{Name: "next", Attempt: 1})
	if err := store.Save(ctx, loaded); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	loaded.StepRecords = loaded.StepRecords[:1]
	loaded.StepRecords[0].Error = "rewritten"
	if err := store.Save(ctx, loaded); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	again, err := store.Get(ctx, run.ID)
	if err != nil || len(again.StepRecords) != 1 || again.StepRecords[0].Error != "rewritten" {
		t.Fatalf("Get = %+v, %v; want the truncated, rewritten records", again.StepRecords, err)
	}
}

func TestStoreUpdateSerializesConcurrentSave(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	updater := openStoreDB(t, openPool(t, dsn))
	saver := openStoreDB(t, openPool(t, dsn))

	ctx := t.Context()
	if err := updater.Save(ctx, workflowkit.WorkflowRun{
		ID:       "wf-atomic-update",
		Status:   workflowkit.StatusRunning,
		InputRef: "artifact:initial-input",
		Metadata: map[string]any{"source": "initial"},
	}); err != nil {
		t.Fatalf("initial Save returned error: %v", err)
	}

	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	updateResult := make(chan error, 1)
	go func() {
		_, err := updater.Update(ctx, "wf-atomic-update", func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
			close(callbackEntered)
			<-releaseCallback
			run.Status = workflowkit.StatusFailed
			run.Error = "updated"
			return run, nil
		})
		updateResult <- err
	}()
	<-callbackEntered

	saveCtx, cancelSave := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelSave()
	saveErr := saver.Save(saveCtx, workflowkit.WorkflowRun{
		ID:       "wf-atomic-update",
		Status:   workflowkit.StatusSucceeded,
		InputRef: "artifact:concurrent-input",
		Metadata: map[string]any{"source": "concurrent-save"},
	})
	close(releaseCallback)
	updateErr := <-updateResult

	if saveErr == nil {
		final, _ := updater.Get(ctx, "wf-atomic-update")
		t.Fatalf("concurrent Save completed inside Update callback and was overwritten: final=%+v", final)
	}
	if saveCtx.Err() == nil {
		t.Fatalf("concurrent Save error = %v before its deadline, want it blocked on the row lock", saveErr)
	}
	if updateErr != nil {
		t.Fatalf("Update returned error: %v", updateErr)
	}
	final, err := updater.Get(ctx, "wf-atomic-update")
	if err != nil {
		t.Fatalf("final Get returned error: %v", err)
	}
	if final.Status != workflowkit.StatusFailed || final.Error != "updated" {
		t.Fatalf("final run = %+v, want failed update", final)
	}
	if final.InputRef != "artifact:initial-input" || final.Metadata["source"] != "initial" {
		t.Fatalf("Update lost fields from its transactional snapshot: %+v", final)
	}
}

func TestStoreUpdateRollsBackCallbackError(t *testing.T) {
	store := openIsolatedStore(t)
	ctx := t.Context()
	original := workflowkit.WorkflowRun{ID: "wf-callback-error", Status: workflowkit.StatusRunning, InputRef: "artifact:original"}
	if err := store.Save(ctx, original); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	sentinel := errors.New("mutate failed")
	_, err := store.Update(ctx, original.ID, func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		run.Status = workflowkit.StatusFailed
		run.InputRef = "artifact:changed"
		return run, sentinel
	})
	if err != sentinel {
		t.Fatalf("Update error = %v, want sentinel", err)
	}
	loaded, err := store.Get(ctx, original.ID)
	if err != nil || loaded.Status != original.Status || loaded.InputRef != original.InputRef {
		t.Fatalf("Get = %+v, %v; want callback error to leave the run unchanged", loaded, err)
	}
}

func TestStoreUpdatePropagatesContextCancellation(t *testing.T) {
	store := openIsolatedStore(t)
	original := workflowkit.WorkflowRun{ID: "wf-context-cancel", Status: workflowkit.StatusRunning}
	if err := store.Save(t.Context(), original); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err := store.Update(ctx, original.ID, func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		run.Status = workflowkit.StatusFailed
		cancel()
		return run, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Update error = %v, want context.Canceled", err)
	}
	loaded, err := store.Get(t.Context(), original.ID)
	if err != nil || loaded.Status != original.Status {
		t.Fatalf("Get = %+v, %v; want cancellation to leave the run unchanged", loaded, err)
	}
}

func TestCreateRacingSameIDCreatesOnce(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	stores := []*Store{openStoreDB(t, openPool(t, dsn)), openStoreDB(t, openPool(t, dsn))}
	errs := make([]error, len(stores))
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Go(func() {
			errs[i] = store.Create(t.Context(), workflowkit.WorkflowRun{ID: "wf-race", Status: workflowkit.StatusPending, InputRef: fmt.Sprint(i)})
		})
	}
	wg.Wait()
	created, exists := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, workflowkit.ErrRunExists):
			exists++
		default:
			t.Fatalf("Create returned error: %v", err)
		}
	}
	if created != 1 || exists != 1 {
		t.Fatalf("created = %d, exists = %d; want one of each", created, exists)
	}
}

func TestClaimRunnableAcrossPoolsHasOneWinner(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	const replicas = 4
	stores := make([]*Store, replicas)
	for i := range stores {
		stores[i] = openStoreDB(t, openPool(t, dsn))
	}
	for round := range 20 {
		id := fmt.Sprintf("wf-claim-%02d", round)
		if err := stores[0].Save(t.Context(), workflowkit.WorkflowRun{ID: id, Status: workflowkit.StatusPending}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
		start := make(chan struct{})
		results := make([]error, replicas)
		claimed := make([]workflowkit.WorkflowRun, replicas)
		var wg sync.WaitGroup
		for i, store := range stores {
			wg.Go(func() {
				<-start
				claimed[i], results[i] = store.ClaimRunnable(t.Context(), fmt.Sprintf("worker-%d", i), time.Minute)
			})
		}
		close(start)
		wg.Wait()

		winners := 0
		for i, err := range results {
			switch {
			case err == nil:
				winners++
				if claimed[i].ID != id || claimed[i].LeaseOwner != fmt.Sprintf("worker-%d", i) {
					t.Fatalf("round %d: worker-%d claimed %+v", round, i, claimed[i])
				}
			case errors.Is(err, workflowkit.ErrNoRunnableWorkflow):
			default:
				t.Fatalf("round %d: ClaimRunnable returned error: %v", round, err)
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d workers claimed %s, want exactly one", round, winners, id)
		}
	}
}

func TestClaimRunnableSkipsRowLockedByUpdate(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	updater := openStoreDB(t, openPool(t, dsn))
	claimer := openStoreDB(t, openPool(t, dsn))
	ctx := t.Context()
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"wf-locked", "wf-free"} {
		if err := updater.Save(ctx, workflowkit.WorkflowRun{ID: id, Status: workflowkit.StatusPending, CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := updater.Update(ctx, "wf-locked", func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
			close(entered)
			<-release
			return run, nil
		})
		done <- err
	}()
	<-entered
	claimed, err := claimer.ClaimRunnable(ctx, "worker-1", time.Minute)
	close(release)
	if err != nil || claimed.ID != "wf-free" {
		t.Fatalf("ClaimRunnable = %s, %v; want the unlocked run", claimed.ID, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
}

func TestFailExpiredRunningAcrossPoolsReportsEachRunOnce(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	const replicas, runs = 4, 30
	stores := make([]*Store, replicas)
	for i := range stores {
		stores[i] = openStoreDB(t, openPool(t, dsn))
	}
	expired := time.Now().Add(-time.Minute)
	for i := range runs {
		if err := stores[0].Save(t.Context(), workflowkit.WorkflowRun{
			ID:         fmt.Sprintf("wf-expired-%02d", i),
			Status:     workflowkit.StatusRunning,
			LeaseOwner: "crashed-worker",
			LeaseUntil: expired,
		}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
	}
	start := make(chan struct{})
	reported := make([][]workflowkit.WorkflowRun, replicas)
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Go(func() {
			<-start
			reported[i], errs[i] = store.FailExpiredRunning(t.Context(), "lease_expired")
		})
	}
	close(start)
	wg.Wait()

	seen := map[string]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("FailExpiredRunning returned error: %v", err)
		}
		for _, run := range reported[i] {
			if run.Status != workflowkit.StatusFailed || run.LeaseOwner != "" {
				t.Fatalf("reported run = %+v, want failed with cleared lease", run)
			}
			seen[run.ID]++
		}
	}
	if len(seen) != runs {
		t.Fatalf("reported %d distinct runs, want %d", len(seen), runs)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("%s reported %d times, want once", id, count)
		}
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	db := openPool(t, isolatedSchemaDSN(t))
	for range 3 {
		openStoreDB(t, db)
	}
	var versions []int
	rows, err := db.QueryContext(t.Context(), `SELECT version FROM workflowkit_schema_versions ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema versions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0] != SchemaVersion {
		t.Fatalf("schema versions = %v, want [%d]", versions, SchemaVersion)
	}
}

func TestMigrationConcurrentOpensSucceed(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		db := openPool(t, dsn)
		wg.Go(func() {
			_, errs[i] = OpenDB(t.Context(), db)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent OpenDB returned error: %v", err)
		}
	}
}

func TestMigrationRejectsNewerSchemaVersionAndReleasesLock(t *testing.T) {
	db := openPool(t, isolatedSchemaDSN(t))
	openStoreDB(t, db)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workflowkit_schema_versions (version, applied_at) VALUES ($1, now())`, SchemaVersion+1); err != nil {
		t.Fatalf("insert newer schema version: %v", err)
	}
	if _, err := OpenDB(t.Context(), db); err == nil {
		t.Fatal("OpenDB accepted a newer schema version")
	}

	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var locked bool
	if err := conn.QueryRowContext(t.Context(), `SELECT pg_try_advisory_lock(hashtext('workflowkit.schema'))`).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("migration left the schema advisory lock held")
	}
	if _, err := conn.ExecContext(t.Context(), schemaUnlockSQL); err != nil {
		t.Fatal(err)
	}
}

func TestCloseLeavesCallerPoolOpen(t *testing.T) {
	db := openPool(t, isolatedSchemaDSN(t))
	store := openStoreDB(t, db)
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("caller pool closed by Store.Close: %v", err)
	}
}
