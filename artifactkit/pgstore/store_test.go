package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/artifactkit/storetest"
)

func TestStoreConformance(t *testing.T) {
	postgresDSN(t)
	storetest.RunStoreConformance(t, func(t *testing.T) artifactkit.Store {
		return openIsolatedStore(t)
	})
}

func TestOpenRequiresDSNAndDB(t *testing.T) {
	if _, err := Open(context.Background(), " "); err == nil {
		t.Fatal("Open with an empty DSN returned no error")
	}
	if _, err := OpenDB(context.Background(), nil); err == nil {
		t.Fatal("OpenDB with a nil pool returned no error")
	}
}

func TestPutOverwritesAndKeepsCreatedAt(t *testing.T) {
	db := openPool(t, isolatedSchemaDSN(t))
	store := openStoreDB(t, db)
	ctx := t.Context()

	if err := store.Put(ctx, artifactkit.Artifact{
		Ref:         "artifact:wf-1:output",
		Content:     []byte("first"),
		ContentType: "text/plain",
		Metadata:    map[string]any{"version": 1, "keep": "only-first"},
	}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	var created, updated time.Time
	if err := db.QueryRowContext(ctx, `SELECT created_at, updated_at FROM artifacts WHERE ref = $1`, "artifact:wf-1:output").Scan(&created, &updated); err != nil {
		t.Fatalf("read timestamps: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	if err := store.Put(ctx, artifactkit.Artifact{
		Ref:      "artifact:wf-1:output",
		Content:  []byte("second"),
		Metadata: map[string]any{"version": 2},
	}); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	got, err := store.Get(ctx, "artifact:wf-1:output")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Content) != "second" || got.ContentType != "" {
		t.Fatalf("Get = %+v, want second write with empty content type", got)
	}
	if len(got.Metadata) != 1 || got.Metadata["version"] != float64(2) {
		t.Fatalf("metadata = %#v, want only version 2 as float64", got.Metadata)
	}

	var created2, updated2 time.Time
	if err := db.QueryRowContext(ctx, `SELECT created_at, updated_at FROM artifacts WHERE ref = $1`, "artifact:wf-1:output").Scan(&created2, &updated2); err != nil {
		t.Fatalf("read timestamps: %v", err)
	}
	if !created2.Equal(created) {
		t.Fatalf("created_at changed from %v to %v", created, created2)
	}
	if !updated2.After(updated) {
		t.Fatalf("updated_at = %v, want after %v", updated2, updated)
	}
}

func TestEmptyContentAndMetadataReadBackNil(t *testing.T) {
	store := openIsolatedStore(t)
	ctx := t.Context()
	for _, artifact := range []artifactkit.Artifact{
		{Ref: "artifact:nil"},
		{Ref: "artifact:empty", Content: []byte{}, Metadata: map[string]any{}},
	} {
		if err := store.Put(ctx, artifact); err != nil {
			t.Fatalf("Put %s: %v", artifact.Ref, err)
		}
		got, err := store.Get(ctx, artifact.Ref)
		if err != nil {
			t.Fatalf("Get %s: %v", artifact.Ref, err)
		}
		if got.Content != nil || got.Metadata != nil || got.Ref != artifact.Ref {
			t.Fatalf("Get %s = %#v, want nil content and metadata", artifact.Ref, got)
		}
	}
}

func TestUnstorableRefs(t *testing.T) {
	store := openIsolatedStore(t)
	ctx := t.Context()
	for _, ref := range []string{"artifact:\x00nul", "artifact:\xffbad"} {
		if err := store.Put(ctx, artifactkit.Artifact{Ref: ref, Content: []byte("x")}); err == nil {
			t.Fatalf("Put %q returned no error", ref)
		}
		if _, err := store.Get(ctx, ref); !errors.Is(err, artifactkit.ErrArtifactNotFound) {
			t.Fatalf("Get %q error = %v, want ErrArtifactNotFound", ref, err)
		}
	}
}

func TestPutRejectsUnencodableMetadata(t *testing.T) {
	store := openIsolatedStore(t)
	err := store.Put(t.Context(), artifactkit.Artifact{
		Ref:      "artifact:bad-metadata",
		Metadata: map[string]any{"fn": func() {}},
	})
	if err == nil {
		t.Fatal("Put with unencodable metadata returned no error")
	}
	if _, err := store.Get(t.Context(), "artifact:bad-metadata"); !errors.Is(err, artifactkit.ErrArtifactNotFound) {
		t.Fatalf("Get after failed Put error = %v, want ErrArtifactNotFound", err)
	}
}

