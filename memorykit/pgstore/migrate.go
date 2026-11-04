package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eruca/goagents/memorykit"
	pgvector "github.com/pgvector/pgvector-go"
)

const (
	supportedSchemaVersion = 2
	schemaLockSQL          = "SELECT pg_advisory_lock(hashtext('memorykit.schema'))"
	schemaUnlockSQL        = "SELECT pg_advisory_unlock(hashtext('memorykit.schema'))"
)

var migrationV1 = []string{
	`CREATE EXTENSION IF NOT EXISTS vector`,
	`CREATE TABLE IF NOT EXISTS memorykit_schema_versions (
  version INTEGER PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS memories (
  id UUID PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  subject_type TEXT NOT NULL CHECK (subject_type IN ('project','user')),
  subject_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('fact','decision','constraint','lesson')),
  memory_key TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('candidate','active','inactive')),
  content TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  valid_from TIMESTAMPTZ NOT NULL,
  valid_until TIMESTAMPTZ,
  importance SMALLINT NOT NULL CHECK (importance BETWEEN 0 AND 100),
  confidence DOUBLE PRECISION NOT NULL CHECK (confidence BETWEEN 0 AND 1),
  source_agent_id TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL,
  idempotency_key TEXT NOT NULL DEFAULT '',
  version BIGINT NOT NULL CHECK (version > 0),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  search_vector TSVECTOR GENERATED ALWAYS AS
    (to_tsvector('simple', coalesce(memory_key,'') || ' ' || coalesce(content,''))) STORED,
  CHECK (valid_until IS NULL OR valid_until > valid_from)
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS uq_memories_active_key
  ON memories (tenant_id, subject_type, subject_id, kind, memory_key)
  WHERE status = 'active'`,
	`CREATE UNIQUE INDEX IF NOT EXISTS uq_memories_idempotency
  ON memories (tenant_id, subject_type, subject_id, idempotency_key)
  WHERE idempotency_key <> ''`,
	`CREATE INDEX IF NOT EXISTS idx_memories_search ON memories USING GIN (search_vector)`,
	`CREATE TABLE IF NOT EXISTS memory_sources (
  memory_id UUID NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  source_kind TEXT NOT NULL,
  source_ref TEXT NOT NULL,
  evidence_hash TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (memory_id, source_kind, source_ref)
)`,
	`CREATE TABLE IF NOT EXISTS memory_revisions (
  memory_id UUID NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  version BIGINT NOT NULL,
  action TEXT NOT NULL CHECK (action IN
    ('create','activate','correct','supersede','forget','erase','dismiss')),
  actor TEXT NOT NULL,
  reason TEXT NOT NULL,
  snapshot JSONB NOT NULL,
  content_erased BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (memory_id, version)
)`,
	`CREATE TABLE IF NOT EXISTS memory_embeddings (
  memory_id UUID NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  embedding_profile_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  dimension INTEGER NOT NULL CHECK (dimension > 0),
  embedding vector NOT NULL,
  embedded_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (memory_id, embedding_profile_id)
)`,
	`CREATE TABLE IF NOT EXISTS memory_extraction_jobs (
  id UUID PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  subject_type TEXT NOT NULL CHECK (subject_type IN ('project','user')),
  subject_id TEXT NOT NULL,
  source_kind TEXT NOT NULL,
  source_ref TEXT NOT NULL,
  evidence_hash TEXT NOT NULL DEFAULT '',
  source_agent_id TEXT NOT NULL DEFAULT '',
  extractor_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','leased','completed','failed')),
  attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_until TIMESTAMPTZ,
  failure_code TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE INDEX IF NOT EXISTS idx_memory_extraction_claim
  ON memory_extraction_jobs (status, lease_until, created_at, id)`,
	`CREATE INDEX IF NOT EXISTS idx_memory_extraction_claimable
  ON memory_extraction_jobs (created_at, id) WHERE status IN ('pending','leased')`,
	`INSERT INTO memorykit_schema_versions(version, applied_at)
VALUES (1, now()) ON CONFLICT (version) DO NOTHING`,
}

// migrationV2 indexes the Chinese and Japanese grams of each memory, which
// the 'simple' tsvector cannot segment, so full-text recall can find gram
// matches through an index instead of scanning every memory in scope.
// backfillSearchGrams fills the column for memories written before it.
var migrationV2 = []string{
	`ALTER TABLE memories ADD COLUMN IF NOT EXISTS search_grams TEXT[] NOT NULL DEFAULT '{}'`,
	`CREATE INDEX IF NOT EXISTS idx_memories_search_grams ON memories USING GIN (search_grams)`,
}

func migrate(ctx context.Context, db *sql.DB) (returnErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return wrapBackend("migration connection", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, schemaLockSQL); err != nil {
		return wrapBackend("migration lock", err)
	}
	defer func() {
		// Unlock must not inherit a canceled startup context; closing conn is the final safeguard.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, schemaUnlockSQL); err != nil && returnErr == nil {
			returnErr = wrapBackend("migration unlock", err)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return wrapBackend("migration transaction", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range migrationV1 {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return wrapBackend("migration apply", err)
		}
	}
	var version int
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM memorykit_schema_versions").Scan(&version); err != nil {
		return wrapBackend("migration version", err)
	}
	if version > supportedSchemaVersion {
		return wrapBackend("migration version", fmt.Errorf("unsupported schema version %d", version))
	}
	if version < 2 {
		for _, statement := range migrationV2 {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return wrapBackend("migration apply", err)
			}
		}
		if err := backfillSearchGrams(ctx, tx); err != nil {
			return wrapBackend("migration backfill", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memorykit_schema_versions(version, applied_at)
VALUES (2, now()) ON CONFLICT (version) DO NOTHING`); err != nil {
			return wrapBackend("migration apply", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return wrapBackend("migration commit", err)
	}

	// A value round trip verifies that the installed extension and database/sql codec interoperate.
	var vector pgvector.Vector
	if err := conn.QueryRowContext(ctx, "SELECT $1::vector", pgvector.NewVector([]float32{0})).Scan(&vector); err != nil {
		return wrapBackend("vector compatibility", err)
	}
	if len(vector.Slice()) != 1 {
		return wrapBackend("vector compatibility", errors.New("unexpected vector dimensions"))
	}
	return nil
}

func backfillSearchGrams(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, memory_key, content FROM memories`)
	if err != nil {
		return err
	}
	grams := map[string][]string{}
	for rows.Next() {
		var id, key, content string
		if err := rows.Scan(&id, &key, &content); err != nil {
			_ = rows.Close()
			return err
		}
		if memoryGrams := searchGrams(key, content); len(memoryGrams) > 0 {
			grams[id] = memoryGrams
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, memoryGrams := range grams {
		if _, err := tx.ExecContext(ctx, `UPDATE memories SET search_grams = $1 WHERE id = $2`, memoryGrams, id); err != nil {
			return err
		}
	}
	return nil
}

// searchGrams is the search_grams value of a memory, matching the text its
// search_vector covers.
func searchGrams(key, content string) []string {
	return memorykit.IndexGrams(key + " " + content)
}
