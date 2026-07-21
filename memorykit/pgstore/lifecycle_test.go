package pgstore

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/storetest"
)

const conformanceTenantID = "tenant-conformance"

func TestLifecycleConformance(t *testing.T) {
	storetest.RunLifecycleConformance(t, func(t *testing.T) memorykit.LifecycleStore {
		t.Helper()
		return openIsolatedStore(t)
	})
}

func TestRevisionSnapshotUsesPrivateSnakeCaseContentKey(t *testing.T) {
	store := openIsolatedStore(t)
	created, err := store.Create(context.Background(), lifecycleCreateRequest(
		"e1111111-1111-1111-1111-111111111111", memorykit.StatusCandidate, "original content"))
	if err != nil {
		t.Fatal(err)
	}

	var hasContent, hasPublicContent bool
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT snapshot ? 'content', snapshot ? 'Content'
		FROM memory_revisions WHERE memory_id = $1 AND version = $2
	`, created.ID, created.Version).Scan(&hasContent, &hasPublicContent); err != nil {
		t.Fatalf("query raw revision snapshot: %v", err)
	}
	if !hasContent || hasPublicContent {
		t.Fatalf("revision keys content/Content = %v/%v, want true/false", hasContent, hasPublicContent)
	}
}

func TestEraseScrubsPrivateDataFromAllPostgreSQLTables(t *testing.T) {
	store := openIsolatedStore(t)
	request := lifecycleCreateRequest(
		"e2111111-1111-1111-1111-111111111111", memorykit.StatusActive, "private content")
	request.IdempotencyKey = "create:privacy:erase"
	created, err := store.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var hasFingerprint bool
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT length(snapshot ->> 'create_fingerprint') = 64
		FROM memory_revisions WHERE memory_id = $1 AND version = 1
	`, created.ID).Scan(&hasFingerprint); err != nil {
		t.Fatalf("query create fingerprint: %v", err)
	}
	if !hasFingerprint {
		t.Fatal("create revision did not retain the internal replay fingerprint")
	}
	if _, err := store.db.ExecContext(context.Background(), `
		INSERT INTO memory_embeddings
			(memory_id, embedding_profile_id, content_hash, dimension, embedding, embedded_at)
		SELECT id, 'test-3d', content_hash, 3, '[1,0,0]'::vector, $2
		FROM memories WHERE id = $1
	`, created.ID, created.CreatedAt); err != nil {
		t.Fatalf("insert embedding fixture: %v", err)
	}

	if err := store.Erase(context.Background(), memorykit.VersionedCommand{
		Scope: created.Scope, ID: created.ID, ExpectedVersion: created.Version,
		Actor: "privacy-admin", Reason: "erasure", Now: created.UpdatedAt.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	var embeddings, sources, snapshotsWithPrivatePayload int
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT
			(SELECT count(*) FROM memory_embeddings WHERE memory_id = $1),
			(SELECT count(*) FROM memory_sources WHERE memory_id = $1),
			(SELECT count(*) FROM memory_revisions
			 WHERE memory_id = $1 AND
				(snapshot ? 'content' OR snapshot ? 'Content' OR snapshot ? 'create_fingerprint'))
	`, created.ID).Scan(&embeddings, &sources, &snapshotsWithPrivatePayload); err != nil {
		t.Fatalf("query erased private rows: %v", err)
	}
	if embeddings != 0 || sources != 0 || snapshotsWithPrivatePayload != 0 {
		t.Fatalf("private rows embeddings/sources/payload snapshots = %d/%d/%d",
			embeddings, sources, snapshotsWithPrivatePayload)
	}
	var storedContent, storedContentHash string
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT content, content_hash FROM memories WHERE id = $1
	`, created.ID).Scan(&storedContent, &storedContentHash); err != nil {
		t.Fatalf("query erased memory payload: %v", err)
	}
	if storedContent != "" || storedContentHash != "" {
		t.Fatalf("erased memory content/hash = %q/%q, want both empty", storedContent, storedContentHash)
	}

	var finalHasSourceRef, finalHasEvidenceHash, finalHasEmbedding bool
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT snapshot ? 'source_ref', snapshot ? 'evidence_hash', snapshot ? 'embedding'
		FROM memory_revisions WHERE memory_id = $1 ORDER BY version DESC LIMIT 1
	`, created.ID).Scan(&finalHasSourceRef, &finalHasEvidenceHash, &finalHasEmbedding); err != nil {
		t.Fatalf("query final erase snapshot: %v", err)
	}
	if finalHasSourceRef || finalHasEvidenceHash || finalHasEmbedding {
		t.Fatalf("final erase snapshot retained private metadata: %v/%v/%v",
			finalHasSourceRef, finalHasEvidenceHash, finalHasEmbedding)
	}
}

func TestConcurrentActivationHasExactlyOneWinner(t *testing.T) {
	store := openIsolatedStore(t)
	current, err := store.Create(context.Background(), lifecycleCreateRequest(
		"e3000000-0000-4000-8000-000000000000", memorykit.StatusActive, "current active"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(context.Background(), lifecycleCreateRequest(
		"e3111111-1111-1111-1111-111111111111", memorykit.StatusCandidate, "first candidate"))
	if err != nil {
		t.Fatal(err)
	}

	// Hold the current active row until both serializable Activate transactions
	// have established snapshots and are waiting on its lock. This proves real
	// transaction overlap instead of relying on goroutine scheduling.
	blocker, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback() })
	var lockedID string
	if err := blocker.QueryRowContext(context.Background(), `
		SELECT id FROM memories WHERE id = $1 FOR UPDATE
	`, current.ID).Scan(&lockedID); err != nil {
		t.Fatalf("lock current active: %v", err)
	}
	secondRequest := lifecycleCreateRequest(
		"e3222222-2222-2222-2222-222222222222", memorykit.StatusCandidate, "second candidate")
	second, err := store.Create(context.Background(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsByCall := make([]error, 2)
	commands := []memorykit.VersionedCommand{
		{Scope: first.Scope, ID: first.ID, ExpectedVersion: first.Version, Actor: "reviewer", Reason: "race", Now: first.UpdatedAt.Add(time.Minute)},
		{Scope: second.Scope, ID: second.ID, ExpectedVersion: second.Version, Actor: "reviewer", Reason: "race", Now: second.UpdatedAt.Add(2 * time.Minute)},
	}
	var workers sync.WaitGroup
	for i := range commands {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			_, errorsByCall[index] = store.Activate(context.Background(), commands[index])
		}(i)
	}
	close(start)
	waitForLockWaiters(t, store, 2)
	if err := blocker.Rollback(); err != nil {
		t.Fatalf("release current active lock: %v", err)
	}
	workers.Wait()

	succeeded, conflicted := 0, 0
	for _, err := range errorsByCall {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, memorykit.ErrConflict):
			conflicted++
		default:
			t.Fatalf("activation error = %v, want nil or ErrConflict", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("activation success/conflict = %d/%d, want 1/1", succeeded, conflicted)
	}
	active, err := store.List(context.Background(), memorykit.ListQuery{
		Scope: first.Scope, Kind: first.Kind, Status: memorykit.StatusActive, Key: first.Key, Limit: 10,
	})
	if err != nil || len(active) != 1 {
		t.Fatalf("active memories = %#v, %v", active, err)
	}
}

func waitForLockWaiters(t *testing.T, store *Store, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := store.db.QueryRowContext(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%FOR UPDATE%'
		`).Scan(&count); err != nil {
			t.Fatalf("query lock waiters: %v", err)
		}
		if count >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d activation lock waiters", want)
}

