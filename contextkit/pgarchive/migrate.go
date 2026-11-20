package pgarchive

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	supportedSchemaVersion = 1
	schemaLockSQL          = "SELECT pg_advisory_lock(hashtext('contextkit.archive.schema'))"
	schemaUnlockSQL        = "SELECT pg_advisory_unlock(hashtext('contextkit.archive.schema'))"
)

// migrationV1 keeps one row per archived message. seq is the insertion
// order, which ranks ties and decides what a full scope drops. message is the
// contextkit.Message as JSON; terms are its search terms (archive.Terms).
var migrationV1 = []string{
	`CREATE TABLE IF NOT EXISTS contextkit_archive_schema_versions (
  version INTEGER PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS contextkit_archive_messages (
  seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  scope TEXT COLLATE "C" NOT NULL,
  ref TEXT COLLATE "C" NOT NULL,
  message JSON NOT NULL,
  terms TEXT[] NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE (scope, ref)
)`,
	`CREATE INDEX IF NOT EXISTS idx_contextkit_archive_scope_seq
  ON contextkit_archive_messages (scope, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_contextkit_archive_terms
  ON contextkit_archive_messages USING GIN (terms)`,
	`CREATE INDEX IF NOT EXISTS idx_contextkit_archive_created
  ON contextkit_archive_messages (created_at)`,
	`INSERT INTO contextkit_archive_schema_versions(version, applied_at)
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
		"SELECT COALESCE(MAX(version), 0) FROM contextkit_archive_schema_versions").Scan(&version); err != nil {
		return wrap("migration version", err)
	}
	if version > supportedSchemaVersion {
		return wrap("migration version", fmt.Errorf("unsupported schema version %d", version))
	}
	return wrap("migration commit", tx.Commit())
}
