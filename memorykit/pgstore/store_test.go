package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/eruca/goagents/memorykit"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestOpenRejectsBlankDSN(t *testing.T) {
	store, err := Open(context.Background(), " \t\n", validConfig())
	if err == nil {
		_ = store.Close()
		t.Fatal("Open() error = nil, want validation error")
	}
	if !errors.Is(err, memorykit.ErrInvalidMemory) {
		t.Fatalf("Open() error = %v, want ErrInvalidMemory", err)
	}
}

func TestOpenRejectsInvalidConfigBeforeConnecting(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "limits", mutate: func(cfg *Config) { cfg.Limits = memorykit.Limits{} }},
		{name: "blank profile", mutate: func(cfg *Config) { cfg.EmbeddingProfileID = "" }},
		{name: "profile whitespace", mutate: func(cfg *Config) { cfg.EmbeddingProfileID = " test-3d " }},
		{name: "dimensions", mutate: func(cfg *Config) { cfg.EmbeddingDimensions = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			test.mutate(&cfg)
			store, err := Open(context.Background(), "not-a-postgres-dsn", cfg)
			if err == nil {
				_ = store.Close()
				t.Fatal("Open() error = nil, want validation error")
			}
			if !errors.Is(err, memorykit.ErrInvalidMemory) {
				t.Fatalf("Open() error = %v, want ErrInvalidMemory", err)
			}
		})
	}
}

func TestOpenDBMissingVectorReturnsSafeInitializationError(t *testing.T) {
	db, state := openMigrationDriverDB(t, migrationDriverOptions{
		missingVector: true,
		schemaVersion: supportedSchemaVersion,
	})
	_, err := OpenDB(context.Background(), db, validConfig())
	if err == nil {
		t.Fatal("OpenDB() error = nil, want vector initialization error")
	}
	if memorykit.IsRecoverable(err) {
		t.Fatalf("OpenDB() error = %v, missing extension must not be recoverable", err)
	}
	if strings.Contains(err.Error(), "server-path") || strings.Contains(err.Error(), "vector.control") {
		t.Fatalf("OpenDB() leaked backend detail: %v", err)
	}
	if pingErr := db.PingContext(context.Background()); pingErr != nil {
		t.Fatalf("OpenDB() closed caller-owned DB after failure: %v", pingErr)
	}
	if state.committed || !state.rolledBack || !state.unlocked {
		t.Fatalf("DDL failure transaction state = commit:%v rollback:%v unlock:%v, want false/true/true",
			state.committed, state.rolledBack, state.unlocked)
	}
	if state.migrationExecs != 1 {
		t.Fatalf("DDL failure executed %d migration statements, want failure on first statement", state.migrationExecs)
	}
}

func TestOpenDBDoesNotOwnSuppliedConnectionPool(t *testing.T) {
	dsn := postgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := OpenDB(context.Background(), db, validConfig())
	if err != nil {
		t.Fatalf("OpenDB() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("Store.Close() closed caller-owned DB: %v", err)
	}
}

func TestOpenOwnsConnectionPool(t *testing.T) {
	store, err := Open(context.Background(), postgresDSN(t), validConfig())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}
	if err := store.db.PingContext(context.Background()); err == nil {
		t.Fatal("Store.Close() left owned DB usable")
	}
}

func TestOpenPreservesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store, err := Open(ctx, postgresDSN(t), validConfig())
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open() error = %v, want context.Canceled", err)
	}
	if memorykit.IsRecoverable(err) {
		t.Fatalf("Open() cancellation is recoverable: %v", err)
	}
}

func TestMigrationCreatesRequiredSchemaWithoutANNIndexes(t *testing.T) {
	store := openTestStore(t)
	wantTables := []string{
		"memories",
		"memory_sources",
		"memory_revisions",
		"memory_embeddings",
		"memory_extraction_jobs",
		"memorykit_schema_versions",
	}
	for _, table := range wantTables {
		var regclass string
		err := store.db.QueryRowContext(context.Background(), "SELECT to_regclass($1)", "public."+table).Scan(&regclass)
		if err != nil {
			t.Fatalf("query table %s: %v", table, err)
		}
		if regclass != table {
			t.Errorf("table %s = %q, want %q", table, regclass, table)
		}
	}

	var extensionVersion string
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT extversion FROM pg_extension WHERE extname = 'vector'
	`).Scan(&extensionVersion); err != nil {
		t.Fatalf("vector extension: %v", err)
	}
	if extensionVersion == "" {
		t.Fatal("vector extension version is blank")
	}

	var schemaVersion int
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT COALESCE(MAX(version), 0) FROM memorykit_schema_versions
	`).Scan(&schemaVersion); err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if schemaVersion != supportedSchemaVersion {
		t.Fatalf("schema version = %d, want %d", schemaVersion, supportedSchemaVersion)
	}

	rows, err := store.db.QueryContext(context.Background(), `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND tablename IN ('memories', 'memory_embeddings')
	`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		lower := strings.ToLower(definition)
		if strings.Contains(lower, "hnsw") || strings.Contains(lower, "ivfflat") {
			t.Errorf("unexpected ANN index: %s", definition)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index rows: %v", err)
	}
}

func TestMigrationContainsNoANNDDL(t *testing.T) {
	ddl := strings.ToLower(strings.Join(migrationV1, "\n"))
	if strings.Contains(ddl, "hnsw") || strings.Contains(ddl, "ivfflat") {
		t.Fatalf("migration contains ANN DDL: %s", ddl)
	}
}

