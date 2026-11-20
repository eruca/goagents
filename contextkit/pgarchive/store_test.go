package pgarchive

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/archive/archivetest"
)

func postgresDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CONTEXTKIT_POSTGRES_TEST_DSN"))
	if dsn == "" && os.Getenv("CONTEXTKIT_REQUIRE_POSTGRES") == "1" {
		t.Fatal("CONTEXTKIT_POSTGRES_TEST_DSN is required")
	}
	if dsn == "" {
		t.Skip("set CONTEXTKIT_POSTGRES_TEST_DSN")
	}
	return dsn
}

func openTestStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	store, err := Open(context.Background(), postgresDSN(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestConformance(t *testing.T) {
	archivetest.Run(t, func(t *testing.T, max int) contextkit.Archive {
		return openTestStore(t, Config{MaxMessagesPerScope: max})
	})
}

// TestStoresShareTheArchive opens two stores, as two replicas would, and
// reads from one what the other archived.
func TestStoresShareTheArchive(t *testing.T) {
	a, b := openTestStore(t, Config{}), openTestStore(t, Config{})
	ctx := contextkit.WithArchiveScope(context.Background(), "shared-"+time.Now().Format(time.RFC3339Nano))
	ref, err := a.Offload(ctx, contextkit.Message{Role: contextkit.RoleTool, ToolName: "read_page", Content: "invoice INV-7 total 4370 yuan"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.Read(ctx, ref); err != nil || got.ToolName != "read_page" {
		t.Fatalf("read from the other store = %+v, %v", got, err)
	}
	if hits, err := b.Search(ctx, "INV-7 total", 3); err != nil || len(hits) != 1 || hits[0].Ref != ref {
		t.Fatalf("search from the other store = %+v, %v", hits, err)
	}
	if err := b.DeleteScope(ctx, contextkit.ArchiveScope(ctx)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(ctx, ref); !errors.Is(err, contextkit.ErrArchiveNotFound) {
		t.Fatalf("read after DeleteScope = %v", err)
	}
}

func TestDeleteBefore(t *testing.T) {
	store := openTestStore(t, Config{})
	ctx := contextkit.WithArchiveScope(context.Background(), "retention-"+time.Now().Format(time.RFC3339Nano))
	ref, err := store.Offload(ctx, contextkit.Message{Role: contextkit.RoleUser, Content: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE contextkit_archive_messages SET created_at = now() - interval '2 days' WHERE ref = $1`, ref); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteBefore(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil || deleted < 1 {
		t.Fatalf("deleted = %d, %v", deleted, err)
	}
	if _, err := store.Read(ctx, ref); !errors.Is(err, contextkit.ErrArchiveNotFound) {
		t.Fatalf("read after DeleteBefore = %v", err)
	}
}

func TestOpenValidates(t *testing.T) {
	if _, err := Open(context.Background(), " ", Config{}); err == nil {
		t.Fatal("empty DSN accepted")
	}
	if _, err := OpenDB(context.Background(), nil, Config{}); err == nil {
		t.Fatal("nil database accepted")
	}
	if _, err := Open(context.Background(), postgresDSN(t), Config{MaxMessagesPerScope: -1}); err == nil {
		t.Fatal("negative bound accepted")
	}
}
