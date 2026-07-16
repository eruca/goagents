package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/runkit"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestStoreApprovalCheckpointLifecycle(t *testing.T) {
	dsn := os.Getenv("RUNKIT_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set RUNKIT_POSTGRES_TEST_DSN to run PostgreSQL integration tests")
	}
	store, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	checkpoint := runkit.ApprovalCheckpoint{
		ID:             "checkpoint-integration",
		RunID:          "run-integration",
		TenantID:       "tenant-integration",
		DefinitionHash: "agent-v1",
		Ciphertext:     []byte("ciphertext"),
		ExpiresAt:      now.Add(time.Hour),
	}
	if err := store.CreateCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	leased, err := store.ApproveAndLease(context.Background(), runkit.ApprovalLeaseRequest{
		CheckpointID:   checkpoint.ID,
		TenantID:       checkpoint.TenantID,
		DefinitionHash: checkpoint.DefinitionHash,
		ApproverID:     "operator-1",
		AuditRef:       "audit-1",
		LeaseOwner:     "worker-1",
		LeaseDuration:  time.Minute,
		Now:            now,
	})
	if err != nil || leased.Status != runkit.CheckpointLeased || leased.Approval == nil {
		t.Fatalf("ApproveAndLease = %#v, %v", leased, err)
	}
	if err := store.CompleteLease(context.Background(), runkit.CheckpointLeaseCompletion{
		CheckpointID: checkpoint.ID,
		TenantID:     checkpoint.TenantID,
		LeaseOwner:   "worker-1",
		Now:          now.Add(time.Second),
	}); err != nil {
		t.Fatalf("CompleteLease: %v", err)
	}
}

func TestStoreAllowsOnlyOneConcurrentLease(t *testing.T) {
	dsn := os.Getenv("RUNKIT_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set RUNKIT_POSTGRES_TEST_DSN to run PostgreSQL integration tests")
	}
	store, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	checkpoint := runkit.ApprovalCheckpoint{
		ID:             "checkpoint-concurrent-" + now.Format("20060102150405.000000000"),
		RunID:          "run-concurrent",
		TenantID:       "tenant-concurrent",
		DefinitionHash: "agent-v1",
		Ciphertext:     []byte("ciphertext"),
		ExpiresAt:      now.Add(time.Hour),
	}
	if err := store.CreateCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	errorsByWorker := make(chan error, 2)
	var workers sync.WaitGroup
	for _, worker := range []string{"worker-1", "worker-2"} {
		workers.Add(1)
		go func(worker string) {
			defer workers.Done()
			_, err := store.ApproveAndLease(context.Background(), runkit.ApprovalLeaseRequest{
				CheckpointID:   checkpoint.ID,
				TenantID:       checkpoint.TenantID,
				DefinitionHash: checkpoint.DefinitionHash,
				ApproverID:     "operator-" + worker,
				AuditRef:       "audit-" + worker,
				LeaseOwner:     worker,
				LeaseDuration:  time.Minute,
				Now:            now,
			})
			errorsByWorker <- err
		}(worker)
	}
	workers.Wait()
	close(errorsByWorker)

	succeeded := 0
	for err := range errorsByWorker {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, runkit.ErrCheckpointNotClaimable) {
			t.Fatalf("lease error = %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful leases = %d, want 1", succeeded)
	}
}

func TestStoreFailureCodePersists(t *testing.T) {
	dsn := os.Getenv("RUNKIT_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set RUNKIT_POSTGRES_TEST_DSN to run PostgreSQL integration tests")
	}
	store, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	checkpoint := runkit.ApprovalCheckpoint{
		ID:             "checkpoint-failure-" + now.Format("20060102150405.000000000"),
		RunID:          "run-failure",
		TenantID:       "tenant-failure",
		DefinitionHash: "agent-v1",
		Ciphertext:     []byte("ciphertext"),
		ExpiresAt:      now.Add(time.Hour),
	}
	if err := store.CreateCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if _, err := store.ApproveAndLease(context.Background(), runkit.ApprovalLeaseRequest{
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
	if err := store.FailLease(context.Background(), runkit.CheckpointLeaseCompletion{
		CheckpointID: checkpoint.ID,
		TenantID:     checkpoint.TenantID,
		LeaseOwner:   "worker-1",
		FailureCode:  "checkpoint_decode_failed",
		Now:          now.Add(time.Second),
	}); err != nil {
		t.Fatalf("FailLease: %v", err)
	}
	stored, err := store.GetCheckpoint(context.Background(), checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if stored.Status != runkit.CheckpointFailed || stored.FailureCode != "checkpoint_decode_failed" {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestStoreMigratesExistingCheckpointSchema(t *testing.T) {
	dsn := os.Getenv("RUNKIT_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set RUNKIT_POSTGRES_TEST_DSN to run PostgreSQL integration tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `
DROP TABLE IF EXISTS approval_decisions;
DROP TABLE IF EXISTS approval_checkpoints;
CREATE TABLE approval_checkpoints (
 checkpoint_id TEXT PRIMARY KEY,
 run_id TEXT NOT NULL,
 tenant_id TEXT NOT NULL,
 definition_hash TEXT NOT NULL,
 ciphertext BYTEA NOT NULL,
 status TEXT NOT NULL,
 lease_owner TEXT NOT NULL DEFAULT '',
 lease_until TIMESTAMPTZ,
 expires_at TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL
);`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	store, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open migration: %v", err)
	}
	defer store.Close()
	var exists bool
	if err := store.db.QueryRowContext(context.Background(), `
SELECT EXISTS (
 SELECT 1 FROM information_schema.columns
 WHERE table_name = 'approval_checkpoints' AND column_name = 'failure_code'
)`).Scan(&exists); err != nil {
		t.Fatalf("query migrated column: %v", err)
	}
	if !exists {
		t.Fatal("failure_code column was not added to legacy checkpoint table")
	}
}
