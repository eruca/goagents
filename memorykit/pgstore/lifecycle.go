package pgstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ memorykit.LifecycleStore = (*Store)(nil)

func (s *Store) Create(ctx context.Context, request memorykit.CreateRequest) (memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	if err := memorykit.ValidateCreateRequest(request, s.cfg.Limits); err != nil {
		return memorykit.Memory{}, err
	}

	var fingerprint string
	if request.IdempotencyKey != "" {
		var err error
		fingerprint, err = createFingerprint(request)
		if err != nil {
			return memorykit.Memory{}, err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memorykit.Memory{}, wrapBackend("create transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	if request.IdempotencyKey != "" {
		if err := lockIdempotencyIdentity(ctx, tx, request.Scope, request.IdempotencyKey); err != nil {
			return memorykit.Memory{}, err
		}
		replayed, found, err := s.idempotentReplay(ctx, tx, request, fingerprint)
		if err != nil {
			return memorykit.Memory{}, err
		}
		if found {
			if err := tx.Commit(); err != nil {
				return memorykit.Memory{}, lifecycleWriteError("create commit", err)
			}
			return replayed, nil
		}
	}

	memory := memorykit.Memory{
		ID: request.ID, Scope: request.Scope, Kind: request.Kind, Key: request.Key,
		Status: request.Status, Content: request.Content,
		ValidFrom: canonicalTime(request.ValidFrom), ValidUntil: canonicalTime(request.ValidUntil),
		Importance: request.Importance, Confidence: request.Confidence,
		SourceAgentID: request.SourceAgentID, CreatedBy: request.Actor,
		IdempotencyKey: request.IdempotencyKey, Version: 1,
		CreatedAt: canonicalTime(request.Now), UpdatedAt: canonicalTime(request.Now),
	}
	if memory.Status == memorykit.StatusActive {
		if err := s.supersedeCurrentActive(ctx, tx, memory, memory.ID, request.Actor, request.Reason, request.Now, false); err != nil {
			return memorykit.Memory{}, err
		}
	}
	if err := insertMemory(ctx, tx, memory); err != nil {
		return memorykit.Memory{}, lifecycleWriteError("create memory", err)
	}
	if err := replaceSources(ctx, tx, memory.ID, request.Sources, request.Now); err != nil {
		return memorykit.Memory{}, lifecycleWriteError("create sources", err)
	}
	if err := appendRevision(ctx, tx, memory, memorykit.RevisionCreate, request.Actor, request.Reason, request.Now, false, fingerprint); err != nil {
		return memorykit.Memory{}, err
	}
	if err := tx.Commit(); err != nil {
		return memorykit.Memory{}, lifecycleWriteError("create commit", err)
	}
	return memory, nil
}

func (s *Store) Get(ctx context.Context, scope memorykit.Scope, id string) (memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	if err := validateIdentity(scope, id); err != nil {
		return memorykit.Memory{}, err
	}
	memory, err := scanMemory(s.db.QueryRowContext(ctx, `SELECT `+memoryColumns+`
		FROM memories
		WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3 AND id = $4`,
		scope.TenantID, scope.SubjectType, scope.SubjectID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return memorykit.Memory{}, memorykit.ErrNotFound
	}
	if err != nil {
		return memorykit.Memory{}, wrapBackend("get memory", err)
	}
	return memory, nil
}

func (s *Store) Sources(ctx context.Context, scope memorykit.Scope, id string) ([]memorykit.Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateIdentity(scope, id); err != nil {
		return nil, err
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT TRUE FROM memories
		WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3 AND id = $4`,
		scope.TenantID, scope.SubjectType, scope.SubjectID, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, memorykit.ErrNotFound
	} else if err != nil {
		return nil, wrapBackend("source identity", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT s.source_kind, s.source_ref, s.evidence_hash
		FROM memory_sources s
		JOIN memories m ON m.id = s.memory_id
		WHERE m.tenant_id = $1 AND m.subject_type = $2 AND m.subject_id = $3 AND m.id = $4
		ORDER BY s.source_kind ASC, s.source_ref ASC`,
		scope.TenantID, scope.SubjectType, scope.SubjectID, id)
	if err != nil {
		return nil, wrapBackend("list sources", err)
	}
	defer rows.Close()
	result := make([]memorykit.Source, 0)
	for rows.Next() {
		var source memorykit.Source
		if err := rows.Scan(&source.Kind, &source.Ref, &source.EvidenceHash); err != nil {
			return nil, wrapBackend("scan source", err)
		}
		result = append(result, source)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapBackend("list sources", err)
	}
	return result, nil
}

func (s *Store) List(ctx context.Context, query memorykit.ListQuery) ([]memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := memorykit.ValidateListQuery(query, s.cfg.Limits); err != nil {
		return nil, err
	}
	statement := `SELECT ` + memoryColumns + ` FROM memories
		WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3`
	arguments := []any{query.Scope.TenantID, query.Scope.SubjectType, query.Scope.SubjectID}
	if query.Kind != "" {
		arguments = append(arguments, query.Kind)
		statement += fmt.Sprintf(" AND kind = $%d", len(arguments))
	}
	if query.Status != "" {
		arguments = append(arguments, query.Status)
		statement += fmt.Sprintf(" AND status = $%d", len(arguments))
	}
	if query.Key != "" {
		arguments = append(arguments, query.Key)
		statement += fmt.Sprintf(" AND memory_key = $%d", len(arguments))
	}
	arguments = append(arguments, query.Limit)
	statement += fmt.Sprintf(" ORDER BY updated_at DESC, id ASC LIMIT $%d", len(arguments))

	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, wrapBackend("list memories", err)
	}
	defer rows.Close()
	result := make([]memorykit.Memory, 0)
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, wrapBackend("scan memory", err)
		}
		result = append(result, memory)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapBackend("list memories", err)
	}
	return result, nil
}

