package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eruca/workflowkit"
	"github.com/eruca/workflowkit/storetest"
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

func TestQueueStoreConformance(t *testing.T) {
	storetest.RunQueueStoreConformance(t, func(t *testing.T) workflowkit.Store {
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
