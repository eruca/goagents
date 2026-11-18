package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SchemaVersion is the newest schema version this package can migrate to and
// read. Open refuses a database migrated by a newer version.
const SchemaVersion = 1

const (
	schemaLockSQL   = "SELECT pg_advisory_lock(hashtext('workflowkit.schema'))"
	schemaUnlockSQL = "SELECT pg_advisory_unlock(hashtext('workflowkit.schema'))"
)

// migrationV1 creates the run and step record tables. id uses the "C"
// collation so ordering by id is byte order, as in sqlitestore and
// MemoryStore, whatever the database collation is.
var migrationV1 = []string{
	`CREATE TABLE IF NOT EXISTS workflowkit_schema_versions (
  version INTEGER PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS workflow_runs (
  id TEXT COLLATE "C" PRIMARY KEY,
  status TEXT NOT NULL,
  input_ref TEXT NOT NULL DEFAULT '',
  output_ref TEXT NOT NULL DEFAULT '',
  agent_run_id TEXT NOT NULL DEFAULT '',
  audit_ref TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  approval_ref TEXT NOT NULL DEFAULT '',
  waiting_reason TEXT NOT NULL DEFAULT '',
  current_step TEXT NOT NULL DEFAULT '',
  completed_steps_json TEXT NOT NULL DEFAULT 'null',
  step_attempts_json TEXT NOT NULL DEFAULT 'null',
  step_retries INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMPTZ,
  metadata_json TEXT NOT NULL DEFAULT 'null',
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_until TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS workflow_step_records (
  run_id TEXT COLLATE "C" NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  record_json TEXT NOT NULL,
  PRIMARY KEY (run_id, seq)
)`,
	// Workers claim the oldest pending run on every poll.
	`CREATE INDEX IF NOT EXISTS workflow_runs_pending_created
  ON workflow_runs (created_at, id) WHERE status = 'pending'`,
	`CREATE INDEX IF NOT EXISTS workflow_runs_status_created
  ON workflow_runs (status, created_at, id)`,
	`INSERT INTO workflowkit_schema_versions (version, applied_at)
VALUES (1, now()) ON CONFLICT (version) DO NOTHING`,
}

// migrate applies the schema under a session advisory lock, so replicas
// starting together migrate one at a time, and in one transaction, so a
// failed or refused migration leaves nothing behind.
func migrate(ctx context.Context, db *sql.DB) (returnErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("workflowkit pgstore: migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, schemaLockSQL); err != nil {
		return fmt.Errorf("workflowkit pgstore: migration lock: %w", err)
	}
	defer func() {
		// Unlock must not inherit a canceled startup context; closing conn is the final safeguard.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, schemaUnlockSQL); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("workflowkit pgstore: migration unlock: %w", err)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("workflowkit pgstore: migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range migrationV1 {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("workflowkit pgstore: migration apply: %w", err)
		}
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM workflowkit_schema_versions`).Scan(&version); err != nil {
		return fmt.Errorf("workflowkit pgstore: migration version: %w", err)
	}
	if version > SchemaVersion {
		return fmt.Errorf("workflowkit pgstore: database schema version %d is newer than supported version %d", version, SchemaVersion)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workflowkit pgstore: migration commit: %w", err)
	}
	return nil
}
