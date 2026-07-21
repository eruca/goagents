package memorystore

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/storetest"
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

	createTestMemory(t, store, testMemoryRequest("m-exact", scope, memorykit.KindDecision, "build.test_command", "Run the verified command", memorykit.StatusActive, now, 80))
	createTestMemory(t, store, testMemoryRequest("m-fts", scope, memorykit.KindDecision, "testing.pg", "Verify VECTOR STORAGE against the real database", memorykit.StatusActive, now, 70))
	createTestMemory(t, store, testMemoryRequest("m-vector", scope, memorykit.KindDecision, "semantic.database", "Check semantic persistence", memorykit.StatusActive, now, 60))
	createTestMemory(t, store, testMemoryRequest("m-candidate", scope, memorykit.KindDecision, "build.test_command", "VECTOR STORAGE candidate", memorykit.StatusCandidate, now, 100))
	createTestMemory(t, store, testMemoryRequest("m-inactive", scope, memorykit.KindDecision, "inactive.key", "VECTOR STORAGE inactive", memorykit.StatusInactive, now, 100))
	future := testMemoryRequest("m-future", scope, memorykit.KindDecision, "future.key", "VECTOR STORAGE future", memorykit.StatusActive, now, 100)
	future.ValidFrom = now.Add(time.Minute)
	createTestMemory(t, store, future)
	expired := testMemoryRequest("m-expired", scope, memorykit.KindDecision, "expired.key", "VECTOR STORAGE expired", memorykit.StatusActive, now, 100)
	expired.ValidUntil = now
	createTestMemory(t, store, expired)
	createTestMemory(t, store, testMemoryRequest("m-other-scope", otherScope, memorykit.KindDecision, "build.test_command", "VECTOR STORAGE other", memorykit.StatusActive, now, 100))
	createTestMemory(t, store, testMemoryRequest("m-other-kind", scope, memorykit.KindFact, "other.kind", "VECTOR STORAGE fact", memorykit.StatusActive, now, 100))

	putTestEmbedding(t, store, "m-vector", "test-3d", []float32{1, 0, 0})
	putTestEmbedding(t, store, "m-fts", "test-3d", []float32{0, 1, 0})
	putTestEmbedding(t, store, "m-candidate", "test-3d", []float32{1, 0, 0})

	got, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{
		Scope: scope, Text: "vector storage", Keys: []string{"build.test_command"},
		Kinds: []memorykit.Kind{memorykit.KindDecision}, QueryVector: []float32{1, 0, 0}, Now: now,
		ExactLimit: 4, FullTextLimit: 4, VectorLimit: 4, MinVectorSimilarity: 0.7,
		EmbeddingProfileID: "test-3d", EmbeddingDimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateIDs(t, got.Exact, []string{"m-exact"})
	assertCandidateIDs(t, got.FullText, []string{"m-fts"})
	assertCandidateIDs(t, got.Vector, []string{"m-vector"})
	if got.Exact[0].Rank != 1 || got.Exact[0].Channel != memorykit.ChannelExact ||
		got.FullText[0].Rank != 1 || got.FullText[0].Channel != memorykit.ChannelFullText ||
		got.Vector[0].Rank != 1 || got.Vector[0].Channel != memorykit.ChannelVector || got.Vector[0].Similarity != 1 {
		t.Fatalf("unexpected candidates: %#v", got)
	}
	if !reflect.DeepEqual(got.Exact[0].Sources, []memorykit.Source{{Kind: "event", Ref: "ref-m-exact", EvidenceHash: "hash"}}) {
		t.Fatalf("sources = %#v", got.Exact[0].Sources)
	}
}

func TestSearchCandidatesAppliesDeterministicLimitsAndReturnsCopies(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	scope := memorykit.Scope{TenantID: "tenant-1", SubjectType: memorykit.SubjectProject, SubjectID: "project-1"}
	createTestMemory(t, store, testMemoryRequest("lower", scope, memorykit.KindDecision, "same.key", "shared phrase lower", memorykit.StatusActive, now, 10))
	createTestMemory(t, store, testMemoryRequest("higher", scope, memorykit.KindLesson, "same.key", "shared phrase higher", memorykit.StatusActive, now, 90))
	putTestEmbedding(t, store, "lower", "test-3d", []float32{0.8, 0.6, 0})
	putTestEmbedding(t, store, "higher", "test-3d", []float32{1, 0, 0})
	queryVector := []float32{1, 0, 0}

	got, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{
		Scope: scope, Text: "shared phrase", Keys: []string{"same.key"}, QueryVector: queryVector, Now: now,
		ExactLimit: 1, FullTextLimit: 1, VectorLimit: 1, MinVectorSimilarity: 0.7,
		EmbeddingProfileID: "test-3d", EmbeddingDimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateIDs(t, got.Exact, []string{"higher"})
	assertCandidateIDs(t, got.FullText, []string{"higher"})
	assertCandidateIDs(t, got.Vector, []string{"higher"})
	got.Exact[0].Sources[0].Ref = "mutated"
	queryVector[0] = 0
	sources, err := store.Sources(context.Background(), scope, "higher")
	if err != nil {
		t.Fatal(err)
	}
	if sources[0].Ref != "ref-higher" || store.embeddings["higher"].vector[0] != 1 {
		t.Fatalf("store state mutated: sources=%#v embedding=%#v", sources, store.embeddings["higher"])
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
		"erase-me", scope, memorykit.KindDecision, "vector.storage", "verify vector storage",
		memorykit.StatusActive, now, 80,
	))
	putTestEmbedding(t, store, "erase-me", "test-3d", []float32{1, 0, 0})

	if err := store.Erase(context.Background(), memorykit.VersionedCommand{
		Scope: scope, ID: "erase-me", ExpectedVersion: 1,
		Actor: "test", Reason: "erase fixture", Now: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	store.mu.RLock()
	_, embeddingExists := store.embeddings["erase-me"]
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
	store.embeddings[id] = storedEmbedding{
		profileID: profileID, dimensions: len(vector), vector: append([]float32(nil), vector...),
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
