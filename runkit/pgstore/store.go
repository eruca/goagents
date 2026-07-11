// Package pgstore persists opaque runkit approval checkpoints in PostgreSQL.
package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eruca/runkit"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("PostgreSQL DSN is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &Store{db: db}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) CreateCheckpoint(ctx context.Context, checkpoint runkit.ApprovalCheckpoint) error {
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	now := time.Now().UTC()
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = now
	}
	checkpoint.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO approval_checkpoints (
 checkpoint_id, run_id, tenant_id, definition_hash, ciphertext, status, failure_code, expires_at, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, 'pending', '', $6, $7, $8)`,
		checkpoint.ID, checkpoint.RunID, checkpoint.TenantID, checkpoint.DefinitionHash, checkpoint.Ciphertext,
		checkpoint.ExpiresAt.UTC(), checkpoint.CreatedAt.UTC(), checkpoint.UpdatedAt.UTC())
	return err
}

func (s *Store) GetCheckpoint(ctx context.Context, checkpointID, tenantID string) (runkit.ApprovalCheckpoint, error) {
	row := s.db.QueryRowContext(ctx, checkpointSelect+` WHERE c.checkpoint_id = $1 AND c.tenant_id = $2`, checkpointID, tenantID)
	checkpoint, err := scanCheckpoint(row)
	if errors.Is(err, sql.ErrNoRows) {
		return runkit.ApprovalCheckpoint{}, runkit.ErrCheckpointNotFound
	}
	return checkpoint, err
}

func (s *Store) ApproveAndLease(ctx context.Context, request runkit.ApprovalLeaseRequest) (runkit.ApprovalCheckpoint, error) {
	if err := validateLeaseRequest(request); err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	now := requestNow(request.Now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	defer tx.Rollback()
	checkpoint, err := scanCheckpoint(tx.QueryRowContext(ctx, checkpointSelect+`
 WHERE c.checkpoint_id = $1 AND c.tenant_id = $2 AND c.definition_hash = $3 AND c.status = 'pending' AND c.expires_at > $4
 FOR UPDATE OF c`, request.CheckpointID, request.TenantID, request.DefinitionHash, now))
	if errors.Is(err, sql.ErrNoRows) {
		return runkit.ApprovalCheckpoint{}, runkit.ErrCheckpointNotClaimable
	}
	if err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	leaseUntil := now.Add(request.LeaseDuration)
	if _, err := tx.ExecContext(ctx, `UPDATE approval_checkpoints SET status = 'leased', lease_owner = $1, lease_until = $2, updated_at = $3 WHERE checkpoint_id = $4`, request.LeaseOwner, leaseUntil, now, checkpoint.ID); err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO approval_decisions (checkpoint_id, approver_id, approved, audit_ref, reason_code, decided_at) VALUES ($1, $2, true, $3, $4, $5)`, checkpoint.ID, request.ApproverID, request.AuditRef, request.ReasonCode, now); err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	checkpoint.Status = runkit.CheckpointLeased
	checkpoint.LeaseOwner = request.LeaseOwner
	checkpoint.LeaseUntil = leaseUntil
	checkpoint.UpdatedAt = now
	checkpoint.Approval = &runkit.ApprovalAudit{ApproverID: request.ApproverID, Approved: true, AuditRef: request.AuditRef, ReasonCode: request.ReasonCode, DecidedAt: now}
	return checkpoint, nil
}

