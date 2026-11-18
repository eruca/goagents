package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func postgresDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("WORKFLOWKIT_POSTGRES_TEST_DSN"))
	if dsn == "" && os.Getenv("WORKFLOWKIT_REQUIRE_POSTGRES") == "1" {
		t.Fatal("WORKFLOWKIT_POSTGRES_TEST_DSN is required")
	}
	if dsn == "" {
		t.Skip("set WORKFLOWKIT_POSTGRES_TEST_DSN")
	}
	return dsn
}

var schemaSequence atomic.Uint64

// isolatedSchemaDSN creates a fresh schema, dropped at cleanup, and returns a
// DSN whose connections use it. The conformance suite seeds fixed ids, so
// every store it opens needs its own tables.
func isolatedSchemaDSN(t *testing.T) string {
	t.Helper()
	dsn := postgresDSN(t)
	schema := fmt.Sprintf("workflowkit_test_%d_%d", os.Getpid(), schemaSequence.Add(1))

	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open schema admin DB: %v", err)
	}
	if _, err := adminDB.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = adminDB.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := adminDB.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated schema: %v", err)
		}
		_ = adminDB.Close()
	})

	scopedURL, err := url.Parse(dsn)
	if err != nil || scopedURL.Scheme == "" {
		t.Fatalf("test DSN must be a PostgreSQL URL: %v", err)
	}
	query := scopedURL.Query()
	query.Set("search_path", schema)
	scopedURL.RawQuery = query.Encode()
	return scopedURL.String()
}

// openPool opens a separate connection pool, standing in for another replica.
func openPool(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openStoreDB(t *testing.T, db *sql.DB) *Store {
	t.Helper()
	store, err := OpenDB(context.Background(), db)
	if err != nil {
		t.Fatalf("OpenDB returned error: %v", err)
	}
	return store
}

// openIsolatedStore opens a Store that owns its pool in a fresh schema.
func openIsolatedStore(t *testing.T) *Store {
	t.Helper()
	dsn := isolatedSchemaDSN(t)
	store, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	// Registered after the schema drop, so it runs first.
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close returned error: %v", err)
		}
	})
	return store
}
