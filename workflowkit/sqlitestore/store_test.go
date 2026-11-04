package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/eruca/goagents/workflowkit"
	"github.com/eruca/goagents/workflowkit/storetest"
)

func TestStoreConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) workflowkit.Store {
		store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
		if err != nil {
			t.Fatalf("Open returned error: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Fatalf("Close returned error: %v", err)
			}
		})
		return store
	})
}

func TestQueueRecoveryStoreConformance(t *testing.T) {
	storetest.RunQueueRecoveryStoreConformance(t, func(t *testing.T) workflowkit.Store {
		store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
		if err != nil {
			t.Fatalf("Open returned error: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Fatalf("Close returned error: %v", err)
			}
		})
		return store
	})
}

func TestWorkflowQueryStoreConformance(t *testing.T) {
	storetest.RunWorkflowQueryStoreConformance(t, func(t *testing.T) workflowkit.Store {
		store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
		if err != nil {
			t.Fatalf("Open returned error: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Fatalf("Close returned error: %v", err)
			}
		})
		return store
	})
}

func TestStorePersistsRunAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	started := time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC)
	ended := started.Add(time.Second)
	run := workflowkit.WorkflowRun{
		ID:             "wf-sqlite",
		Status:         workflowkit.StatusWaitingApproval,
		InputRef:       "artifact:in",
		OutputRef:      "artifact:out",
		AgentRunID:     "agent:run-1",
		AuditRef:       "audit:run-1",
		ApprovalRef:    "approval:req-1",
		WaitingReason:  "operator approval required",
		CurrentStep:    "agent",
		CompletedSteps: []string{"prepare", "agent"},
		StepAttempts:   map[string]int{"prepare": 1, "agent": 1},
		StepRecords: []workflowkit.StepRecord{{
			Name:          "agent",
			Status:        workflowkit.StatusWaitingApproval,
			Attempt:       1,
			AgentRunID:    "agent:run-1",
			ApprovalRef:   "approval:req-1",
			WaitingReason: "operator approval required",
			StartedAt:     started,
			EndedAt:       ended,
			Metadata:      map[string]any{"tool": "write_file"},
		}},
		Metadata: map[string]any{"tenant": "demo"},
	}
	if err := store.Save(ctx, run); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen returned error: %v", err)
	}
	defer reopened.Close()

	loaded, err := reopened.Get(ctx, "wf-sqlite")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if loaded.Status != workflowkit.StatusWaitingApproval || loaded.InputRef != "artifact:in" || loaded.ApprovalRef != "approval:req-1" {
		t.Fatalf("loaded = %#v", loaded)
	}
	if loaded.Metadata["tenant"] != "demo" {
		t.Fatalf("metadata = %#v", loaded.Metadata)
	}
	if len(loaded.StepRecords) != 1 || loaded.StepRecords[0].Metadata["tool"] != "write_file" {
		t.Fatalf("step records = %#v", loaded.StepRecords)
	}
}

