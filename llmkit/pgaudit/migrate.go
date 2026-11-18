package pgaudit

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	supportedSchemaVersion = 1
	schemaLockSQL          = "SELECT pg_advisory_lock(hashtext('llmkit.audit.schema'))"
	schemaUnlockSQL        = "SELECT pg_advisory_unlock(hashtext('llmkit.audit.schema'))"
)

// migrationV1 creates one append-only table per record kind. seq is the
// insertion order, which stands in for JSONL append order. record is the
// sanitized record as JSON; json rather than jsonb keeps the encoded text as
// written, including \u0000 escapes jsonb rejects. task_id, route_id and
// attempt copy record fields for filtering and joining.
var migrationV1 = []string{
	`CREATE TABLE IF NOT EXISTS llmkit_audit_schema_versions (
  version INTEGER PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS llmkit_audit_routes (
  seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  task_id TEXT COLLATE "C" NOT NULL,
  route_id TEXT COLLATE "C" NOT NULL,
  attempt INTEGER NOT NULL,
  recorded_at TIMESTAMPTZ NOT NULL,
  record JSON NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS llmkit_audit_outcomes (
  seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  task_id TEXT COLLATE "C" NOT NULL,
  route_id TEXT COLLATE "C" NOT NULL,
  attempt INTEGER NOT NULL,
  recorded_at TIMESTAMPTZ NOT NULL,
  record JSON NOT NULL
)`,
	`CREATE INDEX IF NOT EXISTS idx_llmkit_audit_routes_task
  ON llmkit_audit_routes (task_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_llmkit_audit_outcomes_task
  ON llmkit_audit_outcomes (task_id, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_llmkit_audit_outcomes_route
  ON llmkit_audit_outcomes (route_id, attempt, seq)`,
	`INSERT INTO llmkit_audit_schema_versions(version, applied_at)
VALUES (1, now()) ON CONFLICT (version) DO NOTHING`,
}

// migrate applies the schema under a session advisory lock, so replicas
// starting together do not race on it, and refuses a database that a newer
// version of this package has migrated.
func migrate(ctx context.Context, db *sql.DB) (returnErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return wrap("migration connection", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, schemaLockSQL); err != nil {
		return wrap("migration lock", err)
	}
	defer func() {
		// Unlock must not inherit a cancelled startup context; closing conn
		// is the final safeguard.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, schemaUnlockSQL); err != nil && returnErr == nil {
			returnErr = wrap("migration unlock", err)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return wrap("migration transaction", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range migrationV1 {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return wrap("migration apply", err)
		}
	}
	var version int
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM llmkit_audit_schema_versions").Scan(&version); err != nil {
		return wrap("migration version", err)
	}
	if version > supportedSchemaVersion {
		return wrap("migration version", fmt.Errorf("unsupported schema version %d", version))
	}
	if err := tx.Commit(); err != nil {
		return wrap("migration commit", err)
	}
	return nil
}
