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
