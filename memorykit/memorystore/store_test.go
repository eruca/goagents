package memorystore

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/storetest"
)

const (
	memoryIDExact      = "10000000-0000-4000-8000-000000000001"
	memoryIDFullText   = "10000000-0000-4000-8000-000000000002"
	memoryIDVector     = "10000000-0000-4000-8000-000000000003"
	memoryIDCandidate  = "10000000-0000-4000-8000-000000000004"
	memoryIDInactive   = "10000000-0000-4000-8000-000000000005"
	memoryIDFuture     = "10000000-0000-4000-8000-000000000006"
	memoryIDExpired    = "10000000-0000-4000-8000-000000000007"
	memoryIDOtherScope = "10000000-0000-4000-8000-000000000008"
	memoryIDOtherKind  = "10000000-0000-4000-8000-000000000009"
	memoryIDLower      = "10000000-0000-4000-8000-000000000010"
	memoryIDHigher     = "10000000-0000-4000-8000-000000000011"
	memoryIDErase      = "10000000-0000-4000-8000-000000000012"
)

func TestNewRejectsInvalidLimits(t *testing.T) {
	if _, err := New(memorykit.Limits{}); err == nil {
		t.Fatal("New accepted empty limits")
	}
}

func TestLifecycleConformance(t *testing.T) {
	storetest.RunLifecycleConformance(t, func(t *testing.T) memorykit.LifecycleStore {
		t.Helper()
		store, err := New(memorykit.Limits{
			Version:     "project-memory-v1",
			MaxKeyRunes: 128, MaxContentRunes: 4096, MaxMetadataRunes: 512,
			MaxSourcesPerMemory: 8, MaxListItems: 100,
		})
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}

func TestSearchCandidatesFindsExactFullTextAndPrivateVectorFixtures(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	scope := memorykit.Scope{TenantID: "tenant-1", SubjectType: memorykit.SubjectProject, SubjectID: "project-1"}
	otherScope := memorykit.Scope{TenantID: "tenant-1", SubjectType: memorykit.SubjectProject, SubjectID: "project-2"}

	createTestMemory(t, store, testMemoryRequest(memoryIDExact, scope, memorykit.KindDecision, "build.test_command", "Run the verified command", memorykit.StatusActive, now, 80))
	createTestMemory(t, store, testMemoryRequest(memoryIDFullText, scope, memorykit.KindDecision, "testing.pg", "Verify VECTOR STORAGE against the real database", memorykit.StatusActive, now, 70))
	createTestMemory(t, store, testMemoryRequest(memoryIDVector, scope, memorykit.KindDecision, "semantic.database", "Check semantic persistence", memorykit.StatusActive, now, 60))
	createTestMemory(t, store, testMemoryRequest(memoryIDCandidate, scope, memorykit.KindDecision, "build.test_command", "VECTOR STORAGE candidate", memorykit.StatusCandidate, now, 100))
	createTestMemory(t, store, testMemoryRequest(memoryIDInactive, scope, memorykit.KindDecision, "inactive.key", "VECTOR STORAGE inactive", memorykit.StatusInactive, now, 100))
	future := testMemoryRequest(memoryIDFuture, scope, memorykit.KindDecision, "future.key", "VECTOR STORAGE future", memorykit.StatusActive, now, 100)
	future.ValidFrom = now.Add(time.Minute)
	createTestMemory(t, store, future)
	expired := testMemoryRequest(memoryIDExpired, scope, memorykit.KindDecision, "expired.key", "VECTOR STORAGE expired", memorykit.StatusActive, now, 100)
	expired.ValidUntil = now
	createTestMemory(t, store, expired)
	createTestMemory(t, store, testMemoryRequest(memoryIDOtherScope, otherScope, memorykit.KindDecision, "build.test_command", "VECTOR STORAGE other", memorykit.StatusActive, now, 100))
	createTestMemory(t, store, testMemoryRequest(memoryIDOtherKind, scope, memorykit.KindFact, "other.kind", "VECTOR STORAGE fact", memorykit.StatusActive, now, 100))

	putTestEmbedding(t, store, memoryIDVector, "test-3d", []float32{1, 0, 0})
	putTestEmbedding(t, store, memoryIDFullText, "test-3d", []float32{0, 1, 0})
	putTestEmbedding(t, store, memoryIDCandidate, "test-3d", []float32{1, 0, 0})

	got, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{
		Scope: scope, Text: "vector storage", Keys: []string{"build.test_command"},
		Kinds: []memorykit.Kind{memorykit.KindDecision}, QueryVector: []float32{1, 0, 0}, Now: now,
		ExactLimit: 4, FullTextLimit: 4, VectorLimit: 4, MinVectorSimilarity: 0.7,
		EmbeddingProfileID: "test-3d", EmbeddingDimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateIDs(t, got.Exact, []string{memoryIDExact})
	assertCandidateIDs(t, got.FullText, []string{memoryIDFullText})
	assertCandidateIDs(t, got.Vector, []string{memoryIDVector})
	if got.Exact[0].Rank != 1 || got.Exact[0].Channel != memorykit.ChannelExact ||
		got.FullText[0].Rank != 1 || got.FullText[0].Channel != memorykit.ChannelFullText ||
		got.Vector[0].Rank != 1 || got.Vector[0].Channel != memorykit.ChannelVector || got.Vector[0].Similarity != 1 {
		t.Fatalf("unexpected candidates: %#v", got)
	}
	if !reflect.DeepEqual(got.Exact[0].Sources, []memorykit.Source{{Kind: "event", Ref: "ref-" + memoryIDExact, EvidenceHash: "hash"}}) {
		t.Fatalf("sources = %#v", got.Exact[0].Sources)
	}
}

func TestSearchCandidatesAppliesDeterministicLimitsAndReturnsCopies(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	scope := memorykit.Scope{TenantID: "tenant-1", SubjectType: memorykit.SubjectProject, SubjectID: "project-1"}
	createTestMemory(t, store, testMemoryRequest(memoryIDLower, scope, memorykit.KindDecision, "same.key", "shared phrase lower", memorykit.StatusActive, now, 10))
	createTestMemory(t, store, testMemoryRequest(memoryIDHigher, scope, memorykit.KindLesson, "same.key", "shared phrase higher", memorykit.StatusActive, now, 90))
	putTestEmbedding(t, store, memoryIDLower, "test-3d", []float32{0.8, 0.6, 0})
	putTestEmbedding(t, store, memoryIDHigher, "test-3d", []float32{1, 0, 0})
	queryVector := []float32{1, 0, 0}

	got, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{
		Scope: scope, Text: "shared phrase", Keys: []string{"same.key"}, QueryVector: queryVector, Now: now,
		ExactLimit: 1, FullTextLimit: 1, VectorLimit: 1, MinVectorSimilarity: 0.7,
		EmbeddingProfileID: "test-3d", EmbeddingDimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateIDs(t, got.Exact, []string{memoryIDHigher})
	assertCandidateIDs(t, got.FullText, []string{memoryIDHigher})
	assertCandidateIDs(t, got.Vector, []string{memoryIDHigher})
	got.Exact[0].Sources[0].Ref = "mutated"
	queryVector[0] = 0
	sources, err := store.Sources(context.Background(), scope, memoryIDHigher)
	if err != nil {
		t.Fatal(err)
	}
	embedding := store.embeddings[embeddingIdentity{memoryID: memoryIDHigher, profileID: "test-3d"}]
	if sources[0].Ref != "ref-"+memoryIDHigher || embedding.vector[0] != 1 {
		t.Fatalf("store state mutated: sources=%#v embedding=%#v", sources, embedding)
	}
}

func TestSearchCandidatesRejectsInvalidQueryAndCancelledContext(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{}); err == nil {
		t.Fatal("SearchCandidates accepted invalid query")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.SearchCandidates(ctx, memorykit.CandidateQuery{
		Scope: memorykit.Scope{TenantID: "tenant-1", SubjectType: memorykit.SubjectProject, SubjectID: "project-1"},
		Keys:  []string{"key"}, ExactLimit: 1,
	})
	if err == nil {
		t.Fatal("SearchCandidates accepted cancelled context")
	}
}

func TestEraseRemovesPrivateEmbeddingAndAllRecallCandidates(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	scope := memorykit.Scope{TenantID: "tenant-1", SubjectType: memorykit.SubjectProject, SubjectID: "project-1"}
	createTestMemory(t, store, testMemoryRequest(
		memoryIDErase, scope, memorykit.KindDecision, "vector.storage", "verify vector storage",
		memorykit.StatusActive, now, 80,
	))
	putTestEmbedding(t, store, memoryIDErase, "test-3d", []float32{1, 0, 0})

	if err := store.Erase(context.Background(), memorykit.VersionedCommand{
		Scope: scope, ID: memoryIDErase, ExpectedVersion: 1,
		Actor: "test", Reason: "erase fixture", Now: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	store.mu.RLock()
	_, embeddingExists := store.embeddings[embeddingIdentity{memoryID: memoryIDErase, profileID: "test-3d"}]
	store.mu.RUnlock()
	if embeddingExists {
		t.Fatal("Erase retained private embedding")
	}

	got, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{
		Scope: scope, Text: "vector storage", Keys: []string{"vector.storage"},
		QueryVector: []float32{1, 0, 0}, Now: now.Add(2 * time.Minute),
		ExactLimit: 4, FullTextLimit: 4, VectorLimit: 4, MinVectorSimilarity: 0.7,
		EmbeddingProfileID: "test-3d", EmbeddingDimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Exact) != 0 || len(got.FullText) != 0 || len(got.Vector) != 0 {
		t.Fatalf("erased memory remained recallable: %#v", got)
	}
}

func TestEmbeddingStoreUsesExplicitRequestProfiles(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	scope := memorykit.Scope{TenantID: "tenant-1", SubjectType: memorykit.SubjectProject, SubjectID: "project-embedding"}
	request := testMemoryRequest(memoryIDExact, scope, memorykit.KindDecision, "embedding.key", "embedding content", memorykit.StatusActive, now, 80)
	createTestMemory(t, store, request)
	hash := hashMemoryContent(request.Content)

	first, err := store.PendingEmbeddings(context.Background(), memorykit.PendingEmbeddingQuery{ProfileID: "profile-a", Limit: 10})
	if err != nil || len(first) != 1 || first[0].ContentHash != hash {
		t.Fatalf("profile-a pending = %#v, %v", first, err)
	}
	vector := []float32{1, 0}
	if err := store.PutEmbedding(context.Background(), memorykit.PutEmbeddingRequest{
		MemoryID: request.ID, Scope: scope, ProfileID: "profile-a", ContentHash: hash,
		Dimensions: 2, Vector: vector, EmbeddedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	vector[0] = 0
	first, err = store.PendingEmbeddings(context.Background(), memorykit.PendingEmbeddingQuery{ProfileID: "profile-a", Limit: 10})
	if err != nil || len(first) != 0 {
		t.Fatalf("profile-a pending after Put = %#v, %v", first, err)
	}
	second, err := store.PendingEmbeddings(context.Background(), memorykit.PendingEmbeddingQuery{ProfileID: "profile-b", Limit: 10})
	if err != nil || len(second) != 1 {
		t.Fatalf("profile-b pending = %#v, %v", second, err)
	}
	stored := store.embeddings[embeddingIdentity{memoryID: request.ID, profileID: "profile-a"}]
	if !reflect.DeepEqual(stored.vector, []float32{1, 0}) {
		t.Fatalf("stored vector aliased caller: %v", stored.vector)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(memorykit.Limits{
		Version: "project-memory-v1", MaxKeyRunes: 128, MaxContentRunes: 4096,
		MaxMetadataRunes: 512, MaxSourcesPerMemory: 8, MaxListItems: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testMemoryRequest(id string, scope memorykit.Scope, kind memorykit.Kind, key, content string, status memorykit.Status, now time.Time, importance int) memorykit.CreateRequest {
	return memorykit.CreateRequest{
		ID: id, Scope: scope, Kind: kind, Key: key, Content: content, Status: status,
		ValidFrom: now.Add(-time.Hour), Importance: importance, Confidence: 1,
		Actor: "test", Reason: "fixture", Sources: []memorykit.Source{{Kind: "event", Ref: "ref-" + id, EvidenceHash: "hash"}}, Now: now,
	}
}

func createTestMemory(t *testing.T, store *Store, request memorykit.CreateRequest) {
	t.Helper()
	if _, err := store.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

// putTestEmbedding is deliberately test-only and writes private reference-store state.
func putTestEmbedding(t *testing.T, store *Store, id, profileID string, vector []float32) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.embeddings[embeddingIdentity{memoryID: id, profileID: profileID}] = storedEmbedding{
		contentHash: hashMemoryContent(store.memories[id].Content),
		dimensions:  len(vector), vector: append([]float32(nil), vector...),
	}
}

func assertCandidateIDs(t *testing.T, candidates []memorykit.Candidate, want []string) {
	t.Helper()
	got := make([]string, len(candidates))
	for index := range candidates {
		got[index] = candidates[index].Memory.ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate IDs = %v, want %v", got, want)
	}
}
