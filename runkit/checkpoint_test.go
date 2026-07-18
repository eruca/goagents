package runkit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCheckpointApproveAndLeaseIsAtomic(t *testing.T) {
	store := NewMemoryCheckpointStore()
	now := time.Now().UTC()
	checkpoint := ApprovalCheckpoint{
		ID:             "checkpoint-1",
		RunID:          "run-1",
		TenantID:       "tenant-1",
		DefinitionHash: "agent-v1",
		Ciphertext:     []byte("ciphertext"),
		ExpiresAt:      now.Add(time.Hour),
	}
	if err := store.CreateCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	leased, err := store.ApproveAndLease(context.Background(), ApprovalLeaseRequest{
		CheckpointID:   checkpoint.ID,
		TenantID:       checkpoint.TenantID,
		DefinitionHash: checkpoint.DefinitionHash,
		ApproverID:     "operator-1",
		AuditRef:       "audit-1",
		LeaseOwner:     "worker-1",
		LeaseDuration:  time.Minute,
		Now:            now,
	})
	if err != nil {
		t.Fatalf("ApproveAndLease: %v", err)
	}
	if leased.Status != CheckpointLeased || leased.Approval == nil || leased.Approval.ApproverID != "operator-1" {
		t.Fatalf("leased = %#v", leased)
	}
	leased.Ciphertext[0] = 'X'

	_, err = store.ApproveAndLease(context.Background(), ApprovalLeaseRequest{
		CheckpointID:   checkpoint.ID,
		TenantID:       checkpoint.TenantID,
		DefinitionHash: checkpoint.DefinitionHash,
		ApproverID:     "operator-2",
		AuditRef:       "audit-2",
		LeaseOwner:     "worker-2",
		LeaseDuration:  time.Minute,
		Now:            now,
	})
	if !errors.Is(err, ErrCheckpointNotClaimable) {
		t.Fatalf("second lease error = %v, want ErrCheckpointNotClaimable", err)
	}

	if err := store.CompleteLease(context.Background(), CheckpointLeaseCompletion{
		CheckpointID: checkpoint.ID,
		TenantID:     checkpoint.TenantID,
		LeaseOwner:   "worker-1",
		Now:          now.Add(time.Second),
	}); err != nil {
		t.Fatalf("CompleteLease: %v", err)
	}
	stored, err := store.GetCheckpoint(context.Background(), checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if stored.Status != CheckpointConsumed || string(stored.Ciphertext) != "ciphertext" {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestCheckpointRejectsWrongBindingAndExpiresWithoutRetry(t *testing.T) {
	store := NewMemoryCheckpointStore()
	now := time.Now().UTC()
	checkpoint := ApprovalCheckpoint{
		ID:             "checkpoint-2",
		RunID:          "run-2",
		TenantID:       "tenant-2",
		DefinitionHash: "agent-v2",
		Ciphertext:     []byte("ciphertext"),
		ExpiresAt:      now.Add(time.Minute),
	}
	if err := store.CreateCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	_, err := store.ApproveAndLease(context.Background(), ApprovalLeaseRequest{
		CheckpointID:   checkpoint.ID,
		TenantID:       "other-tenant",
		DefinitionHash: checkpoint.DefinitionHash,
		ApproverID:     "operator-1",
		AuditRef:       "audit-1",
		LeaseOwner:     "worker-1",
		LeaseDuration:  time.Minute,
		Now:            now,
	})
	if !errors.Is(err, ErrCheckpointNotClaimable) {
		t.Fatalf("binding error = %v, want ErrCheckpointNotClaimable", err)
	}

	expired, err := store.ExpireCheckpoints(context.Background(), now.Add(2*time.Minute))
	if err != nil || expired != 1 {
		t.Fatalf("ExpireCheckpoints = %d, %v", expired, err)
	}
	_, err = store.ApproveAndLease(context.Background(), ApprovalLeaseRequest{
		CheckpointID:   checkpoint.ID,
		TenantID:       checkpoint.TenantID,
		DefinitionHash: checkpoint.DefinitionHash,
		ApproverID:     "operator-1",
		AuditRef:       "audit-1",
		LeaseOwner:     "worker-1",
		LeaseDuration:  time.Minute,
		Now:            now.Add(2 * time.Minute),
	})
	if !errors.Is(err, ErrCheckpointNotClaimable) {
		t.Fatalf("expired lease error = %v, want ErrCheckpointNotClaimable", err)
	}
}

func TestCheckpointFailureCodePersists(t *testing.T) {
	store := NewMemoryCheckpointStore()
	now := time.Now().UTC()
	checkpoint := ApprovalCheckpoint{
		ID:             "checkpoint-3",
		RunID:          "run-3",
		TenantID:       "tenant-3",
		DefinitionHash: "agent-v3",
		Ciphertext:     []byte("ciphertext"),
		ExpiresAt:      now.Add(time.Hour),
	}
	if err := store.CreateCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if _, err := store.ApproveAndLease(context.Background(), ApprovalLeaseRequest{
		CheckpointID:   checkpoint.ID,
		TenantID:       checkpoint.TenantID,
		DefinitionHash: checkpoint.DefinitionHash,
		ApproverID:     "operator-1",
		AuditRef:       "audit-1",
		LeaseOwner:     "worker-1",
		LeaseDuration:  time.Minute,
		Now:            now,
	}); err != nil {
		t.Fatalf("ApproveAndLease: %v", err)
	}
	if err := store.FailLease(context.Background(), CheckpointLeaseCompletion{
		CheckpointID: checkpoint.ID,
		TenantID:     checkpoint.TenantID,
		LeaseOwner:   "worker-1",
		FailureCode:  "next_checkpoint_persist_failed",
		Now:          now.Add(time.Second),
	}); err != nil {
		t.Fatalf("FailLease: %v", err)
	}
	stored, err := store.GetCheckpoint(context.Background(), checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if stored.Status != CheckpointFailed || stored.FailureCode != "next_checkpoint_persist_failed" {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestFailPendingCheckpointRequiresExactIdentityAndIsIdempotent(t *testing.T) {
	store := NewMemoryCheckpointStore()
	var capability PendingCheckpointFailureStore = store
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	checkpoint := ApprovalCheckpoint{
		ID:             "checkpoint-pending-failure",
		RunID:          "run-pending-failure",
		TenantID:       "tenant-pending-failure",
		DefinitionHash: "agent-v1",
		Ciphertext:     []byte("ciphertext"),
		ExpiresAt:      now.Add(time.Hour),
	}
	if err := store.CreateCheckpoint(ctx, checkpoint); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	request := PendingCheckpointFailure{
		CheckpointID:   checkpoint.ID,
		RunID:          checkpoint.RunID,
		TenantID:       checkpoint.TenantID,
		DefinitionHash: checkpoint.DefinitionHash,
		FailureCode:    "host_shutdown_timeout",
		Now:            now,
	}
	wrong := request
	wrong.RunID = "other-run"
	if err := capability.FailPendingCheckpoint(ctx, wrong); !errors.Is(err, ErrCheckpointNotClaimable) {
		t.Fatalf("wrong identity error = %v, want ErrCheckpointNotClaimable", err)
	}
	pending, err := store.GetCheckpoint(ctx, checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint pending: %v", err)
	}
	if pending.Status != CheckpointPending {
		t.Fatalf("checkpoint after wrong identity = %#v", pending)
	}

	if err := capability.FailPendingCheckpoint(ctx, request); err != nil {
		t.Fatalf("FailPendingCheckpoint: %v", err)
	}
	failed, err := store.GetCheckpoint(ctx, checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint failed: %v", err)
	}
	if failed.Status != CheckpointFailed || failed.FailureCode != request.FailureCode || failed.LeaseOwner != "" || !failed.LeaseUntil.IsZero() || !failed.UpdatedAt.Equal(now) {
		t.Fatalf("failed checkpoint = %#v", failed)
	}

	repeated := request
	repeated.Now = now.Add(time.Minute)
	if err := capability.FailPendingCheckpoint(ctx, repeated); err != nil {
		t.Fatalf("idempotent FailPendingCheckpoint: %v", err)
	}
	idempotent, err := store.GetCheckpoint(ctx, checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint idempotent: %v", err)
	}
	if !idempotent.UpdatedAt.Equal(failed.UpdatedAt) {
		t.Fatalf("idempotent UpdatedAt = %s, want %s", idempotent.UpdatedAt, failed.UpdatedAt)
	}

	differentFailure := request
	differentFailure.FailureCode = "other_failure"
	if err := capability.FailPendingCheckpoint(ctx, differentFailure); !errors.Is(err, ErrCheckpointNotClaimable) {
		t.Fatalf("different failure error = %v, want ErrCheckpointNotClaimable", err)
	}
}

func TestFailPendingCheckpointValidatesRequiredIdentity(t *testing.T) {
	store := NewMemoryCheckpointStore()
	base := PendingCheckpointFailure{
		CheckpointID:   "checkpoint-required",
		RunID:          "run-required",
		TenantID:       "tenant-required",
		DefinitionHash: "agent-v1",
		FailureCode:    "host_shutdown_timeout",
	}
	tests := []struct {
		name   string
		mutate func(*PendingCheckpointFailure)
	}{
		{name: "checkpoint id", mutate: func(request *PendingCheckpointFailure) { request.CheckpointID = "" }},
		{name: "run id", mutate: func(request *PendingCheckpointFailure) { request.RunID = "" }},
		{name: "tenant id", mutate: func(request *PendingCheckpointFailure) { request.TenantID = "" }},
		{name: "definition hash", mutate: func(request *PendingCheckpointFailure) { request.DefinitionHash = "" }},
		{name: "failure code", mutate: func(request *PendingCheckpointFailure) { request.FailureCode = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := base
			tt.mutate(&request)
			if err := store.FailPendingCheckpoint(context.Background(), request); err == nil {
				t.Fatal("FailPendingCheckpoint error = nil, want validation error")
			}
		})
	}
}