func TestCreateIdempotencyReturnsExistingRecordWithoutRevision(t *testing.T) {
	store := openIsolatedStore(t)
	request := lifecycleCreateRequest(
		"e4111111-1111-1111-1111-111111111111", memorykit.StatusCandidate, "idempotent content")
	request.IdempotencyKey = "create:idempotency:postgres"
	testZone := time.FixedZone("test-zone", 5*60*60)
	request.ValidFrom = request.ValidFrom.In(testZone)
	request.Now = request.Now.In(testZone)
	created, err := store.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Now = request.Now.Add(time.Hour)
	replayed, err := store.Create(context.Background(), request)
	if err != nil || !reflect.DeepEqual(replayed, created) {
		t.Fatalf("replayed/current = %#v/%#v, %v", replayed, created, err)
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{
		Scope: created.Scope, MemoryID: created.ID, Limit: 10,
	})
	if err != nil || len(revisions) != 1 {
		t.Fatalf("revisions after replay = %#v, %v", revisions, err)
	}
	request.Content = "different content"
	if _, err := store.Create(context.Background(), request); !errors.Is(err, memorykit.ErrConflict) {
		t.Fatalf("changed idempotency replay error = %v, want ErrConflict", err)
	}
}

func TestConcurrentIdenticalCreateIsIdempotent(t *testing.T) {
	store := openIsolatedStore(t)
	request := lifecycleCreateRequest(
		"e5888888-8888-8888-8888-888888888888", memorykit.StatusCandidate, "concurrent replay")
	request.IdempotencyKey = "create:idempotency:concurrent"

	start := make(chan struct{})
	results := make([]memorykit.Memory, 2)
	errorsByCall := make([]error, 2)
	var workers sync.WaitGroup
	for index := range results {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			results[index], errorsByCall[index] = store.Create(context.Background(), request)
		}(index)
	}
	close(start)
	workers.Wait()

	for index, err := range errorsByCall {
		if err != nil {
			t.Fatalf("Create call %d error = %v", index, err)
		}
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatalf("concurrent results differ: %#v / %#v", results[0], results[1])
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{
		Scope: request.Scope, MemoryID: request.ID, Limit: 10,
	})
	if err != nil || len(revisions) != 1 {
		t.Fatalf("concurrent create revisions = %#v, %v", revisions, err)
	}
}

