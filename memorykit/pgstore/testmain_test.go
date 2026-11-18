package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eruca/goagents/memorykit"
	"github.com/jackc/pgx/v5/pgconn"
)

func postgresDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("MEMORYKIT_POSTGRES_TEST_DSN"))
	if dsn == "" && os.Getenv("MEMORYKIT_REQUIRE_POSTGRES") == "1" {
		t.Fatal("MEMORYKIT_POSTGRES_TEST_DSN is required")
	}
	if dsn == "" {
		t.Skip("set MEMORYKIT_POSTGRES_TEST_DSN")
	}
	return dsn
}

func validConfig() Config {
	return Config{
		Limits: memorykit.Limits{
			Version:             "project-memory-v1",
			MaxKeyRunes:         128,
			MaxContentRunes:     4096,
			MaxMetadataRunes:    512,
			MaxSourcesPerMemory: 16,
			MaxListItems:        100,
		},
		EmbeddingProfileID:  "test-3d",
		EmbeddingDimensions: 3,
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), postgresDSN(t), validConfig())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}

var migrationDriverSequence atomic.Uint64

// openIsolatedSchemaStore opens a Store migrated into a fresh schema that is
// dropped at cleanup, for tests that seed fixed IDs or need an empty table.
func openIsolatedSchemaStore(t *testing.T) *Store {
	t.Helper()
	dsn := postgresDSN(t)
	schema := fmt.Sprintf("memorykit_test_%d_%d", os.Getpid(), migrationDriverSequence.Add(1))

	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open schema admin DB: %v", err)
	}
	if _, err := adminDB.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = adminDB.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	dropSchema := func() {
		if _, err := adminDB.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated schema: %v", err)
		}
		_ = adminDB.Close()
	}

	scopedURL, err := url.Parse(dsn)
	if err != nil || scopedURL.Scheme == "" {
		dropSchema()
		t.Fatalf("test DSN must be a PostgreSQL URL: %v", err)
	}
	query := scopedURL.Query()
	query.Set("search_path", schema+",public")
	scopedURL.RawQuery = query.Encode()
	db, err := sql.Open("pgx", scopedURL.String())
	if err != nil {
		dropSchema()
		t.Fatalf("open isolated schema DB: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		dropSchema()
	})
	store, err := OpenDB(context.Background(), db, validConfig())
	if err != nil {
		t.Fatalf("OpenDB() in isolated schema: %v", err)
	}
	return store
}

func openMigrationDriverDB(t *testing.T, options migrationDriverOptions) (*sql.DB, *migrationDriverState) {
	t.Helper()
	state := &migrationDriverState{options: options}
	driverName := fmt.Sprintf("memorykit_migration_%d", migrationDriverSequence.Add(1))
	sql.Register(driverName, migrationDriver{state: state})
	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, state
}

type migrationDriverOptions struct {
	missingVector bool
	schemaVersion int64
}

type migrationDriverState struct {
	options            migrationDriverOptions
	inTransaction      bool
	committed          bool
	rolledBack         bool
	unlocked           bool
	versionQueriedInTx bool
	vectorChecked      bool
	migrationExecs     int
}

type migrationDriver struct{ state *migrationDriverState }

func (d migrationDriver) Open(string) (driver.Conn, error) {
	return &migrationConn{state: d.state}, nil
}

type migrationConn struct{ state *migrationDriverState }

func (*migrationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is unsupported")
}
func (*migrationConn) Close() error { return nil }
func (c *migrationConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *migrationConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.state.inTransaction = true
	return &migrationTx{state: c.state}, nil
}
func (*migrationConn) Ping(context.Context) error { return nil }
func (c *migrationConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	lowerQuery := strings.ToLower(query)
	if strings.Contains(lowerQuery, "pg_advisory_unlock") {
		c.state.unlocked = true
	}
	if c.state.inTransaction {
		c.state.migrationExecs++
	}
	if c.state.options.missingVector && strings.Contains(lowerQuery, "create extension") {
		return nil, &pgconn.PgError{
			Code:    "0A000",
			Message: "extension vector is not available",
			Detail:  "server-path=/private/postgresql/extensions/vector.control",
		}
	}
	return driver.RowsAffected(0), nil
}
func (c *migrationConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	lowerQuery := strings.ToLower(query)
	if strings.Contains(lowerQuery, "max(version)") {
		c.state.versionQueriedInTx = c.state.inTransaction
		return &singleValueRows{values: [][]driver.Value{{c.state.options.schemaVersion}}}, nil
	}
	if strings.Contains(lowerQuery, "::vector") {
		c.state.vectorChecked = true
		return &singleValueRows{values: [][]driver.Value{{"[0]"}}}, nil
	}
	return &singleValueRows{values: [][]driver.Value{{true}}}, nil
}

type migrationTx struct{ state *migrationDriverState }

func (tx *migrationTx) Commit() error {
	tx.state.committed = true
	tx.state.inTransaction = false
	return nil
}
func (tx *migrationTx) Rollback() error {
	tx.state.rolledBack = true
	tx.state.inTransaction = false
	return nil
}

type singleValueRows struct {
	values [][]driver.Value
	index  int
}

func (*singleValueRows) Columns() []string { return []string{"value"} }
func (*singleValueRows) Close() error      { return nil }
func (r *singleValueRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

var (
	_ driver.Driver         = migrationDriver{}
	_ driver.Conn           = (*migrationConn)(nil)
	_ driver.ConnBeginTx    = (*migrationConn)(nil)
	_ driver.Pinger         = (*migrationConn)(nil)
	_ driver.ExecerContext  = (*migrationConn)(nil)
	_ driver.QueryerContext = (*migrationConn)(nil)
)
