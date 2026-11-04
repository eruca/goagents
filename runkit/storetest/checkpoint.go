package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eruca/goagents/runkit"
)

type NewCheckpointStore func(*testing.T) runkit.CheckpointStore

// RunCheckpointStoreConformance checks the transitions and errors every
// runkit.CheckpointStore must share. Each subtest uses its own checkpoint id,
// so stores backed by a shared database can run it.
func RunCheckpointStoreConformance(t *testing.T, prefix string, newStore NewCheckpointStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	create := func(t *testing.T, store runkit.CheckpointStore, id string, expiresAt time.Time) {
		t.Helper()
		if err := store.CreateCheckpoint(ctx, runkit.ApprovalCheckpoint{
			ID: prefix + id, RunID: "run-" + id, TenantID: "tenant-a", DefinitionHash: "def-1",
			Ciphertext: []byte("sealed"), ExpiresAt: expiresAt,
		}); err != nil {
			t.Fatalf("CreateCheckpoint returned error: %v", err)
		}
	}
	lease := func(id string, at time.Time, duration time.Duration) runkit.ApprovalLeaseRequest {
		return runkit.ApprovalLeaseRequest{
			CheckpointID: prefix + id, TenantID: "tenant-a", DefinitionHash: "def-1",
			ApproverID: "operator", AuditRef: "audit:" + id, LeaseOwner: "worker-1", LeaseDuration: duration, Now: at,
		}
	}
	completion := func(id, tenant, owner string) runkit.CheckpointLeaseCompletion {
		return runkit.CheckpointLeaseCompletion{CheckpointID: prefix + id, TenantID: tenant, LeaseOwner: owner, Now: now}
	}

	t.Run("duplicate create is not claimable", func(t *testing.T) {
		store := newStore(t)
		create(t, store, "dup", now.Add(time.Hour))
		err := store.CreateCheckpoint(ctx, runkit.ApprovalCheckpoint{
			ID: prefix + "dup", RunID: "run-dup", TenantID: "tenant-a", DefinitionHash: "def-1",
			Ciphertext: []byte("sealed"), ExpiresAt: now.Add(time.Hour),
		})
		if !errors.Is(err, runkit.ErrCheckpointNotClaimable) {
			t.Fatalf("duplicate CreateCheckpoint error = %v, want ErrCheckpointNotClaimable", err)
		}
	})

	t.Run("another tenant cannot see a checkpoint", func(t *testing.T) {
		store := newStore(t)
		create(t, store, "tenant", now.Add(time.Hour))
		if _, err := store.GetCheckpoint(ctx, prefix+"tenant", "tenant-b"); !errors.Is(err, runkit.ErrCheckpointNotFound) {
			t.Fatalf("GetCheckpoint as another tenant error = %v, want ErrCheckpointNotFound", err)
		}
	})

	t.Run("finishing a lease reports missing and foreign leases apart", func(t *testing.T) {
		store := newStore(t)
		create(t, store, "finish", now.Add(time.Hour))
		if _, err := store.ApproveAndLease(ctx, lease("finish", now, time.Minute)); err != nil {
			t.Fatalf("ApproveAndLease returned error: %v", err)
		}
		if err := store.CompleteLease(ctx, completion("missing", "tenant-a", "worker-1")); !errors.Is(err, runkit.ErrCheckpointNotFound) {
			t.Fatalf("CompleteLease of a missing checkpoint error = %v, want ErrCheckpointNotFound", err)
		}
		if err := store.CompleteLease(ctx, completion("finish", "tenant-b", "worker-1")); !errors.Is(err, runkit.ErrCheckpointNotFound) {
			t.Fatalf("CompleteLease as another tenant error = %v, want ErrCheckpointNotFound", err)
		}
		if err := store.CompleteLease(ctx, completion("finish", "tenant-a", "worker-2")); !errors.Is(err, runkit.ErrCheckpointLeaseOwner) {
			t.Fatalf("CompleteLease by another owner error = %v, want ErrCheckpointLeaseOwner", err)
		}
		if err := store.CompleteLease(ctx, completion("finish", "tenant-a", "worker-1")); err != nil {
			t.Fatalf("CompleteLease returned error: %v", err)
		}
		consumed, err := store.GetCheckpoint(ctx, prefix+"finish", "tenant-a")
		if err != nil || consumed.Status != runkit.CheckpointConsumed || len(consumed.Ciphertext) != 0 {
			t.Fatalf("consumed checkpoint = %+v, %v; want consumed without ciphertext", consumed, err)
		}
	})

	t.Run("expiry waits for a live lease", func(t *testing.T) {
		store := newStore(t)
		expiresAt := now.Add(time.Minute)
		create(t, store, "live", expiresAt)
		create(t, store, "idle", expiresAt)
		// Approved a second before expiry with a ten-minute lease.
		if _, err := store.ApproveAndLease(ctx, lease("live", expiresAt.Add(-time.Second), 10*time.Minute)); err != nil {
			t.Fatalf("ApproveAndLease returned error: %v", err)
		}
		if _, err := store.ExpireCheckpoints(ctx, expiresAt.Add(time.Second)); err != nil {
			t.Fatalf("ExpireCheckpoints returned error: %v", err)
		}
		idle, err := store.GetCheckpoint(ctx, prefix+"idle", "tenant-a")
		if err != nil || idle.Status != runkit.CheckpointExpired || len(idle.Ciphertext) != 0 {
			t.Fatalf("pending checkpoint after expiry = %+v, %v; want expired", idle, err)
		}
		live, err := store.GetCheckpoint(ctx, prefix+"live", "tenant-a")
		if err != nil || live.Status != runkit.CheckpointLeased {
			t.Fatalf("leased checkpoint after expiry = %+v, %v; want still leased", live, err)
		}
		if _, err := store.ExpireCheckpoints(ctx, expiresAt.Add(11*time.Minute)); err != nil {
			t.Fatalf("ExpireCheckpoints returned error: %v", err)
		}
		abandoned, err := store.GetCheckpoint(ctx, prefix+"live", "tenant-a")
		if err != nil || abandoned.Status != runkit.CheckpointExpired {
			t.Fatalf("checkpoint after its lease ended = %+v, %v; want expired", abandoned, err)
		}
	})
}