func TestMigrationFutureVersionRollsBackBeforeCommitAndUnlocks(t *testing.T) {
	db, state := openMigrationDriverDB(t, migrationDriverOptions{
		schemaVersion: supportedSchemaVersion + 1,
	})
	_, err := OpenDB(context.Background(), db, validConfig())
	if err == nil {
		t.Fatal("OpenDB() error = nil, want unsupported schema version error")
	}
	if memorykit.IsRecoverable(err) {
		t.Fatalf("schema incompatibility is recoverable: %v", err)
	}
	if !state.versionQueriedInTx {
		t.Fatal("schema version was not verified inside the migration transaction")
	}
	if state.migrationExecs != len(migrationV1) {
		t.Fatalf("schema version checked after %d migration statements, want %d",
			state.migrationExecs, len(migrationV1))
	}
	if state.committed {
		t.Fatal("future schema version committed V1 migration")
	}
	if !state.rolledBack {
		t.Fatal("future schema version did not roll back V1 migration")
	}
	if !state.unlocked {
		t.Fatal("future schema version left advisory lock held")
	}
	if state.vectorChecked {
		t.Fatal("future schema version ran post-commit vector compatibility check")
	}
}

func TestMigrationRejectsUnsupportedSchemaVersionAndReleasesLock(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.db.ExecContext(context.Background(), `
		INSERT INTO memorykit_schema_versions(version, applied_at)
		VALUES ($1, now()) ON CONFLICT (version) DO NOTHING
	`, supportedSchemaVersion+1); err != nil {
		t.Fatalf("insert future schema version: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(),
			"DELETE FROM memorykit_schema_versions WHERE version > $1", supportedSchemaVersion)
	})

	_, err := OpenDB(context.Background(), store.db, validConfig())
	if err == nil {
		t.Fatal("OpenDB() error = nil, want unsupported schema version error")
	}
	if memorykit.IsRecoverable(err) {
		t.Fatalf("schema incompatibility is recoverable: %v", err)
	}

	otherDB, err := sql.Open("pgx", postgresDSN(t))
	if err != nil {
		t.Fatalf("open independent pool: %v", err)
	}
	t.Cleanup(func() { _ = otherDB.Close() })
	var acquired bool
	if err := otherDB.QueryRowContext(context.Background(),
		"SELECT pg_try_advisory_lock(hashtext('memorykit.schema'))").Scan(&acquired); err != nil {
		t.Fatalf("try advisory lock: %v", err)
	}
	if !acquired {
		t.Fatal("migration left advisory lock held")
	}
	if _, err := otherDB.ExecContext(context.Background(),
		"SELECT pg_advisory_unlock(hashtext('memorykit.schema'))"); err != nil {
		t.Fatalf("unlock advisory lock: %v", err)
	}
}

func TestWrapBackendClassification(t *testing.T) {
	temporaryNetwork := temporaryNetError{}
	tests := []struct {
		name        string
		err         error
		recoverable bool
	}{
		{name: "bad connection", err: driver.ErrBadConn, recoverable: true},
		{name: "temporary network", err: temporaryNetwork, recoverable: true},
		{name: "SQLSTATE connection", err: &pgconn.PgError{Code: "08006", Message: "connection failure"}, recoverable: true},
		{name: "SQLSTATE resources", err: &pgconn.PgError{Code: "53000", Message: "insufficient resources"}, recoverable: true},
		{name: "admin shutdown", err: &pgconn.PgError{Code: "57P01", Message: "admin shutdown"}, recoverable: true},
		{name: "crash shutdown", err: &pgconn.PgError{Code: "57P02", Message: "crash shutdown"}, recoverable: true},
		{name: "cannot connect now", err: &pgconn.PgError{Code: "57P03", Message: "cannot connect now"}, recoverable: true},
		{name: "unique violation", err: &pgconn.PgError{Code: "23505", Message: "secret unique detail"}},
		{name: "check violation", err: &pgconn.PgError{Code: "23514", Message: "secret check detail"}},
		{name: "scan error", err: errors.New("secret scan detail")},
		{name: "permanent network", err: permanentNetError{}},
		{name: "temporary non-network", err: temporaryNonNetworkError{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := wrapBackend("query", fmt.Errorf("outer: %w", test.err))
			if memorykit.IsRecoverable(got) != test.recoverable {
				t.Fatalf("IsRecoverable(%v) = %v, want %v", got, memorykit.IsRecoverable(got), test.recoverable)
			}
			var backend *memorykit.BackendError
			if !errors.As(got, &backend) {
				t.Fatalf("wrapBackend() error type = %T, want *memorykit.BackendError", got)
			}
			if strings.Contains(got.Error(), "secret") {
				t.Fatalf("wrapBackend() leaked backend detail: %v", got)
			}
		})
	}
}

func TestWrapBackendPreservesContextErrors(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("wrapped: %w", want)
			got := wrapBackend("query", wrapped)
			if got != wrapped {
				t.Fatalf("wrapBackend() changed context error: got %p, want %p", got, wrapped)
			}
			if !errors.Is(got, want) {
				t.Fatalf("wrapBackend() error = %v, want %v", got, want)
			}
			if memorykit.IsRecoverable(got) {
				t.Fatalf("context error is recoverable: %v", got)
			}
		})
	}
}

type temporaryNetError struct{}

func (temporaryNetError) Error() string   { return "temporary network failure" }
func (temporaryNetError) Timeout() bool   { return false }
func (temporaryNetError) Temporary() bool { return true }

type permanentNetError struct{}

func (permanentNetError) Error() string   { return "permanent network failure" }
func (permanentNetError) Timeout() bool   { return false }
func (permanentNetError) Temporary() bool { return false }

type temporaryNonNetworkError struct{}

func (temporaryNonNetworkError) Error() string   { return "temporary non-network failure" }
func (temporaryNonNetworkError) Temporary() bool { return true }

var (
	_ net.Error = temporaryNetError{}
	_ net.Error = permanentNetError{}
)
