package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eruca/goagents/runkit"
)

var (
	_ runkit.CheckpointStore               = (*Store)(nil)
	_ runkit.PendingCheckpointFailureStore = (*Store)(nil)
)

func (s *Store) CreateCheckpoint(ctx context.Context, checkpoint runkit.ApprovalCheckpoint) error {
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	now := time.Now().UTC()
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = now
	}
	checkpoint.UpdatedAt = now
	result, err := s.db.ExecContext(ctx, `
INSERT INTO approval_checkpoints (
 checkpoint_id, run_id, tenant_id, definition_hash, ciphertext, status, failure_code, expires_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'pending', '', ?, ?, ?)
ON CONFLICT(checkpoint_id) DO NOTHING`,
		checkpoint.ID, checkpoint.RunID, checkpoint.TenantID, checkpoint.DefinitionHash, checkpoint.Ciphertext,
		checkpoint.ExpiresAt.UTC(), checkpoint.CreatedAt.UTC(), checkpoint.UpdatedAt.UTC())
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
	return nil
}

func (s *Store) GetCheckpoint(ctx context.Context, checkpointID, tenantID string) (runkit.ApprovalCheckpoint, error) {
	checkpoint, err := scanCheckpoint(s.db.QueryRowContext(ctx, checkpointSelect+` WHERE c.checkpoint_id = ? AND c.tenant_id = ?`, checkpointID, tenantID))
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
	leaseUntil := now.Add(request.LeaseDuration)
	result, err := tx.ExecContext(ctx, `
UPDATE approval_checkpoints
SET status = 'leased', lease_owner = ?, lease_until = ?, updated_at = ?
WHERE checkpoint_id = ? AND tenant_id = ? AND definition_hash = ? AND status = 'pending' AND expires_at > ?`,
		request.LeaseOwner, leaseUntil, now, request.CheckpointID, request.TenantID, request.DefinitionHash, now)
	if err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	if affected != 1 {
		return runkit.ApprovalCheckpoint{}, runkit.ErrCheckpointNotClaimable
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO approval_decisions (checkpoint_id, approver_id, approved, audit_ref, reason_code, decided_at)
VALUES (?, ?, 1, ?, ?, ?)`, request.CheckpointID, request.ApproverID, request.AuditRef, request.ReasonCode, now); err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	checkpoint, err := scanCheckpoint(tx.QueryRowContext(ctx, checkpointSelect+` WHERE c.checkpoint_id = ? AND c.tenant_id = ?`, request.CheckpointID, request.TenantID))
	if err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
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
UPDATE approval_checkpoints
SET status = 'rejected', updated_at = ?
WHERE checkpoint_id = ? AND tenant_id = ? AND definition_hash = ? AND status = 'pending' AND expires_at > ?`,
		now, request.CheckpointID, request.TenantID, request.DefinitionHash, now)
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
	if _, err := tx.ExecContext(ctx, `
INSERT INTO approval_decisions (checkpoint_id, approver_id, approved, audit_ref, reason_code, decided_at)
VALUES (?, ?, 0, ?, ?, ?)`, request.CheckpointID, request.ApproverID, request.AuditRef, request.ReasonCode, now); err != nil {
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

func (s *Store) FailPendingCheckpoint(ctx context.Context, request runkit.PendingCheckpointFailure) error {
	if err := validatePendingCheckpointFailure(request); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE approval_checkpoints
SET status = 'failed', failure_code = ?, lease_owner = '', lease_until = NULL,
 updated_at = CASE WHEN status = 'pending' THEN ? ELSE updated_at END
WHERE checkpoint_id = ? AND run_id = ? AND tenant_id = ? AND definition_hash = ?
 AND (status = 'pending' OR (status = 'failed' AND failure_code = ?))`,
		request.FailureCode, requestNow(request.Now), request.CheckpointID, request.RunID,
		request.TenantID, request.DefinitionHash, request.FailureCode)
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
	return nil
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
UPDATE approval_checkpoints
SET status = ?, failure_code = ?, lease_owner = '', lease_until = NULL, updated_at = ?
WHERE checkpoint_id = ? AND tenant_id = ? AND status = 'leased' AND lease_owner = ?`,
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
	result, err := s.db.ExecContext(ctx, `
UPDATE approval_checkpoints
SET status = 'expired', lease_owner = '', lease_until = NULL, updated_at = ?
WHERE status IN ('pending', 'leased') AND expires_at <= ?`, requestNow(now), requestNow(now))
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}

const checkpointSelect = `
SELECT c.checkpoint_id, c.run_id, c.tenant_id, c.definition_hash, c.ciphertext,
 c.status, c.failure_code, c.lease_owner, c.lease_until, c.expires_at, c.created_at,
 c.updated_at, d.approver_id, d.approved, d.audit_ref, d.reason_code, d.decided_at
FROM approval_checkpoints c
LEFT JOIN approval_decisions d ON d.checkpoint_id = c.checkpoint_id`

type checkpointRowScanner interface{ Scan(...any) error }

func scanCheckpoint(row checkpointRowScanner) (runkit.ApprovalCheckpoint, error) {
	var checkpoint runkit.ApprovalCheckpoint
	var status string
	var leaseUntil sql.NullTime
	var approverID, auditRef, reasonCode sql.NullString
	var approved sql.NullBool
	var decidedAt sql.NullTime
	err := row.Scan(
		&checkpoint.ID, &checkpoint.RunID, &checkpoint.TenantID, &checkpoint.DefinitionHash, &checkpoint.Ciphertext,
		&status, &checkpoint.FailureCode, &checkpoint.LeaseOwner, &leaseUntil, &checkpoint.ExpiresAt, &checkpoint.CreatedAt,
		&checkpoint.UpdatedAt, &approverID, &approved, &auditRef, &reasonCode, &decidedAt,
	)
	if err != nil {
		return runkit.ApprovalCheckpoint{}, err
	}
	checkpoint.Status = runkit.CheckpointStatus(status)
	if leaseUntil.Valid {
		checkpoint.LeaseUntil = leaseUntil.Time.UTC()
	}
	if approverID.Valid {
		checkpoint.Approval = &runkit.ApprovalAudit{
			ApproverID: approverID.String,
			Approved:   approved.Bool,
			AuditRef:   auditRef.String,
			ReasonCode: reasonCode.String,
			DecidedAt:  decidedAt.Time.UTC(),
		}
	}
	return checkpoint, nil
}

func validateCheckpoint(checkpoint runkit.ApprovalCheckpoint) error {
	if strings.TrimSpace(checkpoint.ID) == "" || strings.TrimSpace(checkpoint.RunID) == "" || strings.TrimSpace(checkpoint.TenantID) == "" || strings.TrimSpace(checkpoint.DefinitionHash) == "" || len(checkpoint.Ciphertext) == 0 || checkpoint.ExpiresAt.IsZero() {
		return fmt.Errorf("checkpoint id, run id, tenant id, definition hash, ciphertext, and expiry are required")
	}
	return nil
}

func validatePendingCheckpointFailure(request runkit.PendingCheckpointFailure) error {
	if strings.TrimSpace(request.CheckpointID) == "" ||
		strings.TrimSpace(request.RunID) == "" ||
		strings.TrimSpace(request.TenantID) == "" ||
		strings.TrimSpace(request.DefinitionHash) == "" ||
		strings.TrimSpace(request.FailureCode) == "" {
		return fmt.Errorf("checkpoint id, run id, tenant id, definition hash, and failure code are required")
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
