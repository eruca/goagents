package pgstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
	pgvector "github.com/pgvector/pgvector-go"
)

const (
	embeddingIDActive    = "62000000-0000-4000-8000-000000000001"
	embeddingIDCandidate = "62000000-0000-4000-8000-000000000002"
	embeddingIDInactive  = "62000000-0000-4000-8000-000000000003"
)

func TestEmbeddingPendingPutCorrectAndErase(t *testing.T) {
	store := openIsolatedStore(t)
	now := time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)
	scope := memorykit.Scope{TenantID: conformanceTenantID, SubjectType: memorykit.SubjectProject, SubjectID: "project-embedding"}
	active := queryCreateRequest(embeddingIDActive, scope, memorykit.StatusActive, "embedding.active", "active content", now, 70)
	createQueryMemory(t, store, active)
	createQueryMemory(t, store, queryCreateRequest(embeddingIDCandidate, scope, memorykit.StatusCandidate, "embedding.candidate", "candidate content", now, 70))
	createQueryMemory(t, store, queryCreateRequest(embeddingIDInactive, scope, memorykit.StatusInactive, "embedding.inactive", "inactive content", now, 70))

	pending, err := store.PendingEmbeddings(context.Background(), memorykit.PendingEmbeddingQuery{ProfileID: "test-3d", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].MemoryID != active.ID || pending[0].Scope != scope ||
		pending[0].Content != active.Content || pending[0].ContentHash != contentDigest(active.Content) || pending[0].Version != 1 {
		t.Fatalf("pending embeddings = %#v", pending)
	}

	put := memorykit.PutEmbeddingRequest{
		MemoryID: active.ID, Scope: scope, ProfileID: "test-3d", ContentHash: contentDigest(active.Content),
		Dimensions: 3, Vector: []float32{1, 0, 0}, EmbeddedAt: now,
	}
	if err := store.PutEmbedding(context.Background(), put); err != nil {
		t.Fatal(err)
	}
	pending, err = store.PendingEmbeddings(context.Background(), memorykit.PendingEmbeddingQuery{ProfileID: "test-3d", Limit: 10})
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after Put = %#v, %v", pending, err)
	}

	corrected, err := store.Correct(context.Background(), memorykit.CorrectRequest{
		Command: memorykit.VersionedCommand{Scope: scope, ID: active.ID, ExpectedVersion: 1, Actor: "embedding-test", Reason: "new content", Now: now.Add(time.Minute)},
		Content: "corrected content", ValidFrom: active.ValidFrom, Importance: active.Importance, Confidence: active.Confidence,
		Sources: active.Sources,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutEmbedding(context.Background(), put); !errors.Is(err, memorykit.ErrConflict) {
		t.Fatalf("stale PutEmbedding error = %v, want ErrConflict", err)
	}
	staleRecall, err := store.SearchCandidates(context.Background(), memorykit.CandidateQuery{
		Scope: scope, Text: "corrected", QueryVector: []float32{1, 0, 0}, Now: now.Add(time.Minute),
		VectorLimit: 1, MinVectorSimilarity: 0.5, EmbeddingProfileID: "test-3d", EmbeddingDimensions: 3,
	})
	if err != nil || len(staleRecall.Vector) != 0 {
		t.Fatalf("stale embedding remained recallable: %#v, %v", staleRecall.Vector, err)
	}
	pending, err = store.PendingEmbeddings(context.Background(), memorykit.PendingEmbeddingQuery{ProfileID: "test-3d", Limit: 10})
	if err != nil || len(pending) != 1 || pending[0].ContentHash != contentDigest(corrected.Content) || pending[0].Version != corrected.Version {
		t.Fatalf("pending after Correct = %#v, %v", pending, err)
	}
	put.ContentHash = contentDigest(corrected.Content)
	put.EmbeddedAt = now.Add(2 * time.Minute)
	if err := store.PutEmbedding(context.Background(), put); err != nil {
		t.Fatal(err)
	}

	foreign := put
	foreign.Scope.SubjectID = "project-foreign"
	if err := store.PutEmbedding(context.Background(), foreign); !errors.Is(err, memorykit.ErrConflict) {
		t.Fatalf("foreign-scope PutEmbedding error = %v, want ErrConflict", err)
	}
	if err := store.Erase(context.Background(), memorykit.VersionedCommand{
		Scope: scope, ID: active.ID, ExpectedVersion: corrected.Version,
		Actor: "embedding-test", Reason: "erase", Now: now.Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(context.Background(), "SELECT count(*) FROM memory_embeddings WHERE memory_id = $1", active.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("embedding count after erase = %d, %v", count, err)
	}
	pending, err = store.PendingEmbeddings(context.Background(), memorykit.PendingEmbeddingQuery{ProfileID: "test-3d", Limit: 10})
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after erase = %#v, %v", pending, err)
	}
}

func TestEmbeddingValidationAndDefensiveCopies(t *testing.T) {
	store := openIsolatedStore(t)
	now := time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
	scope := memorykit.Scope{TenantID: conformanceTenantID, SubjectType: memorykit.SubjectProject, SubjectID: "project-embedding-validation"}
	request := queryCreateRequest(embeddingIDActive, scope, memorykit.StatusActive, "embedding.active", "active content", now, 70)
	createQueryMemory(t, store, request)

	for name, query := range map[string]memorykit.PendingEmbeddingQuery{
		"blank profile": {Limit: 1},
		"wrong profile": {ProfileID: "other-3d", Limit: 1},
		"zero limit":    {ProfileID: "test-3d"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.PendingEmbeddings(context.Background(), query); err == nil {
				t.Fatalf("PendingEmbeddings(%#v) succeeded", query)
			}
		})
	}

	valid := memorykit.PutEmbeddingRequest{
		MemoryID: request.ID, Scope: scope, ProfileID: "test-3d", ContentHash: contentDigest(request.Content),
		Dimensions: 3, Vector: []float32{1, 0, 0}, EmbeddedAt: now,
	}
	checks := map[string]func(*memorykit.PutEmbeddingRequest){
		"noncanonical id": func(value *memorykit.PutEmbeddingRequest) { value.MemoryID = "not-a-uuid" },
		"invalid scope":   func(value *memorykit.PutEmbeddingRequest) { value.Scope.SubjectID = "" },
		"wrong profile":   func(value *memorykit.PutEmbeddingRequest) { value.ProfileID = "other-3d" },
		"wrong dimensions": func(value *memorykit.PutEmbeddingRequest) {
			value.Dimensions = 2
		},
		"wrong vector length": func(value *memorykit.PutEmbeddingRequest) { value.Vector = []float32{1, 0} },
		"nonfinite vector":    func(value *memorykit.PutEmbeddingRequest) { value.Vector[0] = float32(math.NaN()) },
	}
	for name, mutate := range checks {
		t.Run(name, func(t *testing.T) {
			request := valid
			request.Vector = append([]float32(nil), valid.Vector...)
			mutate(&request)
			if err := store.PutEmbedding(context.Background(), request); err == nil {
				t.Fatalf("PutEmbedding(%#v) succeeded", request)
			}
		})
	}

	vector := append([]float32(nil), valid.Vector...)
	valid.Vector = vector
	if err := store.PutEmbedding(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	vector[0] = 0
	var stored []float32
	if err := store.db.QueryRowContext(context.Background(), "SELECT embedding FROM memory_embeddings WHERE memory_id = $1", request.ID).Scan((*vectorScanner)(&stored)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, []float32{1, 0, 0}) {
		t.Fatalf("stored embedding aliased caller vector: %v", stored)
	}
}

func contentDigest(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

// vectorScanner keeps the assertion independent of pgvector's exported representation.
type vectorScanner []float32

func (vector *vectorScanner) Scan(src any) error {
	var decoded pgvector.Vector
	if err := decoded.Scan(src); err != nil {
		return err
	}
	*vector = append((*vector)[:0], decoded.Slice()...)
	return nil
}
