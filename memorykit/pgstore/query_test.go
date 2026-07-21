package pgstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
	pgvector "github.com/pgvector/pgvector-go"
)

const (
	queryIDCommand       = "61000000-0000-4000-8000-000000000001"
	queryIDSemantic      = "61000000-0000-4000-8000-000000000002"
	queryIDLexical       = "61000000-0000-4000-8000-000000000003"
	queryIDCandidate     = "61000000-0000-4000-8000-000000000004"
	queryIDInactive      = "61000000-0000-4000-8000-000000000005"
	queryIDExpired       = "61000000-0000-4000-8000-000000000006"
	queryIDFuture        = "61000000-0000-4000-8000-000000000007"
	queryIDOtherProject  = "61000000-0000-4000-8000-000000000008"
	queryIDOtherTenant   = "61000000-0000-4000-8000-000000000009"
	queryIDBrokenVector  = "61000000-0000-4000-8000-000000000010"
	queryIDExactLower    = "61000000-0000-4000-8000-000000000011"
	queryOtherTenantName = "tenant-query-other"
)

func TestSearchCandidatesSeparatesScopedEffectiveChannels(t *testing.T) {
	store := openIsolatedStore(t)
	t.Cleanup(func() {
		if _, err := store.db.ExecContext(context.Background(), "DELETE FROM memories WHERE tenant_id = $1", queryOtherTenantName); err != nil {
			t.Errorf("cleanup other query tenant: %v", err)
		}
	})
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	scope := memorykit.Scope{TenantID: conformanceTenantID, SubjectType: memorykit.SubjectProject, SubjectID: "project-query"}
	otherProject := scope
	otherProject.SubjectID = "project-query-other"
	otherTenant := scope
	otherTenant.TenantID = queryOtherTenantName

	command := queryCreateRequest(queryIDCommand, scope, memorykit.StatusActive, "build.test_command", "validation should run go test", now, 90)
	command.Kind = memorykit.KindDecision
	command.Sources = []memorykit.Source{
		{Kind: "artifact", Ref: "artifact:command", EvidenceHash: "hash-command"},
		{Kind: "event", Ref: "event:command", EvidenceHash: "hash-event"},
	}
	createQueryMemory(t, store, command)
	createQueryMemory(t, store, queryCreateRequest(queryIDExactLower, scope, memorykit.StatusActive, "build.verify_command", "use the narrow workflow", now, 40))
	createQueryMemory(t, store, queryCreateRequest(queryIDSemantic, scope, memorykit.StatusActive, "lesson.semantic", "prefer the verified narrow workflow", now, 70))
	createQueryMemory(t, store, queryCreateRequest(queryIDLexical, scope, memorykit.StatusActive, "lesson.lexical", "validation should run go test in a different workflow", now, 80))
	createQueryMemory(t, store, queryCreateRequest(queryIDCandidate, scope, memorykit.StatusCandidate, "build.test_command", "validation should run go test", now, 100))
	createQueryMemory(t, store, queryCreateRequest(queryIDInactive, scope, memorykit.StatusInactive, "build.test_command", "validation should run go test", now, 100))
	expired := queryCreateRequest(queryIDExpired, scope, memorykit.StatusActive, "expired.key", "validation should run go test", now, 100)
	expired.ValidUntil = now
	createQueryMemory(t, store, expired)
	future := queryCreateRequest(queryIDFuture, scope, memorykit.StatusActive, "future.key", "validation should run go test", now, 100)
	future.ValidFrom = now.Add(time.Minute)
	createQueryMemory(t, store, future)
	createQueryMemory(t, store, queryCreateRequest(queryIDOtherProject, otherProject, memorykit.StatusActive, "build.test_command", "validation should run go test", now, 100))
	createQueryMemory(t, store, queryCreateRequest(queryIDOtherTenant, otherTenant, memorykit.StatusActive, "build.test_command", "validation should run go test", now, 100))

	putQueryEmbedding(t, store, queryIDCommand, scope, contentDigest(command.Content), []float32{0, 1, 0}, now)
	putQueryEmbedding(t, store, queryIDSemantic, scope, contentDigest("prefer the verified narrow workflow"), []float32{1, 0, 0}, now)
	putQueryEmbedding(t, store, queryIDLexical, scope, contentDigest("validation should run go test in a different workflow"), []float32{0, 1, 0}, now)
	insertQueryEmbeddingFixture(t, store, queryIDCandidate, contentDigest("validation should run go test"), []float32{1, 0, 0}, now)
	putQueryEmbedding(t, store, queryIDOtherProject, otherProject, contentDigest("validation should run go test"), []float32{1, 0, 0}, now)
	putQueryEmbedding(t, store, queryIDOtherTenant, otherTenant, contentDigest("validation should run go test"), []float32{1, 0, 0}, now)

	got, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{
		Scope: scope, Text: "validation run go test", Keys: []string{"build.test_command", "build.verify_command"},
		Kinds: []memorykit.Kind{memorykit.KindDecision, memorykit.KindLesson}, QueryVector: []float32{1, 0, 0}, Now: now,
		ExactLimit: 4, FullTextLimit: 4, VectorLimit: 4, MinVectorSimilarity: 0.70,
		EmbeddingProfileID: "test-3d", EmbeddingDimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertQueryCandidateIDs(t, got.Exact, []string{queryIDCommand, queryIDExactLower})
	assertQueryCandidateIDs(t, got.FullText, []string{queryIDCommand, queryIDLexical})
	assertQueryCandidateIDs(t, got.Vector, []string{queryIDSemantic})
	assertQueryRanks(t, got.Exact, memorykit.ChannelExact)
	assertQueryRanks(t, got.FullText, memorykit.ChannelFullText)
	assertQueryRanks(t, got.Vector, memorykit.ChannelVector)
	if !reflect.DeepEqual(got.Exact[0].Sources, command.Sources) {
		t.Fatalf("exact sources = %#v, want %#v", got.Exact[0].Sources, command.Sources)
	}
	limited, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{
		Scope: scope, Keys: []string{"build.test_command", "build.verify_command"}, Now: now, ExactLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertQueryCandidateIDs(t, limited.Exact, []string{queryIDCommand})

	// Result slices and their nested Sources must not alias a later result.
	got.Exact[0].Sources[0].Ref = "mutated"
	if got.FullText[0].Sources[0].Ref != "artifact:command" {
		t.Fatalf("candidate channels shared source storage: %#v", got.FullText[0].Sources)
	}
	again, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{
		Scope: scope, Keys: []string{"build.test_command"}, Now: now, ExactLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Exact[0].Sources[0].Ref != "artifact:command" {
		t.Fatalf("stored source was mutated: %#v", again.Exact[0].Sources)
	}

	if _, err := store.db.ExecContext(context.Background(), "DELETE FROM memory_embeddings WHERE memory_id = ANY($1::uuid[])", []string{queryIDCommand, queryIDSemantic, queryIDLexical}); err != nil {
		t.Fatalf("delete embedding fixtures: %v", err)
	}
	withoutVectors, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{
		Scope: scope, Text: "validation run go test", Keys: []string{"build.test_command"}, QueryVector: []float32{1, 0, 0}, Now: now,
		ExactLimit: 4, FullTextLimit: 4, VectorLimit: 4, MinVectorSimilarity: 0.70,
		EmbeddingProfileID: "test-3d", EmbeddingDimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertQueryCandidateIDs(t, withoutVectors.Exact, []string{queryIDCommand})
	assertQueryCandidateIDs(t, withoutVectors.FullText, []string{queryIDCommand, queryIDLexical})
	assertQueryCandidateIDs(t, withoutVectors.Vector, []string{})
}

func TestSearchCandidatesRejectsConfigMismatchAndFailsClosedOnVectorError(t *testing.T) {
	store := openIsolatedStore(t)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	scope := memorykit.Scope{TenantID: conformanceTenantID, SubjectType: memorykit.SubjectProject, SubjectID: "project-query-errors"}
	request := queryCreateRequest(queryIDBrokenVector, scope, memorykit.StatusActive, "build.test_command", "validation run go test", now, 80)
	createQueryMemory(t, store, request)

	invalid := memorykit.CandidateQuery{
		Scope: scope, Text: "validation", QueryVector: []float32{1, 0, 0}, Now: now,
		FullTextLimit: 1, VectorLimit: 1, MinVectorSimilarity: 0.5,
		EmbeddingProfileID: "other-3d", EmbeddingDimensions: 3,
	}
	if got, err := store.SearchCandidates(context.Background(), invalid); err == nil || !reflect.DeepEqual(got, memorykit.CandidateSet{}) {
		t.Fatalf("config mismatch result/error = %#v/%v", got, err)
	}

	if _, err := store.db.ExecContext(context.Background(), `
		INSERT INTO memory_embeddings
			(memory_id, embedding_profile_id, content_hash, dimension, embedding, embedded_at)
		VALUES ($1, 'test-3d', $2, 3, '[1,0]'::vector, $3)
	`, request.ID, contentDigest(request.Content), now); err != nil {
		t.Fatalf("insert broken vector fixture: %v", err)
	}
	invalid.EmbeddingProfileID = "test-3d"
	got, err := store.SearchCandidates(context.Background(), invalid)
	if err == nil || memorykit.IsRecoverable(err) || !reflect.DeepEqual(got, memorykit.CandidateSet{}) {
		t.Fatalf("nonrecoverable vector result/error = %#v/%v", got, err)
	}
}

func queryCreateRequest(id string, scope memorykit.Scope, status memorykit.Status, key, content string, now time.Time, importance int) memorykit.CreateRequest {
	return memorykit.CreateRequest{
		ID: id, Scope: scope, Kind: memorykit.KindLesson, Key: key, Status: status, Content: content,
		ValidFrom: now.Add(-time.Hour), Importance: importance, Confidence: 1,
		Actor: "query-test", Reason: "query fixture", Now: now,
		Sources: []memorykit.Source{{Kind: "artifact", Ref: "artifact:" + id, EvidenceHash: "hash-" + id}},
	}
}

func createQueryMemory(t *testing.T, store *Store, request memorykit.CreateRequest) {
	t.Helper()
	if _, err := store.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

func putQueryEmbedding(t *testing.T, store *Store, id string, scope memorykit.Scope, hash string, vector []float32, now time.Time) {
	t.Helper()
	if err := store.PutEmbedding(context.Background(), memorykit.PutEmbeddingRequest{
		MemoryID: id, Scope: scope, ProfileID: "test-3d", ContentHash: hash,
		Dimensions: 3, Vector: vector, EmbeddedAt: now,
	}); err != nil {
		t.Fatalf("PutEmbedding(%s): %v", id, err)
	}
}

func insertQueryEmbeddingFixture(t *testing.T, store *Store, id, hash string, vector []float32, now time.Time) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `
		INSERT INTO memory_embeddings
			(memory_id, embedding_profile_id, content_hash, dimension, embedding, embedded_at)
		VALUES ($1, 'test-3d', $2, 3, $3, $4)
	`, id, hash, pgvector.NewVector(append([]float32(nil), vector...)), now); err != nil {
		t.Fatalf("insert embedding fixture %s: %v", id, err)
	}
}

func assertQueryCandidateIDs(t *testing.T, candidates []memorykit.Candidate, want []string) {
	t.Helper()
	got := make([]string, len(candidates))
	for index := range candidates {
		got[index] = candidates[index].Memory.ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate IDs = %v, want %v", got, want)
	}
}

func assertQueryRanks(t *testing.T, candidates []memorykit.Candidate, channel memorykit.Channel) {
	t.Helper()
	for index := range candidates {
		if candidates[index].Channel != channel || candidates[index].Rank != index+1 {
			t.Fatalf("candidate %d channel/rank = %s/%d", index, candidates[index].Channel, candidates[index].Rank)
		}
	}
}

func TestVectorChannelErrorClassification(t *testing.T) {
	partial := memorykit.CandidateSet{Exact: []memorykit.Candidate{{Rank: 1}}}
	recoverable := &memorykit.BackendError{Op: "vector candidates", Recoverable: true, Err: errors.New("safe")}
	got, err := vectorChannelResult(partial, recoverable)
	var channelErr *memorykit.ChannelError
	if !errors.As(err, &channelErr) || channelErr.Channel != memorykit.ChannelVector || !memorykit.IsRecoverable(err) || !reflect.DeepEqual(got, partial) {
		t.Fatalf("recoverable vector classification = %#v/%v", got, err)
	}
	got, err = vectorChannelResult(partial, errors.New("fatal"))
	if err == nil || !reflect.DeepEqual(got, memorykit.CandidateSet{}) {
		t.Fatalf("fatal vector classification = %#v/%v", got, err)
	}
}
