package pghealth

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	supportedSchemaVersion = 1
	schemaLockSQL          = "SELECT pg_advisory_lock(hashtext('llmkit.health.schema'))"
	schemaUnlockSQL        = "SELECT pg_advisory_unlock(hashtext('llmkit.health.schema'))"
)

// migrationV1 creates the health entries and the leases that stand for
// admitted calls. An entry's in-flight count is not stored: it is the number
// of its unexpired leases.
var migrationV1 = []string{
	`CREATE TABLE IF NOT EXISTS llmkit_health_schema_versions (
  version INTEGER PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS llmkit_provider_health (
  key TEXT PRIMARY KEY,
  account_alias TEXT NOT NULL,
  model_alias TEXT NOT NULL,
  provider TEXT NOT NULL,
  availability TEXT NOT NULL DEFAULT 'available'
    CHECK (availability IN ('available','degraded','unavailable')),
  max_concurrency INTEGER NOT NULL DEFAULT 0,
  quota_remaining INTEGER NOT NULL DEFAULT 0,
  quota_exhausted BOOLEAN NOT NULL DEFAULT FALSE,
  failure_streak INTEGER NOT NULL DEFAULT 0,
  last_failure_at TIMESTAMPTZ,
  cooldown_until TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS llmkit_provider_health_leases (
  lease_id UUID PRIMARY KEY,
  key TEXT NOT NULL,
  account_alias TEXT NOT NULL,
  owner TEXT NOT NULL,
  acquired_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE INDEX IF NOT EXISTS idx_llmkit_health_leases_account
  ON llmkit_provider_health_leases (account_alias, expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_llmkit_health_leases_owner
  ON llmkit_provider_health_leases (owner, key, acquired_at)`,
	`INSERT INTO llmkit_health_schema_versions(version, applied_at)
VALUES (1, now()) ON CONFLICT (version) DO NOTHING`,
}

// migrate applies the schema under a session advisory lock, so replicas
// starting together do not race on it.
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
		"SELECT COALESCE(MAX(version), 0) FROM llmkit_health_schema_versions").Scan(&version); err != nil {
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