func (s *Store) RejectCheckpoint(ctx context.Context, request runkit.ApprovalLeaseRequest) error {
	if err := validateResolutionRequest(request); err != nil {
		return err
	}
	now := requestNow(request.Now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
UPDATE approval_checkpoints SET status = 'rejected', updated_at = $1
WHERE checkpoint_id = $2 AND tenant_id = $3 AND definition_hash = $4 AND status = 'pending' AND expires_at > $1`,
		now, request.CheckpointID, request.TenantID, request.DefinitionHash)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return runkit.ErrCheckpointNotClaimable
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO approval_decisions (checkpoint_id, approver_id, approved, audit_ref, reason_code, decided_at) VALUES ($1, $2, false, $3, $4, $5)`, request.CheckpointID, request.ApproverID, request.AuditRef, request.ReasonCode, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CompleteLease(ctx context.Context, completion runkit.CheckpointLeaseCompletion) error {
	return s.finishLease(ctx, completion, runkit.CheckpointConsumed)
}

func (s *Store) FailLease(ctx context.Context, completion runkit.CheckpointLeaseCompletion) error {
	return s.finishLease(ctx, completion, runkit.CheckpointFailed)
}

func (s *Store) finishLease(ctx context.Context, completion runkit.CheckpointLeaseCompletion, status runkit.CheckpointStatus) error {
	if strings.TrimSpace(completion.CheckpointID) == "" || strings.TrimSpace(completion.TenantID) == "" || strings.TrimSpace(completion.LeaseOwner) == "" {
		return fmt.Errorf("checkpoint id, tenant id, and lease owner are required")
	}
	failureCode := ""
	if status == runkit.CheckpointFailed {
		if strings.TrimSpace(completion.FailureCode) == "" {
			return fmt.Errorf("failure code is required when failing a checkpoint lease")
		}
		failureCode = completion.FailureCode
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE approval_checkpoints SET status = $1, failure_code = $2, lease_owner = '', lease_until = NULL, updated_at = $3
WHERE checkpoint_id = $4 AND tenant_id = $5 AND status = 'leased' AND lease_owner = $6`,
		status, failureCode, requestNow(completion.Now), completion.CheckpointID, completion.TenantID, completion.LeaseOwner)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return runkit.ErrCheckpointLeaseOwner
	}
	return nil
}

func (s *Store) ExpireCheckpoints(ctx context.Context, now time.Time) (int, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE approval_checkpoints SET status = 'expired', lease_owner = '', lease_until = NULL, updated_at = $1 WHERE status IN ('pending', 'leased') AND expires_at <= $1`, requestNow(now))
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS approval_checkpoints (
 checkpoint_id TEXT PRIMARY KEY,
 run_id TEXT NOT NULL,
 tenant_id TEXT NOT NULL,
 definition_hash TEXT NOT NULL,
 ciphertext BYTEA NOT NULL,
 status TEXT NOT NULL CHECK (status IN ('pending', 'leased', 'consumed', 'rejected', 'failed', 'expired')),
 failure_code TEXT NOT NULL DEFAULT '',
 lease_owner TEXT NOT NULL DEFAULT '',
 lease_until TIMESTAMPTZ,
 expires_at TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_approval_checkpoints_tenant_status_expiry ON approval_checkpoints (tenant_id, status, expires_at);
CREATE TABLE IF NOT EXISTS approval_decisions (
 checkpoint_id TEXT PRIMARY KEY REFERENCES approval_checkpoints(checkpoint_id) ON DELETE CASCADE,
 approver_id TEXT NOT NULL,
 approved BOOLEAN NOT NULL,
 audit_ref TEXT NOT NULL,
 reason_code TEXT NOT NULL DEFAULT '',
 decided_at TIMESTAMPTZ NOT NULL
);
ALTER TABLE approval_checkpoints ADD COLUMN IF NOT EXISTS failure_code TEXT NOT NULL DEFAULT '';`)
	return err
}

const checkpointSelect = `SELECT c.checkpoint_id, c.run_id, c.tenant_id, c.definition_hash, c.ciphertext, c.status, c.failure_code, c.lease_owner, c.lease_until, c.expires_at, c.created_at, c.updated_at, d.approver_id, d.approved, d.audit_ref, d.reason_code, d.decided_at FROM approval_checkpoints c LEFT JOIN approval_decisions d ON d.checkpoint_id = c.checkpoint_id`

type rowScanner interface{ Scan(...any) error }

func scanCheckpoint(row rowScanner) (runkit.ApprovalCheckpoint, error) {
	var checkpoint runkit.ApprovalCheckpoint
	var status string
	var leaseUntil sql.NullTime
	var approverID, auditRef, reasonCode sql.NullString
	var approved sql.NullBool
	var decidedAt sql.NullTime
	err := row.Scan(&checkpoint.ID, &checkpoint.RunID, &checkpoint.TenantID, &checkpoint.DefinitionHash, &checkpoint.Ciphertext, &status, &checkpoint.FailureCode, &checkpoint.LeaseOwner, &leaseUntil, &checkpoint.ExpiresAt, &checkpoint.CreatedAt, &checkpoint.UpdatedAt, &approverID, &approved, &auditRef, &reasonCode, &decidedAt)
	if err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	checkpoint.Status = runkit.CheckpointStatus(status)
	if leaseUntil.Valid {
		checkpoint.LeaseUntil = leaseUntil.Time.UTC()
	}
	if approverID.Valid {
		checkpoint.Approval = &runkit.ApprovalAudit{ApproverID: approverID.String, Approved: approved.Bool, AuditRef: auditRef.String, ReasonCode: reasonCode.String, DecidedAt: decidedAt.Time.UTC()}
	}
	return checkpoint, nil
}

func validateCheckpoint(checkpoint runkit.ApprovalCheckpoint) error {
	if strings.TrimSpace(checkpoint.ID) == "" || strings.TrimSpace(checkpoint.RunID) == "" || strings.TrimSpace(checkpoint.TenantID) == "" || strings.TrimSpace(checkpoint.DefinitionHash) == "" || len(checkpoint.Ciphertext) == 0 || checkpoint.ExpiresAt.IsZero() {
		return fmt.Errorf("checkpoint id, run id, tenant id, definition hash, ciphertext, and expiry are required")
	}
	return nil
}

func validateResolutionRequest(request runkit.ApprovalLeaseRequest) error {
	if strings.TrimSpace(request.CheckpointID) == "" || strings.TrimSpace(request.TenantID) == "" || strings.TrimSpace(request.DefinitionHash) == "" || strings.TrimSpace(request.ApproverID) == "" || strings.TrimSpace(request.AuditRef) == "" {
		return fmt.Errorf("checkpoint id, tenant id, definition hash, approver id, and audit ref are required")
	}
	return nil
}

func validateLeaseRequest(request runkit.ApprovalLeaseRequest) error {
	if err := validateResolutionRequest(request); err != nil {
		return err
	}
	if strings.TrimSpace(request.LeaseOwner) == "" || request.LeaseDuration <= 0 {
		return fmt.Errorf("lease owner and positive lease duration are required")
	}
	return nil
}

func requestNow(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}
