package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eruca/goagents/memorykit"
)

var _ memorykit.ExtractionJobStore = (*Store)(nil)

func (s *Store) EnqueueExtraction(ctx context.Context, job memorykit.ExtractionJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validateEnqueueExtraction(job); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_extraction_jobs (
			id, tenant_id, subject_type, subject_id,
			source_kind, source_ref, evidence_hash, source_agent_id, extractor_id,
			status, attempts, lease_owner, lease_until, failure_code, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0, '', NULL, '', $11, $12)
		ON CONFLICT (id) DO NOTHING`,
		job.ID, job.Scope.TenantID, job.Scope.SubjectType, job.Scope.SubjectID,
		job.Source.Kind, job.Source.Ref, job.Source.EvidenceHash, job.SourceAgentID, job.ExtractorID,
		memorykit.ExtractionPending, job.CreatedAt, job.UpdatedAt)
	if err != nil {
		return wrapBackend("enqueue extraction", err)
	}
	return nil
}

func (s *Store) ClaimExtraction(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
	maxAttempts int,
	now time.Time,
) (memorykit.ExtractionJob, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.ExtractionJob{}, err
	}
	if !s.validExtractionIdentity(workerID) || leaseDuration < time.Microsecond ||
		maxAttempts <= 0 || maxAttempts > math.MaxInt32 || now.IsZero() {
		return memorykit.ExtractionJob{}, fmt.Errorf("%w: invalid extraction claim", memorykit.ErrInvalidMemory)
	}
	leaseUntil := now.Add(leaseDuration)
	if !leaseUntil.After(now) || leaseUntil.Unix() < now.Unix() {
		return memorykit.ExtractionJob{}, fmt.Errorf("%w: invalid extraction lease duration", memorykit.ErrInvalidMemory)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memorykit.ExtractionJob{}, wrapBackend("claim extraction transaction", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		UPDATE memory_extraction_jobs
		SET status = 'failed', failure_code = 'lease_exhausted',
			source_kind = '', source_ref = '', evidence_hash = '', source_agent_id = '',
			lease_owner = '', lease_until = NULL, updated_at = $1
		WHERE status = 'leased' AND lease_until <= $1 AND attempts >= $2`, now, maxAttempts); err != nil {
		return memorykit.ExtractionJob{}, wrapBackend("terminalize extraction lease", err)
	}

	job, err := scanExtractionJob(tx.QueryRowContext(ctx, `
		WITH claimable AS (
			SELECT id FROM memory_extraction_jobs
			WHERE attempts < $2
			  AND (status = 'pending' OR (status = 'leased' AND lease_until <= $1))
			ORDER BY created_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE memory_extraction_jobs AS jobs
		SET status = 'leased', attempts = jobs.attempts + 1,
			lease_owner = $3, lease_until = $4, updated_at = $1
		FROM claimable
		WHERE jobs.id = claimable.id
		RETURNING `+qualifiedExtractionJobColumns("jobs"), now, maxAttempts, workerID, leaseUntil))
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return memorykit.ExtractionJob{}, wrapBackend("claim extraction commit", err)
		}
		return memorykit.ExtractionJob{}, memorykit.ErrNoExtractionJob
	}
	if err != nil {
		return memorykit.ExtractionJob{}, wrapBackend("claim extraction", err)
	}
	if err := tx.Commit(); err != nil {
		return memorykit.ExtractionJob{}, wrapBackend("claim extraction commit", err)
	}
	return job, nil
}