func TestStoreUpdateSerializesConcurrentSave(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	ctx := t.Context()
	if err := store.Save(ctx, workflowkit.WorkflowRun{
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
		_, err := store.Update(ctx, "wf-atomic-update", func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
			close(callbackEntered)
			<-releaseCallback
			run.Status = workflowkit.StatusFailed
			run.Error = "updated"
			return run, nil
		})
		updateResult <- err
	}()
	<-callbackEntered

	saveCtx, cancelSave := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelSave()
	saveResult := make(chan error, 1)
	go func() {
		saveResult <- store.Save(saveCtx, workflowkit.WorkflowRun{
			ID:       "wf-atomic-update",
			Status:   workflowkit.StatusSucceeded,
			InputRef: "artifact:concurrent-input",
			Metadata: map[string]any{"source": "concurrent-save"},
		})
	}()

	saveErr := <-saveResult
	close(releaseCallback)
	updateErr := <-updateResult

	if saveErr == nil {
		final, getErr := store.Get(ctx, "wf-atomic-update")
		if getErr != nil {
			t.Fatalf("Get after non-serialized Save returned error: %v", getErr)
		}
		t.Fatalf("concurrent Save completed inside Update callback and was overwritten: final=%+v", final)
	}
	if !errors.Is(saveErr, context.DeadlineExceeded) {
		t.Fatalf("concurrent Save error = %v, want context deadline while Update owns the Store connection", saveErr)
	}
	if updateErr != nil {
		t.Fatalf("Update returned error: %v", updateErr)
	}

	final, err := store.Get(ctx, "wf-atomic-update")
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
	store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	ctx := t.Context()
	original := workflowkit.WorkflowRun{
		ID:       "wf-callback-error",
		Status:   workflowkit.StatusRunning,
		InputRef: "artifact:original",
	}
	if err := store.Save(ctx, original); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	sentinel := errors.New("mutate failed")
	_, err = store.Update(ctx, original.ID, func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		run.Status = workflowkit.StatusFailed
		run.InputRef = "artifact:changed"
		return run, sentinel
	})
	if err != sentinel {
		t.Fatalf("Update error = %v, want sentinel", err)
	}

	loaded, err := store.Get(ctx, original.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if loaded.Status != original.Status || loaded.InputRef != original.InputRef {
		t.Fatalf("callback error changed stored run: %+v", loaded)
	}
	if err := store.Save(ctx, workflowkit.WorkflowRun{ID: "wf-after-rollback", Status: workflowkit.StatusPending}); err != nil {
		t.Fatalf("Save after callback rollback returned error: %v", err)
	}
}

func TestStoreUpdatePropagatesContextCancellation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	original := workflowkit.WorkflowRun{
		ID:     "wf-context-cancel",
		Status: workflowkit.StatusRunning,
	}
	if err := store.Save(t.Context(), original); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	_, err = store.Update(ctx, original.ID, func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		run.Status = workflowkit.StatusFailed
		cancel()
		return run, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Update error = %v, want context.Canceled", err)
	}

	loaded, err := store.Get(t.Context(), original.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if loaded.Status != original.Status {
		t.Fatalf("context cancellation changed stored run: %+v", loaded)
	}
}

func TestStoreMigratesSchemaVersion(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	var version int
	err = store.db.QueryRow(`SELECT version FROM workflowkit_schema WHERE id = 'sqlitestore'`).Scan(&version)
	if err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, SchemaVersion)
	}
}

func TestOpenConvertsLegacyTextTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Schema version 2 let the driver store times as zone-qualified text.
	if _, err := legacy.Exec(`
CREATE TABLE workflow_runs (
	id TEXT PRIMARY KEY, status TEXT NOT NULL,
	input_ref TEXT NOT NULL DEFAULT '', output_ref TEXT NOT NULL DEFAULT '',
	agent_run_id TEXT NOT NULL DEFAULT '', audit_ref TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '', approval_ref TEXT NOT NULL DEFAULT '',
	waiting_reason TEXT NOT NULL DEFAULT '', current_step TEXT NOT NULL DEFAULT '',
	completed_steps_json TEXT NOT NULL DEFAULT '[]', step_attempts_json TEXT NOT NULL DEFAULT '{}',
	step_records_json TEXT NOT NULL DEFAULT '[]', metadata_json TEXT NOT NULL DEFAULT '{}',
	lease_owner TEXT NOT NULL DEFAULT '',
	lease_until TIMESTAMP NOT NULL DEFAULT '0001-01-01T00:00:00Z',
	created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL
)`); err != nil {
		t.Fatal(err)
	}
	utc9 := time.FixedZone("UTC+9", 9*3600)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	insert := `INSERT INTO workflow_runs (id, status, lease_owner, lease_until, created_at, updated_at) VALUES (?, 'pending', ?, ?, ?, ?)`
	// wf-older was created first but in a zone whose text sorts later, and
	// its lease expired a minute ago.
	if _, err := legacy.Exec(insert, "wf-older", "crashed-worker", time.Now().Add(-time.Minute).In(utc9), base.In(utc9), base.In(utc9)); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(insert, "wf-newer", "", time.Time{}, base.Add(time.Minute).UTC(), base.UTC()); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	var textTimes int
	if err := store.db.QueryRow(`SELECT count(*) FROM workflow_runs WHERE typeof(created_at) = 'text' OR typeof(lease_until) = 'text'`).Scan(&textTimes); err != nil || textTimes != 0 {
		t.Fatalf("text times left = %d, %v; want all converted", textTimes, err)
	}
	older, err := store.Get(context.Background(), "wf-older")
	if err != nil || !older.CreatedAt.Equal(base) {
		t.Fatalf("Get(wf-older) = %+v, %v; want created at %s", older, err, base)
	}
	claimed, err := store.ClaimRunnable(context.Background(), "worker-1", time.Minute)
	if err != nil || claimed.ID != "wf-older" {
		t.Fatalf("ClaimRunnable = %s, %v; want the older run with the expired lease", claimed.ID, err)
	}
}

