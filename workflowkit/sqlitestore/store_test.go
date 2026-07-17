package sqlitestore

import (
	"context"
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

func TestQueueLeaseStoreConformance(t *testing.T) {
	storetest.RunQueueLeaseStoreConformance(t, func(t *testing.T) workflowkit.Store {
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