func (s *Store) Activate(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	if err := memorykit.ValidateCommand(command, s.cfg.Limits); err != nil {
		return memorykit.Memory{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memorykit.Memory{}, wrapBackend("activate transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	target, err := lockCommandTarget(ctx, tx, command)
	if err != nil {
		return memorykit.Memory{}, err
	}
	if target.Status != memorykit.StatusCandidate {
		return memorykit.Memory{}, memorykit.ErrConflict
	}
	if err := s.supersedeCurrentActive(ctx, tx, target, target.ID, command.Actor, command.Reason, command.Now, true); err != nil {
		return memorykit.Memory{}, err
	}
	activated, err := updateStatus(ctx, tx, command, memorykit.StatusActive)
	if err != nil {
		return memorykit.Memory{}, err
	}
	if err := appendRevision(ctx, tx, activated, memorykit.RevisionActivate, command.Actor, command.Reason, command.Now, false, ""); err != nil {
		return memorykit.Memory{}, err
	}
	if err := tx.Commit(); err != nil {
		return memorykit.Memory{}, lifecycleWriteError("activate commit", err)
	}
	return activated, nil
}

func (s *Store) Correct(ctx context.Context, request memorykit.CorrectRequest) (memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	if err := memorykit.ValidateCorrectRequest(request, s.cfg.Limits); err != nil {
		return memorykit.Memory{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memorykit.Memory{}, wrapBackend("correct transaction", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := lockCommandTarget(ctx, tx, request.Command); err != nil {
		return memorykit.Memory{}, err
	}

	corrected, err := scanMemory(tx.QueryRowContext(ctx, `
		UPDATE memories SET
			content = $1, content_hash = $2, valid_from = $3, valid_until = $4,
			importance = $5, confidence = $6, version = version + 1, updated_at = $7
		WHERE id = $8 AND tenant_id = $9 AND subject_type = $10 AND subject_id = $11
		  AND version = $12
		RETURNING `+memoryColumns,
		request.Content, hashContent(request.Content), request.ValidFrom, nullableTime(request.ValidUntil),
		request.Importance, request.Confidence, request.Command.Now,
		request.Command.ID, request.Command.Scope.TenantID, request.Command.Scope.SubjectType,
		request.Command.Scope.SubjectID, request.Command.ExpectedVersion))
	if errors.Is(err, sql.ErrNoRows) {
		return memorykit.Memory{}, versionUpdateFailure(ctx, tx, request.Command.Scope, request.Command.ID)
	}
	if err != nil {
		return memorykit.Memory{}, lifecycleWriteError("correct memory", err)
	}
	if err := replaceSources(ctx, tx, corrected.ID, request.Sources, request.Command.Now); err != nil {
		return memorykit.Memory{}, lifecycleWriteError("correct sources", err)
	}
	if err := appendRevision(ctx, tx, corrected, memorykit.RevisionCorrect, request.Command.Actor, request.Command.Reason, request.Command.Now, false, ""); err != nil {
		return memorykit.Memory{}, err
	}
	if err := tx.Commit(); err != nil {
		return memorykit.Memory{}, lifecycleWriteError("correct commit", err)
	}
	return corrected, nil
}

func (s *Store) Dismiss(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	return s.transitionInactive(ctx, command, memorykit.StatusCandidate, memorykit.RevisionDismiss)
}

func (s *Store) Forget(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	return s.transitionInactive(ctx, command, memorykit.StatusActive, memorykit.RevisionForget)
}

func (s *Store) Erase(ctx context.Context, command memorykit.VersionedCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := memorykit.ValidateCommand(command, s.cfg.Limits); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrapBackend("erase transaction", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := lockCommandTarget(ctx, tx, command); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_embeddings WHERE memory_id = $1`, command.ID); err != nil {
		return lifecycleWriteError("erase embeddings", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_sources WHERE memory_id = $1`, command.ID); err != nil {
		return lifecycleWriteError("erase sources", err)
	}
	// The create fingerprint is private replay material. It is scrubbed with
	// content so erased input can never be reconstructed or accepted again.
	if _, err := tx.ExecContext(ctx, `UPDATE memory_revisions
		SET snapshot = snapshot - 'content' - 'create_fingerprint', content_erased = TRUE
		WHERE memory_id = $1`, command.ID); err != nil {
		return lifecycleWriteError("erase revisions", err)
	}
	erased, err := scanMemory(tx.QueryRowContext(ctx, `
		UPDATE memories
		SET content = '', content_hash = '', status = $1,
			version = version + 1, updated_at = $2
		WHERE id = $3 AND tenant_id = $4 AND subject_type = $5 AND subject_id = $6
		  AND version = $7
		RETURNING `+memoryColumns,
		memorykit.StatusInactive, command.Now, command.ID,
		command.Scope.TenantID, command.Scope.SubjectType, command.Scope.SubjectID, command.ExpectedVersion))
	if errors.Is(err, sql.ErrNoRows) {
		return versionUpdateFailure(ctx, tx, command.Scope, command.ID)
	}
	if err != nil {
		return lifecycleWriteError("erase memory", err)
	}
	if err := appendRevision(ctx, tx, erased, memorykit.RevisionErase, command.Actor, command.Reason, command.Now, true, ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return lifecycleWriteError("erase commit", err)
	}
	return nil
}

func (s *Store) Revisions(ctx context.Context, query memorykit.RevisionQuery) ([]memorykit.Revision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := memorykit.ValidateRevisionQuery(query, s.cfg.Limits); err != nil {
		return nil, err
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT TRUE FROM memories
		WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3 AND id = $4`,
		query.Scope.TenantID, query.Scope.SubjectType, query.Scope.SubjectID, query.MemoryID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, memorykit.ErrNotFound
	} else if err != nil {
		return nil, wrapBackend("revision identity", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		r.memory_id, r.version, r.action, r.actor, r.reason, r.snapshot, r.content_erased, r.created_at
		FROM memory_revisions r
		JOIN memories m ON m.id = r.memory_id
		WHERE m.tenant_id = $1 AND m.subject_type = $2 AND m.subject_id = $3
		  AND m.id = $4 AND ($5::bigint = 0 OR r.version < $5)
		ORDER BY r.version DESC LIMIT $6`,
		query.Scope.TenantID, query.Scope.SubjectType, query.Scope.SubjectID,
		query.MemoryID, query.BeforeVersion, query.Limit)
	if err != nil {
		return nil, wrapBackend("list revisions", err)
	}
	defer rows.Close()
	result := make([]memorykit.Revision, 0)
	for rows.Next() {
		revision, err := scanRevision(rows)
		if err != nil {
			return nil, wrapBackend("scan revision", err)
		}
		result = append(result, revision)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapBackend("list revisions", err)
	}
	return result, nil
}

func (s *Store) transitionInactive(
	ctx context.Context,
	command memorykit.VersionedCommand,
	requiredStatus memorykit.Status,
	action memorykit.RevisionAction,
) (memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	if err := memorykit.ValidateCommand(command, s.cfg.Limits); err != nil {
		return memorykit.Memory{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return memorykit.Memory{}, wrapBackend("transition transaction", err)
	}
	defer func() { _ = tx.Rollback() }()
	target, err := lockCommandTarget(ctx, tx, command)
	if err != nil {
		return memorykit.Memory{}, err
	}
	if target.Status != requiredStatus {
		return memorykit.Memory{}, memorykit.ErrConflict
	}
	updated, err := updateStatus(ctx, tx, command, memorykit.StatusInactive)
	if err != nil {
		return memorykit.Memory{}, err
	}
	if err := appendRevision(ctx, tx, updated, action, command.Actor, command.Reason, command.Now, false, ""); err != nil {
		return memorykit.Memory{}, err
	}
	if err := tx.Commit(); err != nil {
		return memorykit.Memory{}, lifecycleWriteError("transition commit", err)
	}
	return updated, nil
}

func (s *Store) idempotentReplay(
	ctx context.Context,
	tx *sql.Tx,
	request memorykit.CreateRequest,
	fingerprint string,
) (memorykit.Memory, bool, error) {
	var existingID, storedFingerprint string
	err := tx.QueryRowContext(ctx, `
		SELECT m.id, COALESCE(r.snapshot ->> 'create_fingerprint', '')
		FROM memories m
		LEFT JOIN memory_revisions r
		  ON r.memory_id = m.id AND r.version = 1 AND r.action = 'create'
		WHERE m.tenant_id = $1 AND m.subject_type = $2 AND m.subject_id = $3
		  AND m.idempotency_key = $4
		FOR UPDATE OF m`,
		request.Scope.TenantID, request.Scope.SubjectType, request.Scope.SubjectID,
		request.IdempotencyKey).Scan(&existingID, &storedFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return memorykit.Memory{}, false, nil
	}
	if err != nil {
		return memorykit.Memory{}, false, wrapBackend("idempotency lookup", err)
	}
	if existingID != request.ID || storedFingerprint == "" || storedFingerprint != fingerprint {
		return memorykit.Memory{}, false, memorykit.ErrConflict
	}
	memory, err := scanMemory(tx.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories
		WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3 AND id = $4`,
		request.Scope.TenantID, request.Scope.SubjectType, request.Scope.SubjectID, existingID))
	if errors.Is(err, sql.ErrNoRows) {
		return memorykit.Memory{}, false, memorykit.ErrConflict
	}
	if err != nil {
		return memorykit.Memory{}, false, wrapBackend("idempotency current memory", err)
	}
	return memory, true, nil
}

func lockIdempotencyIdentity(ctx context.Context, tx *sql.Tx, scope memorykit.Scope, key string) error {
	identity, err := json.Marshal(struct {
		Scope memorykit.Scope `json:"scope"`
		Key   string          `json:"key"`
	}{Scope: scope, Key: key})
	if err != nil {
		return fmt.Errorf("%w: encode idempotency identity", memorykit.ErrInvalidMemory)
	}
	var ignored any
	if err := tx.QueryRowContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, string(identity)).Scan(&ignored); err != nil {
		return wrapBackend("lock idempotency identity", err)
	}
	return nil
}

func (s *Store) supersedeCurrentActive(
	ctx context.Context,
	tx *sql.Tx,
	target memorykit.Memory,
	excludeID, actor, reason string,
	now time.Time,
	rejectAtOrAfter bool,
) error {
	active, err := scanMemory(tx.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories
		WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3
		  AND kind = $4 AND memory_key = $5 AND status = $6 AND id <> $7
		FOR UPDATE`,
		target.Scope.TenantID, target.Scope.SubjectType, target.Scope.SubjectID,
		target.Kind, target.Key, memorykit.StatusActive, excludeID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return wrapBackend("lock active memory", err)
	}
	// Two reviewers can approve sibling candidates at the same logical time.
	// Once one activation has advanced the key to that time, the sibling is a
	// stale competing decision and must conflict instead of immediately
	// superseding the winner. A later explicit activation can still supersede it.
	if rejectAtOrAfter && !active.UpdatedAt.Before(now) {
		return memorykit.ErrConflict
	}
	command := memorykit.VersionedCommand{
		Scope: active.Scope, ID: active.ID, ExpectedVersion: active.Version,
		Actor: actor, Reason: reason, Now: now,
	}
	inactive, err := updateStatus(ctx, tx, command, memorykit.StatusInactive)
	if err != nil {
		return err
	}
	return appendRevision(ctx, tx, inactive, memorykit.RevisionSupersede, actor, reason, now, false, "")
}

func lockCommandTarget(ctx context.Context, tx *sql.Tx, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	memory, err := scanMemory(tx.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories
		WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3 AND id = $4
		FOR UPDATE`, command.Scope.TenantID, command.Scope.SubjectType, command.Scope.SubjectID, command.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return memorykit.Memory{}, memorykit.ErrNotFound
	}
	if err != nil {
		return memorykit.Memory{}, wrapBackend("lock memory", err)
	}
	if memory.Version != command.ExpectedVersion {
		return memorykit.Memory{}, memorykit.ErrConflict
	}
	return memory, nil
}

func updateStatus(
	ctx context.Context,
	tx *sql.Tx,
	command memorykit.VersionedCommand,
	status memorykit.Status,
) (memorykit.Memory, error) {
	memory, err := scanMemory(tx.QueryRowContext(ctx, `UPDATE memories
		SET status = $1, version = version + 1, updated_at = $2
		WHERE id = $3 AND tenant_id = $4 AND subject_type = $5 AND subject_id = $6
		  AND version = $7
		RETURNING `+memoryColumns,
		status, command.Now, command.ID, command.Scope.TenantID,
		command.Scope.SubjectType, command.Scope.SubjectID, command.ExpectedVersion))
	if errors.Is(err, sql.ErrNoRows) {
		return memorykit.Memory{}, versionUpdateFailure(ctx, tx, command.Scope, command.ID)
	}
	if err != nil {
		return memorykit.Memory{}, lifecycleWriteError("update memory status", err)
	}
	return memory, nil
}

func versionUpdateFailure(ctx context.Context, tx *sql.Tx, scope memorykit.Scope, id string) error {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT TRUE FROM memories
		WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3 AND id = $4`,
		scope.TenantID, scope.SubjectType, scope.SubjectID, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return memorykit.ErrNotFound
	}
	if err != nil {
		return wrapBackend("classify version update", err)
	}
	return memorykit.ErrConflict
}

func insertMemory(ctx context.Context, tx *sql.Tx, memory memorykit.Memory) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO memories (
		id, tenant_id, subject_type, subject_id, kind, memory_key, status, content,
		content_hash, valid_from, valid_until, importance, confidence, source_agent_id,
		created_by, idempotency_key, version, created_at, updated_at
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		$15, $16, $17, $18, $19
	)`,
		memory.ID, memory.Scope.TenantID, memory.Scope.SubjectType, memory.Scope.SubjectID,
		memory.Kind, memory.Key, memory.Status, memory.Content, hashContent(memory.Content),
		memory.ValidFrom, nullableTime(memory.ValidUntil), memory.Importance, memory.Confidence,
		memory.SourceAgentID, memory.CreatedBy, memory.IdempotencyKey, memory.Version,
		memory.CreatedAt, memory.UpdatedAt)
	return err
}

func replaceSources(ctx context.Context, tx *sql.Tx, memoryID string, sources []memorykit.Source, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_sources WHERE memory_id = $1`, memoryID); err != nil {
		return err
	}
	for _, source := range sources {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_sources
			(memory_id, source_kind, source_ref, evidence_hash, created_at)
			VALUES ($1, $2, $3, $4, $5)`,
			memoryID, source.Kind, source.Ref, source.EvidenceHash, now); err != nil {
			return err
		}
	}
	return nil
}

func appendRevision(
	ctx context.Context,
	tx *sql.Tx,
	memory memorykit.Memory,
	action memorykit.RevisionAction,
	actor, reason string,
	createdAt time.Time,
	contentErased bool,
	createFingerprint string,
) error {
	snapshot, err := encodeRevisionSnapshot(memory, createFingerprint)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_revisions
		(memory_id, version, action, actor, reason, snapshot, content_erased, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		memory.ID, memory.Version, action, actor, reason, snapshot, contentErased, createdAt); err != nil {
		return lifecycleWriteError("append revision", err)
	}
	return nil
}

func createFingerprint(request memorykit.CreateRequest) (string, error) {
	request.Now = time.Time{}
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("%w: encode create fingerprint", memorykit.ErrInvalidMemory)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func hashContent(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func canonicalTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC()
}

func validateIdentity(scope memorykit.Scope, id string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: memory ID is required", memorykit.ErrInvalidMemory)
	}
	return nil
}

func lifecycleWriteError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) && pgError.Code == "23505" {
		return memorykit.ErrConflict
	}
	return wrapBackend(op, err)
}
