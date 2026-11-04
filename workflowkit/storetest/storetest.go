package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eruca/goagents/workflowkit"
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

	t.Run("create never replaces a stored run", func(t *testing.T) {
		store := newStore(t)
		creator, ok := store.(workflowkit.CreateStore)
		if !ok {
			t.Skip("store does not implement workflowkit.CreateStore")
		}
		if err := creator.Create(context.Background(), workflowkit.WorkflowRun{ID: "wf-create", Status: workflowkit.StatusWaitingApproval}); err != nil {
			t.Fatalf("Create returned error: %v", err)
		}
		err := creator.Create(context.Background(), workflowkit.WorkflowRun{ID: "wf-create", Status: workflowkit.StatusPending})
		if !errors.Is(err, workflowkit.ErrRunExists) {
			t.Fatalf("second Create error = %v, want ErrRunExists", err)
		}
		loaded, err := store.Get(context.Background(), "wf-create")
		if err != nil || loaded.Status != workflowkit.StatusWaitingApproval {
			t.Fatalf("Get = %+v, %v; want the first run untouched", loaded, err)
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

func RunQueueStoreConformance(t *testing.T, newStore NewStore) {
	t.Helper()
	RunStoreConformance(t, newStore)

	t.Run("claim skips a retry until it is due and claims oldest first", func(t *testing.T) {
		store := queueStore(t, newStore)
		base := time.Now().Add(-time.Hour)
		// Created times in different zones must still order by instant.
		older := base.In(time.FixedZone("UTC+9", 9*3600))
		newer := base.Add(time.Minute).UTC()
		for _, run := range []workflowkit.WorkflowRun{
			{ID: "wf-retry-later", Status: workflowkit.StatusPending, CreatedAt: base.Add(-time.Minute), NextAttemptAt: time.Now().Add(time.Hour), StepRetries: 1},
			{ID: "wf-newer", Status: workflowkit.StatusPending, CreatedAt: newer},
			{ID: "wf-older", Status: workflowkit.StatusPending, CreatedAt: older},
		} {
			if err := store.Save(context.Background(), run); err != nil {
				t.Fatalf("Save(%s) returned error: %v", run.ID, err)
			}
		}
		for _, want := range []string{"wf-older", "wf-newer"} {
			claimed, err := store.ClaimRunnable(context.Background(), "worker-1", time.Minute)
			if err != nil || claimed.ID != want {
				t.Fatalf("ClaimRunnable = %s, %v; want %s", claimed.ID, err, want)
			}
		}
		if claimed, err := store.ClaimRunnable(context.Background(), "worker-1", time.Minute); !errors.Is(err, workflowkit.ErrNoRunnableWorkflow) {
			t.Fatalf("ClaimRunnable = %s, %v; want the retry held back", claimed.ID, err)
		}

		if _, err := store.Update(context.Background(), "wf-retry-later", func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
			run.NextAttemptAt = time.Now().Add(-time.Second)
			return run, nil
		}); err != nil {
			t.Fatalf("Update returned error: %v", err)
		}
		claimed, err := store.ClaimRunnable(context.Background(), "worker-1", time.Minute)
		if err != nil || claimed.ID != "wf-retry-later" || claimed.StepRetries != 1 {
			t.Fatalf("ClaimRunnable = %+v, %v; want due retry with its retry count", claimed, err)
		}
	})

	t.Run("claim runnable pending workflow with lease", func(t *testing.T) {
		store := queueStore(t, newStore)
		if err := store.Save(context.Background(), workflowkit.WorkflowRun{
			ID:     "wf-claim",
			Status: workflowkit.StatusPending,
		}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}

		claimed, err := store.ClaimRunnable(context.Background(), "worker-1", time.Minute)
		if err != nil {
			t.Fatalf("ClaimRunnable returned error: %v", err)
		}
		if claimed.ID != "wf-claim" || claimed.Status != workflowkit.StatusPending {
			t.Fatalf("claimed = %+v, want pending wf-claim", claimed)
		}
		if claimed.LeaseOwner != "worker-1" || claimed.LeaseUntil.IsZero() {
			t.Fatalf("claim lease fields = %+v", claimed)
		}

		stored, err := store.Get(context.Background(), "wf-claim")
		if err != nil {
			t.Fatalf("Get returned error: %v", err)
		}
		if stored.LeaseOwner != "worker-1" || stored.LeaseUntil.IsZero() {
			t.Fatalf("stored lease fields = %+v", stored)
		}
	})

	t.Run("claim skips active lease and reclaims expired lease", func(t *testing.T) {
		store := queueStore(t, newStore)
		future := time.Now().Add(time.Hour).UTC()
		if err := store.Save(context.Background(), workflowkit.WorkflowRun{
			ID:         "wf-active-lease",
			Status:     workflowkit.StatusPending,
			LeaseOwner: "worker-1",
			LeaseUntil: future,
		}); err != nil {
			t.Fatalf("Save active lease returned error: %v", err)
		}
		if _, err := store.ClaimRunnable(context.Background(), "worker-2", time.Minute); !errors.Is(err, workflowkit.ErrNoRunnableWorkflow) {
			t.Fatalf("active lease claim err = %v, want ErrNoRunnableWorkflow", err)
		}

		expired := time.Now().Add(-time.Minute).UTC()
		if err := store.Save(context.Background(), workflowkit.WorkflowRun{
			ID:         "wf-active-lease",
			Status:     workflowkit.StatusPending,
			LeaseOwner: "worker-1",
			LeaseUntil: expired,
		}); err != nil {
			t.Fatalf("Save expired lease returned error: %v", err)
		}
		claimed, err := store.ClaimRunnable(context.Background(), "worker-2", time.Minute)
		if err != nil {
			t.Fatalf("ClaimRunnable expired lease returned error: %v", err)
		}
		if claimed.LeaseOwner != "worker-2" || !claimed.LeaseUntil.After(time.Now()) {
			t.Fatalf("reclaimed lease fields = %+v", claimed)
		}
	})

	t.Run("claim ignores non pending workflows", func(t *testing.T) {
		store := queueStore(t, newStore)
		for _, run := range []workflowkit.WorkflowRun{
			{ID: "wf-running", Status: workflowkit.StatusRunning},
			{ID: "wf-waiting", Status: workflowkit.StatusWaitingApproval},
			{ID: "wf-succeeded", Status: workflowkit.StatusSucceeded},
		} {
			if err := store.Save(context.Background(), run); err != nil {
				t.Fatalf("Save(%s) returned error: %v", run.ID, err)
			}
		}
		if _, err := store.ClaimRunnable(context.Background(), "worker-1", time.Minute); !errors.Is(err, workflowkit.ErrNoRunnableWorkflow) {
			t.Fatalf("claim err = %v, want ErrNoRunnableWorkflow", err)
		}
	})
}

func RunQueueLeaseStoreConformance(t *testing.T, newStore NewStore) {
	t.Helper()
	RunQueueStoreConformance(t, newStore)

	t.Run("extend lease requires current active owner", func(t *testing.T) {
		store := queueLeaseStore(t, newStore)
		originalLease := time.Now().Add(time.Minute).UTC()
		if err := store.Save(context.Background(), workflowkit.WorkflowRun{
			ID:         "wf-extend-lease",
			Status:     workflowkit.StatusRunning,
			LeaseOwner: "worker-1",
			LeaseUntil: originalLease,
		}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}

		if _, err := store.ExtendLease(context.Background(), "wf-extend-lease", "worker-2", time.Minute); !errors.Is(err, workflowkit.ErrWorkflowLeaseNotOwned) {
			t.Fatalf("wrong owner ExtendLease err = %v, want ErrWorkflowLeaseNotOwned", err)
		}

		extended, err := store.ExtendLease(context.Background(), "wf-extend-lease", "worker-1", 2*time.Minute)
		if err != nil {
			t.Fatalf("ExtendLease returned error: %v", err)
		}
		if extended.LeaseOwner != "worker-1" || !extended.LeaseUntil.After(originalLease) {
			t.Fatalf("extended lease = %+v, want same owner with later deadline", extended)
		}
	})

	t.Run("extend lease rejects expired ownership", func(t *testing.T) {
		store := queueLeaseStore(t, newStore)
		if err := store.Save(context.Background(), workflowkit.WorkflowRun{
			ID:         "wf-expired-extend",
			Status:     workflowkit.StatusRunning,
			LeaseOwner: "worker-1",
			LeaseUntil: time.Now().Add(-time.Minute).UTC(),
		}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}

		if _, err := store.ExtendLease(context.Background(), "wf-expired-extend", "worker-1", time.Minute); !errors.Is(err, workflowkit.ErrWorkflowLeaseNotOwned) {
			t.Fatalf("expired ExtendLease err = %v, want ErrWorkflowLeaseNotOwned", err)
		}
	})

	t.Run("release lease clears current owner only", func(t *testing.T) {
		store := queueLeaseStore(t, newStore)
		if err := store.Save(context.Background(), workflowkit.WorkflowRun{
			ID:         "wf-release-lease",
			Status:     workflowkit.StatusWaitingApproval,
			LeaseOwner: "worker-1",
			LeaseUntil: time.Now().Add(time.Minute).UTC(),
		}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}

		if _, err := store.ReleaseLease(context.Background(), "wf-release-lease", "worker-2"); !errors.Is(err, workflowkit.ErrWorkflowLeaseNotOwned) {
			t.Fatalf("wrong owner ReleaseLease err = %v, want ErrWorkflowLeaseNotOwned", err)
		}

		released, err := store.ReleaseLease(context.Background(), "wf-release-lease", "worker-1")
		if err != nil {
			t.Fatalf("ReleaseLease returned error: %v", err)
		}
		if released.LeaseOwner != "" || !released.LeaseUntil.IsZero() {
			t.Fatalf("released lease = %+v, want cleared lease", released)
		}
	})
}

func RunQueueRecoveryStoreConformance(t *testing.T, newStore NewStore) {
	t.Helper()
	RunQueueLeaseStoreConformance(t, newStore)

	t.Run("released running workflow is still recovered", func(t *testing.T) {
		store := queueRecoveryStore(t, newStore)
		if err := store.Save(context.Background(), workflowkit.WorkflowRun{
			ID:          "wf-released-running",
			Status:      workflowkit.StatusRunning,
			LeaseOwner:  "worker-1",
			LeaseUntil:  time.Now().Add(time.Hour).UTC(),
			StepRetries: 2,
		}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
		released, err := store.ReleaseLease(context.Background(), "wf-released-running", "worker-1")
		if err != nil {
			t.Fatalf("ReleaseLease returned error: %v", err)
		}
		if released.LeaseOwner != "worker-1" {
			t.Fatalf("released running lease owner = %q, want it kept for recovery", released.LeaseOwner)
		}
		failed, err := store.FailExpiredRunning(context.Background(), "worker stopped")
		if err != nil {
			t.Fatalf("FailExpiredRunning returned error: %v", err)
		}
		if len(failed) != 1 || failed[0].ID != "wf-released-running" || failed[0].Status != workflowkit.StatusFailed {
			t.Fatalf("failed = %+v, want the released running workflow", failed)
		}
		if failed[0].StepRetries != 0 || !failed[0].NextAttemptAt.IsZero() {
			t.Fatalf("failed run retry state = %d, %s; want reset so a requeue starts a fresh budget", failed[0].StepRetries, failed[0].NextAttemptAt)
		}
	})

	t.Run("fail expired running fails only leased running workflows past their lease", func(t *testing.T) {
		store := queueRecoveryStore(t, newStore)
		expired := time.Now().Add(-time.Minute).UTC()
		active := time.Now().Add(time.Hour).UTC()
		for _, run := range []workflowkit.WorkflowRun{
			{ID: "wf-expired", Status: workflowkit.StatusRunning, LeaseOwner: "worker-1", LeaseUntil: expired, CompletedSteps: []string{"prepare"}},
			{ID: "wf-active", Status: workflowkit.StatusRunning, LeaseOwner: "worker-1", LeaseUntil: active},
			{ID: "wf-unleased", Status: workflowkit.StatusRunning},
			{ID: "wf-pending-expired", Status: workflowkit.StatusPending, LeaseOwner: "worker-1", LeaseUntil: expired},
			{ID: "wf-waiting-expired", Status: workflowkit.StatusWaitingApproval, LeaseOwner: "worker-1", LeaseUntil: expired},
		} {
			if err := store.Save(context.Background(), run); err != nil {
				t.Fatalf("Save(%s) returned error: %v", run.ID, err)
			}
		}

		failed, err := store.FailExpiredRunning(context.Background(), "lease_expired")
		if err != nil {
			t.Fatalf("FailExpiredRunning returned error: %v", err)
		}
		if !equalStrings(workflowIDs(failed), []string{"wf-expired"}) {
			t.Fatalf("failed = %v, want [wf-expired]", workflowIDs(failed))
		}
		stored, err := store.Get(context.Background(), "wf-expired")
		if err != nil {
			t.Fatalf("Get returned error: %v", err)
		}
		if stored.Status != workflowkit.StatusFailed || stored.Error != "lease_expired" || stored.LeaseOwner != "" || !stored.LeaseUntil.IsZero() {
			t.Fatalf("stored = %+v, want failed lease_expired with cleared lease", stored)
		}
		if !equalStrings(stored.CompletedSteps, []string{"prepare"}) {
			t.Fatalf("completed steps = %v, want progress kept for requeue", stored.CompletedSteps)
		}
		for id, want := range map[string]workflowkit.Status{
			"wf-active":          workflowkit.StatusRunning,
			"wf-unleased":        workflowkit.StatusRunning,
			"wf-pending-expired": workflowkit.StatusPending,
			"wf-waiting-expired": workflowkit.StatusWaitingApproval,
		} {
			run, err := store.Get(context.Background(), id)
			if err != nil {
				t.Fatalf("Get(%s) returned error: %v", id, err)
			}
			if run.Status != want {
				t.Fatalf("%s status = %s, want %s", id, run.Status, want)
			}
		}

		again, err := store.FailExpiredRunning(context.Background(), "lease_expired")
		if err != nil || len(again) != 0 {
			t.Fatalf("second FailExpiredRunning = %v, %v; want none", workflowIDs(again), err)
		}
	})
}

func RunWorkflowQueryStoreConformance(t *testing.T, newStore NewStore) {
	t.Helper()
	RunStoreConformance(t, newStore)

	t.Run("list workflows matches numeric metadata as written", func(t *testing.T) {
		store := workflowQueryStore(t, newStore)
		if err := store.Save(context.Background(), workflowkit.WorkflowRun{ID: "wf-number", Status: workflowkit.StatusPending, Metadata: map[string]any{"tenant": 1234567}}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
		runs, err := store.ListWorkflows(context.Background(), workflowkit.WorkflowQuery{MetadataEquals: map[string]string{"tenant": "1234567"}})
		if err != nil || len(runs) != 1 {
			t.Fatalf("ListWorkflows = %d runs, %v; want the numeric match", len(runs), err)
		}
	})

	t.Run("list workflows filters by status and limit", func(t *testing.T) {
		store := workflowQueryStore(t, newStore)
		base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
		runs := []workflowkit.WorkflowRun{
			{ID: "wf-pending-b", Status: workflowkit.StatusPending, CreatedAt: base.Add(2 * time.Minute), Metadata: map[string]any{"order": "second"}},
			{ID: "wf-waiting", Status: workflowkit.StatusWaitingApproval, CreatedAt: base.Add(time.Minute)},
			{ID: "wf-pending-a", Status: workflowkit.StatusPending, CreatedAt: base, Metadata: map[string]any{"order": "first"}},
			{ID: "wf-failed", Status: workflowkit.StatusFailed, CreatedAt: base.Add(3 * time.Minute)},
		}
		for _, run := range runs {
			if err := store.Save(context.Background(), run); err != nil {
				t.Fatalf("Save(%s) returned error: %v", run.ID, err)
			}
		}

		listed, err := store.ListWorkflows(context.Background(), workflowkit.WorkflowQuery{
			Status: workflowkit.StatusPending,
			Limit:  1,
		})
		if err != nil {
			t.Fatalf("ListWorkflows returned error: %v", err)
		}
		if got := workflowIDs(listed); !equalStrings(got, []string{"wf-pending-a"}) {
			t.Fatalf("listed ids = %v, want oldest pending limited to one", got)
		}

		listed[0].Metadata["order"] = "mutated"
		again, err := store.ListWorkflows(context.Background(), workflowkit.WorkflowQuery{
			Status: workflowkit.StatusPending,
			Limit:  1,
		})
		if err != nil {
			t.Fatalf("ListWorkflows returned error: %v", err)
		}
		if again[0].Metadata["order"] != "first" {
			t.Fatalf("listed workflow mutated stored copy: %#v", again[0].Metadata)
		}
	})

	t.Run("list workflows returns all statuses in stable order", func(t *testing.T) {
		store := workflowQueryStore(t, newStore)
		base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
		for _, run := range []workflowkit.WorkflowRun{
			{ID: "wf-b", Status: workflowkit.StatusSucceeded, CreatedAt: base.Add(time.Minute)},
			{ID: "wf-a", Status: workflowkit.StatusPending, CreatedAt: base.Add(time.Minute)},
			{ID: "wf-old", Status: workflowkit.StatusFailed, CreatedAt: base},
		} {
			if err := store.Save(context.Background(), run); err != nil {
				t.Fatalf("Save(%s) returned error: %v", run.ID, err)
			}
		}

		listed, err := store.ListWorkflows(context.Background(), workflowkit.WorkflowQuery{})
		if err != nil {
			t.Fatalf("ListWorkflows returned error: %v", err)
		}
		if got := workflowIDs(listed); !equalStrings(got, []string{"wf-old", "wf-a", "wf-b"}) {
			t.Fatalf("listed ids = %v, want created_at then id order", got)
		}
	})

	t.Run("list workflows filters metadata and applies descending limit after filtering", func(t *testing.T) {
		store := workflowQueryStore(t, newStore)
		base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
		for _, run := range []workflowkit.WorkflowRun{
			{ID: "wf-sync-new", Status: workflowkit.StatusPending, CreatedAt: base.Add(3 * time.Minute), Metadata: map[string]any{"run_mode": "sync"}},
			{ID: "wf-queued-old", Status: workflowkit.StatusPending, CreatedAt: base, Metadata: map[string]any{"run_mode": "queued"}},
			{ID: "wf-queued-new", Status: workflowkit.StatusPending, CreatedAt: base.Add(2 * time.Minute), Metadata: map[string]any{"run_mode": "queued"}},
		} {
			if err := store.Save(context.Background(), run); err != nil {
				t.Fatalf("Save(%s) returned error: %v", run.ID, err)
			}
		}

		listed, err := store.ListWorkflows(context.Background(), workflowkit.WorkflowQuery{
			Status:         workflowkit.StatusPending,
			MetadataEquals: map[string]string{"run_mode": "queued"},
			Order:          workflowkit.WorkflowOrderDesc,
			Limit:          1,
		})
		if err != nil {
			t.Fatalf("ListWorkflows returned error: %v", err)
		}
		if got := workflowIDs(listed); !equalStrings(got, []string{"wf-queued-new"}) {
			t.Fatalf("listed ids = %v, want newest queued workflow", got)
		}
	})
}

func queueStore(t *testing.T, newStore NewStore) workflowkit.QueueStore {
	t.Helper()
	store := newStore(t)
	queue, ok := store.(workflowkit.QueueStore)
	if !ok {
		t.Fatalf("%T does not implement workflowkit.QueueStore", store)
	}
	return queue
}

func workflowQueryStore(t *testing.T, newStore NewStore) workflowkit.WorkflowQueryStore {
	t.Helper()
	store := newStore(t)
	query, ok := store.(workflowkit.WorkflowQueryStore)
	if !ok {
		t.Fatalf("%T does not implement workflowkit.WorkflowQueryStore", store)
	}
	return query
}

func workflowIDs(runs []workflowkit.WorkflowRun) []string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func queueLeaseStore(t *testing.T, newStore NewStore) workflowkit.QueueLeaseStore {
	t.Helper()
	store := newStore(t)
	queue, ok := store.(workflowkit.QueueLeaseStore)
	if !ok {
		t.Fatalf("%T does not implement workflowkit.QueueLeaseStore", store)
	}
	return queue
}

func queueRecoveryStore(t *testing.T, newStore NewStore) workflowkit.QueueRecoveryStore {
	t.Helper()
	store := newStore(t)
	recovery, ok := store.(workflowkit.QueueRecoveryStore)
	if !ok {
		t.Fatalf("%T does not implement workflowkit.QueueRecoveryStore", store)
	}
	return recovery
}