func TestStoreSaveWritesOnlyChangedStepRecords(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	run := workflowkit.WorkflowRun{ID: "wf-records", Status: workflowkit.StatusRunning}
	for i := range 50 {
		run.StepRecords = append(run.StepRecords, workflowkit.StepRecord{Name: "step", Attempt: i + 1, Status: workflowkit.StatusSucceeded})
	}
	if err := store.Save(ctx, run); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	changes := func() int {
		var n int
		if err := store.db.QueryRow(`SELECT total_changes()`).Scan(&n); err != nil {
			t.Fatalf("total_changes: %v", err)
		}
		return n
	}
	before := changes()
	updated, err := store.Update(ctx, run.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		current.StepRecords = append(current.StepRecords, workflowkit.StepRecord{Name: "next", Attempt: 1, Status: workflowkit.StatusSucceeded})
		return current, nil
	})
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	// One upsert of the run row and one inserted record.
	if got := changes() - before; got != 2 {
		t.Fatalf("rows changed by appending one record = %d, want 2", got)
	}
	if len(updated.StepRecords) != 51 || updated.StepRecords[50].Name != "next" {
		t.Fatalf("records = %d, last = %+v", len(updated.StepRecords), updated.StepRecords[len(updated.StepRecords)-1])
	}

	updated.StepRecords = updated.StepRecords[:2]
	updated.StepRecords[1].Error = "rewritten"
	if err := store.Save(ctx, updated); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	loaded, err := store.Get(ctx, run.ID)
	if err != nil || len(loaded.StepRecords) != 2 || loaded.StepRecords[1].Error != "rewritten" {
		t.Fatalf("Get = %+v, %v; want the truncated, rewritten records", loaded.StepRecords, err)
	}
}

func TestOpenMovesInlineStepRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	// Schema version 3 kept step records inline as a JSON array.
	if _, err := store.db.Exec(`INSERT INTO workflow_runs (id, status, step_records_json, created_at, updated_at) VALUES ('wf-legacy', 'succeeded', ?, 1, 1)`,
		`[{"Name":"extract","Status":"succeeded","Attempt":1},{"Name":"review","Status":"failed","Attempt":2,"Error":"boom"}]`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen returned error: %v", err)
	}
	defer store.Close()
	loaded, err := store.Get(context.Background(), "wf-legacy")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if len(loaded.StepRecords) != 2 || loaded.StepRecords[0].Name != "extract" || loaded.StepRecords[1].Error != "boom" {
		t.Fatalf("records = %+v, want both legacy records in order", loaded.StepRecords)
	}
	var inline string
	if err := store.db.QueryRow(`SELECT step_records_json FROM workflow_runs WHERE id = 'wf-legacy'`).Scan(&inline); err != nil || inline != "[]" {
		t.Fatalf("inline records = %q, %v; want cleared", inline, err)
	}
}