// TestTwoPoolsConcurrentPutGet stands in for two replicas sharing one
// database: each writes and reads through its own pool.
func TestTwoPoolsConcurrentPutGet(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	stores := []*Store{openStoreDB(t, openPool(t, dsn)), openStoreDB(t, openPool(t, dsn))}
	ctx := t.Context()
	const shared = "artifact:shared"
	const perReplica = 20

	written := map[string]bool{}
	for r := range stores {
		for i := range perReplica {
			written[fmt.Sprintf("replica-%d-write-%d", r, i)] = true
		}
	}

	errs := make(chan error, 4*len(stores)*perReplica)
	var wg sync.WaitGroup
	for r, store := range stores {
		other := stores[1-r]
		wg.Go(func() {
			for i := range perReplica {
				payload := fmt.Sprintf("replica-%d-write-%d", r, i)
				if err := store.Put(ctx, artifactkit.Artifact{
					Ref:         shared,
					Content:     []byte(payload),
					ContentType: "text/plain",
					Metadata:    map[string]any{"payload": payload},
				}); err != nil {
					errs <- fmt.Errorf("shared Put %s: %w", payload, err)
					return
				}
				own := fmt.Sprintf("artifact:replica-%d:%d", r, i)
				if err := store.Put(ctx, artifactkit.Artifact{Ref: own, Content: []byte(payload)}); err != nil {
					errs <- fmt.Errorf("own Put %s: %w", own, err)
					return
				}
				got, err := other.Get(ctx, own)
				if err != nil {
					errs <- fmt.Errorf("Get %s from other pool: %w", own, err)
					return
				}
				if string(got.Content) != payload {
					errs <- fmt.Errorf("Get %s from other pool = %q, want %q", own, got.Content, payload)
				}
			}
		})
		wg.Go(func() {
			for range perReplica {
				got, err := store.Get(ctx, shared)
				if errors.Is(err, artifactkit.ErrArtifactNotFound) {
					continue
				}
				if err != nil {
					errs <- fmt.Errorf("shared Get: %w", err)
					return
				}
				// A read sees one whole write, never a mix of two.
				if !written[string(got.Content)] || got.Metadata["payload"] != string(got.Content) {
					errs <- fmt.Errorf("shared Get = %q with metadata %v, want one consistent write", got.Content, got.Metadata)
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	first, err := stores[0].Get(ctx, shared)
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	second, err := stores[1].Get(ctx, shared)
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	if !written[string(first.Content)] || string(first.Content) != string(second.Content) {
		t.Fatalf("final shared content = %q and %q, want the same last write", first.Content, second.Content)
	}
	var count int
	if err := stores[0].db.QueryRowContext(ctx, `SELECT count(*) FROM artifacts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if want := 1 + len(stores)*perReplica; count != want {
		t.Fatalf("artifact rows = %d, want %d", count, want)
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	db := openPool(t, isolatedSchemaDSN(t))
	store := openStoreDB(t, db)
	if err := store.Put(t.Context(), artifactkit.Artifact{Ref: "artifact:kept", Content: []byte("kept")}); err != nil {
		t.Fatal(err)
	}
	reopened := openStoreDB(t, db)
	got, err := reopened.Get(t.Context(), "artifact:kept")
	if err != nil || string(got.Content) != "kept" {
		t.Fatalf("Get after reopen = %+v, %v; want kept", got, err)
	}

	rows, err := db.QueryContext(t.Context(), `SELECT version FROM artifactkit_schema_versions ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0] != SchemaVersion {
		t.Fatalf("schema versions = %v, want [%d]", versions, SchemaVersion)
	}
}

func TestMigrationConcurrentOpensSucceed(t *testing.T) {
	dsn := isolatedSchemaDSN(t)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		db := openPool(t, dsn)
		wg.Go(func() {
			_, errs[i] = OpenDB(t.Context(), db)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent OpenDB returned error: %v", err)
		}
	}
}

func TestMigrationRejectsNewerSchemaVersionAndReleasesLock(t *testing.T) {
	db := openPool(t, isolatedSchemaDSN(t))
	openStoreDB(t, db)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO artifactkit_schema_versions (version, applied_at) VALUES ($1, now())`, SchemaVersion+1); err != nil {
		t.Fatalf("insert newer schema version: %v", err)
	}
	if _, err := OpenDB(t.Context(), db); err == nil {
		t.Fatal("OpenDB accepted a newer schema version")
	}

	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var locked bool
	if err := conn.QueryRowContext(t.Context(), `SELECT pg_try_advisory_lock(hashtext('artifactkit.schema'))`).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("migration left the schema advisory lock held")
	}
	if _, err := conn.ExecContext(t.Context(), schemaUnlockSQL); err != nil {
		t.Fatal(err)
	}
}

func TestCloseLeavesCallerPoolOpen(t *testing.T) {
	db := openPool(t, isolatedSchemaDSN(t))
	store := openStoreDB(t, db)
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("caller pool closed by Store.Close: %v", err)
	}
}
