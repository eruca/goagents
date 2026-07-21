package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
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

var registerMissingVectorDriver sync.Once

func openMissingVectorDB(t *testing.T) *sql.DB {
	t.Helper()
	registerMissingVectorDriver.Do(func() {
		sql.Register("memorykit_missing_vector", missingVectorDriver{})
	})
	db, err := sql.Open("memorykit_missing_vector", "")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type missingVectorDriver struct{}

func (missingVectorDriver) Open(string) (driver.Conn, error) {
	return &missingVectorConn{}, nil
}

type missingVectorConn struct{}

func (*missingVectorConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is unsupported")
}
func (*missingVectorConn) Close() error              { return nil }
func (*missingVectorConn) Begin() (driver.Tx, error) { return missingVectorTx{}, nil }
func (*missingVectorConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return missingVectorTx{}, nil
}
func (*missingVectorConn) Ping(context.Context) error { return nil }
func (*missingVectorConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(strings.ToLower(query), "create extension") {
		return nil, &pgconn.PgError{
			Code:    "0A000",
			Message: "extension vector is not available",
			Detail:  "server-path=/private/postgresql/extensions/vector.control",
		}
	}
	return driver.RowsAffected(0), nil
}
func (*missingVectorConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	return &singleValueRows{values: [][]driver.Value{{true}}}, nil
}

type missingVectorTx struct{}

func (missingVectorTx) Commit() error   { return nil }
func (missingVectorTx) Rollback() error { return nil }

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
	_ driver.Driver         = missingVectorDriver{}
	_ driver.Conn           = (*missingVectorConn)(nil)
	_ driver.ConnBeginTx    = (*missingVectorConn)(nil)
	_ driver.Pinger         = (*missingVectorConn)(nil)
	_ driver.ExecerContext  = (*missingVectorConn)(nil)
	_ driver.QueryerContext = (*missingVectorConn)(nil)
)