func TestLifecycleMethodsRejectForeignScopeWithoutMutation(t *testing.T) {
	store := openIsolatedStore(t)
	created, err := store.Create(context.Background(), lifecycleCreateRequest(
		"e7111111-1111-1111-1111-111111111111", memorykit.StatusCandidate, "scoped content"))
	if err != nil {
		t.Fatal(err)
	}
	foreign := created.Scope
	foreign.SubjectID = "project-foreign"
	command := memorykit.VersionedCommand{
		Scope: foreign, ID: created.ID, ExpectedVersion: created.Version,
		Actor: "reviewer", Reason: "foreign", Now: created.UpdatedAt.Add(time.Minute),
	}
	checks := []struct {
		name string
		run  func() error
	}{
		{name: "sources", run: func() error { _, err := store.Sources(context.Background(), foreign, created.ID); return err }},
		{name: "revisions", run: func() error {
			_, err := store.Revisions(context.Background(), memorykit.RevisionQuery{
				Scope: foreign, MemoryID: created.ID, Limit: 10,
			})
			return err
		}},
		{name: "activate", run: func() error { _, err := store.Activate(context.Background(), command); return err }},
		{name: "correct", run: func() error {
			_, err := store.Correct(context.Background(), memorykit.CorrectRequest{
				Command: command, Content: "foreign correction", ValidFrom: created.ValidFrom,
				Importance: created.Importance, Confidence: created.Confidence,
			})
			return err
		}},
		{name: "dismiss", run: func() error { _, err := store.Dismiss(context.Background(), command); return err }},
		{name: "forget", run: func() error { _, err := store.Forget(context.Background(), command); return err }},
		{name: "erase", run: func() error { return store.Erase(context.Background(), command) }},
	}
	for _, check := range checks {
		if err := check.run(); !errors.Is(err, memorykit.ErrNotFound) {
			t.Fatalf("%s foreign-scope error = %v, want ErrNotFound", check.name, err)
		}
	}
	unchanged, err := store.Get(context.Background(), created.Scope, created.ID)
	if err != nil || !reflect.DeepEqual(unchanged, created) {
		t.Fatalf("memory changed after foreign-scope calls = %#v/%#v, %v", created, unchanged, err)
	}
}

func lifecycleCreateRequest(id string, status memorykit.Status, content string) memorykit.CreateRequest {
	now := time.Date(2026, 7, 21, 2, 0, 0, 0, time.UTC)
	return memorykit.CreateRequest{
		ID: id,
		Scope: memorykit.Scope{
			TenantID: conformanceTenantID, SubjectType: memorykit.SubjectProject, SubjectID: "project-lifecycle",
		},
		Kind: memorykit.KindDecision, Key: "build.test_command", Status: status, Content: content,
		ValidFrom: now, Importance: 80, Confidence: 1, SourceAgentID: "agent-1",
		Actor: "user-1", Reason: "integration", Sources: []memorykit.Source{
			{Kind: "artifact", Ref: "artifact:test-command", EvidenceHash: "sha256:test"},
		},
		Now: now,
	}
}

func openIsolatedStore(t *testing.T) *Store {
	t.Helper()
	store := openTestStore(t)
	cleanupConformanceTenant(t, store)
	t.Cleanup(func() { cleanupConformanceTenant(t, store) })
	return store
}

func cleanupConformanceTenant(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(),
		"DELETE FROM memories WHERE tenant_id = $1", conformanceTenantID); err != nil {
		t.Fatalf("cleanup conformance tenant: %v", err)
	}
}