func (s *Store) CompleteExtraction(ctx context.Context, id, workerID string, expectedAttempt int, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := memorykit.ValidateMemoryID(id); err != nil {
		return err
	}
	if !s.validExtractionIdentity(workerID) || expectedAttempt <= 0 || expectedAttempt > math.MaxInt32 || now.IsZero() {
		return fmt.Errorf("%w: invalid extraction completion", memorykit.ErrInvalidMemory)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE memory_extraction_jobs
		SET status = 'completed',
			source_kind = '', source_ref = '', evidence_hash = '', source_agent_id = '',
			lease_owner = '', lease_until = NULL, updated_at = $4
		WHERE id = $1 AND lease_owner = $2 AND attempts = $3
		  AND status = 'leased' AND lease_until > $4`, id, workerID, expectedAttempt, now)
	if err != nil {
		return wrapBackend("complete extraction", err)
	}
	return requireExtractionTransition(result)
}

func (s *Store) FailExtraction(
	ctx context.Context,
	id, workerID string,
	expectedAttempt int,
	failureCode string,
	maxAttempts int,
	now time.Time,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := memorykit.ValidateMemoryID(id); err != nil {
		return err
	}
	if !s.validExtractionIdentity(workerID) || expectedAttempt <= 0 || expectedAttempt > math.MaxInt32 ||
		!validExtractionFailureCode(failureCode) || maxAttempts <= 0 || maxAttempts > math.MaxInt32 || now.IsZero() {
		return fmt.Errorf("%w: invalid extraction failure", memorykit.ErrInvalidMemory)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE memory_extraction_jobs
		SET status = CASE WHEN attempts >= $5 THEN 'failed' ELSE 'pending' END,
			failure_code = $4,
			source_kind = CASE WHEN attempts >= $5 THEN '' ELSE source_kind END,
			source_ref = CASE WHEN attempts >= $5 THEN '' ELSE source_ref END,
			evidence_hash = CASE WHEN attempts >= $5 THEN '' ELSE evidence_hash END,
			source_agent_id = CASE WHEN attempts >= $5 THEN '' ELSE source_agent_id END,
			lease_owner = '', lease_until = NULL, updated_at = $6
		WHERE id = $1 AND lease_owner = $2 AND attempts = $3
		  AND status = 'leased' AND lease_until > $6`,
		id, workerID, expectedAttempt, failureCode, maxAttempts, now)
	if err != nil {
		return wrapBackend("fail extraction", err)
	}
	return requireExtractionTransition(result)
}

func (s *Store) validateEnqueueExtraction(job memorykit.ExtractionJob) error {
	if err := memorykit.ValidateMemoryID(job.ID); err != nil {
		return err
	}
	if err := job.Scope.Validate(); err != nil {
		return err
	}
	if err := memorykit.ValidateSources([]memorykit.Source{job.Source}, s.cfg.Limits); err != nil {
		return err
	}
	if job.Status != memorykit.ExtractionPending || job.Attempts != 0 || job.LeaseOwner != "" ||
		!job.LeaseUntil.IsZero() || job.CreatedAt.IsZero() || job.UpdatedAt.IsZero() ||
		job.UpdatedAt.Before(job.CreatedAt) || strings.TrimSpace(job.Source.Kind) == "" ||
		strings.TrimSpace(job.Source.Ref) == "" || !s.validOptionalExtractionIdentity(job.SourceAgentID) ||
		!s.validExtractionIdentity(job.ExtractorID) || !s.validExtractionIdentity("extractor:"+job.ExtractorID) {
		return fmt.Errorf("%w: invalid extraction job", memorykit.ErrInvalidMemory)
	}
	return nil
}

func (s *Store) validExtractionIdentity(value string) bool {
	return value != "" && value == strings.TrimSpace(value) &&
		utf8.RuneCountInString(value) <= s.cfg.Limits.MaxMetadataRunes
}

func (s *Store) validOptionalExtractionIdentity(value string) bool {
	return value == "" || s.validExtractionIdentity(value)
}

func validExtractionFailureCode(code string) bool {
	switch code {
	case "source_read_failed", "candidate_extract_failed", "candidate_validate_failed", "candidate_write_failed":
		return true
	default:
		return false
	}
}

func requireExtractionTransition(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return wrapBackend("inspect extraction transition", err)
	}
	if affected != 1 {
		return memorykit.ErrConflict
	}
	return nil
}

func qualifiedExtractionJobColumns(alias string) string {
	return alias + `.id, ` + alias + `.tenant_id, ` + alias + `.subject_type, ` + alias + `.subject_id,
		` + alias + `.source_kind, ` + alias + `.source_ref, ` + alias + `.evidence_hash,
		` + alias + `.source_agent_id, ` + alias + `.extractor_id, ` + alias + `.status,
		` + alias + `.attempts, ` + alias + `.lease_owner, ` + alias + `.lease_until,
		` + alias + `.created_at, ` + alias + `.updated_at`
}

func scanExtractionJob(row rowScanner) (memorykit.ExtractionJob, error) {
	var job memorykit.ExtractionJob
	var status string
	var leaseUntil sql.NullTime
	err := row.Scan(
		&job.ID, &job.Scope.TenantID, &job.Scope.SubjectType, &job.Scope.SubjectID,
		&job.Source.Kind, &job.Source.Ref, &job.Source.EvidenceHash,
		&job.SourceAgentID, &job.ExtractorID, &status, &job.Attempts,
		&job.LeaseOwner, &leaseUntil, &job.CreatedAt, &job.UpdatedAt,
	)
	if err != nil {
		return memorykit.ExtractionJob{}, err
	}
	job.Status = memorykit.ExtractionJobStatus(status)
	if leaseUntil.Valid {
		job.LeaseUntil = leaseUntil.Time
	}
	return job, nil
}
