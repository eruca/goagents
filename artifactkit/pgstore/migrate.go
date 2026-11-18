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
	schemaLockSQL   = "SELECT pg_advisory_lock(hashtext('artifactkit.schema'))"
	schemaUnlockSQL = "SELECT pg_advisory_unlock(hashtext('artifactkit.schema'))"
)

// migrationV1 creates the artifact table. ref uses the "C" collation so refs
// compare byte by byte, whatever the database collation is.
var migrationV1 = []string{
	`CREATE TABLE IF NOT EXISTS artifactkit_schema_versions (
  version INTEGER PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS artifacts (
  ref TEXT COLLATE "C" PRIMARY KEY,
  content BYTEA NOT NULL,
  content_type TEXT NOT NULL DEFAULT '',
  metadata_json TEXT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
)`,
	`INSERT INTO artifactkit_schema_versions (version, applied_at)
VALUES (1, now()) ON CONFLICT (version) DO NOTHING`,
}

// migrate applies the schema under a session advisory lock, so replicas
// starting together migrate one at a time, and in one transaction, so a
// failed or refused migration leaves nothing behind.
func migrate(ctx context.Context, db *sql.DB) (returnErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("artifactkit pgstore: migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, schemaLockSQL); err != nil {
		return fmt.Errorf("artifactkit pgstore: migration lock: %w", err)
	}
	defer func() {
		// Unlock must not inherit a canceled startup context; closing conn is the final safeguard.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, schemaUnlockSQL); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("artifactkit pgstore: migration unlock: %w", err)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifactkit pgstore: migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range migrationV1 {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("artifactkit pgstore: migration apply: %w", err)
		}
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM artifactkit_schema_versions`).Scan(&version); err != nil {
		return fmt.Errorf("artifactkit pgstore: migration version: %w", err)
	}
	if version > SchemaVersion {
		return fmt.Errorf("artifactkit pgstore: database schema version %d is newer than supported version %d", version, SchemaVersion)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifactkit pgstore: migration commit: %w", err)
	}
	return nil
}
