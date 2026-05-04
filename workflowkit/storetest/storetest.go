package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/eruca/workflowkit"
)

type NewStore func(*testing.T) workflowkit.Store

func RunStoreConformance(t *testing.T, newStore NewStore) {
	t.Helper()

	t.Run("save get returns copies", func(t *testing.T) {
		store := newStore(t)
		run := workflowkit.WorkflowRun{
			ID:       "wf-1",
			Status:   workflowkit.StatusPending,
			Metadata: map[string]any{"k": "v"},
			StepRecords: []workflowkit.StepRecord{{
				Name:     "prepare",
				Status:   workflowkit.StatusSucceeded,
				Metadata: map[string]any{"record": "original"},
			}},
		}
		if err := store.Save(context.Background(), run); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}

		loaded, err := store.Get(context.Background(), "wf-1")
		if err != nil {
			t.Fatalf("Get returned error: %v", err)
		}
		loaded.Status = workflowkit.StatusSucceeded
		loaded.Metadata["k"] = "changed"
		loaded.StepRecords[0].Metadata["record"] = "changed"

		again, err := store.Get(context.Background(), "wf-1")
		if err != nil {
			t.Fatalf("Get returned error: %v", err)
		}
		if again.Status != workflowkit.StatusPending {
			t.Fatalf("status mutated through loaded copy: %s", again.Status)
		}
		if again.Metadata["k"] != "v" {
			t.Fatalf("metadata mutated through loaded copy: %#v", again.Metadata)
		}
		if again.StepRecords[0].Metadata["record"] != "original" {
			t.Fatalf("record metadata mutated through loaded copy: %#v", again.StepRecords)
		}
	})

	t.Run("update mutates stored copy", func(t *testing.T) {
		store := newStore(t)
		if err := store.Save(context.Background(), workflowkit.WorkflowRun{
			ID:       "wf-update",
			Status:   workflowkit.StatusWaitingApproval,
			Metadata: map[string]any{"approved": false},
		}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}

		updated, err := store.Update(context.Background(), "wf-update", func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
			run.Metadata["approved"] = true
			run.AuditRef = "audit:approval-1"
			return run, nil
		})
		if err != nil {
			t.Fatalf("Update returned error: %v", err)
		}
		if updated.Metadata["approved"] != true || updated.AuditRef != "audit:approval-1" {
			t.Fatalf("updated = %#v", updated)
		}

		updated.Metadata["approved"] = false
		again, err := store.Get(context.Background(), "wf-update")
		if err != nil {
			t.Fatalf("Get returned error: %v", err)
		}
		if again.Metadata["approved"] != true || again.AuditRef != "audit:approval-1" {
			t.Fatalf("stored copy mutated through updated result: %#v", again)
		}
	})

	t.Run("update returns not found", func(t *testing.T) {
		store := newStore(t)
		_, err := store.Update(context.Background(), "missing", func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
			return run, nil
		})
		if !errors.Is(err, workflowkit.ErrRunNotFound) {
			t.Fatalf("err = %v, want ErrRunNotFound", err)
		}
	})

	t.Run("get returns not found", func(t *testing.T) {
		store := newStore(t)
		_, err := store.Get(context.Background(), "missing")
		if !errors.Is(err, workflowkit.ErrRunNotFound) {
			t.Fatalf("err = %v, want ErrRunNotFound", err)
		}
	})
}
