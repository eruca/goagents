package memorykit

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

const (
	embeddingWorkerMemoryOne   = "11111111-1111-4111-8111-111111111111"
	embeddingWorkerMemoryTwo   = "22222222-2222-4222-8222-222222222222"
	embeddingWorkerMemoryThree = "33333333-3333-4333-8333-333333333333"
)

var embeddingWorkerScope = Scope{
	TenantID:    "tenant-a",
	SubjectType: SubjectProject,
	SubjectID:   "project-a",
}

type embeddingWorkerStore struct {
	pendingInputs []EmbeddingInput
	pendingErr    error
	putErrors     []error
	pendingCalls  []PendingEmbeddingQuery
	putRequests   []PutEmbeddingRequest
}

func (s *embeddingWorkerStore) PendingEmbeddings(ctx context.Context, query PendingEmbeddingQuery) ([]EmbeddingInput, error) {
	s.pendingCalls = append(s.pendingCalls, query)
	if s.pendingErr != nil {
		return nil, s.pendingErr
	}
	return append([]EmbeddingInput(nil), s.pendingInputs...), nil
}

func (s *embeddingWorkerStore) PutEmbedding(ctx context.Context, request PutEmbeddingRequest) error {
	request.Vector = append([]float32(nil), request.Vector...)
	s.putRequests = append(s.putRequests, request)
	index := len(s.putRequests) - 1
	if index < len(s.putErrors) {
		return s.putErrors[index]
	}
	return nil
}

type embeddingWorkerEmbedder struct {
	responses [][][]float32
	errors    []error
	requests  []EmbedRequest
	onEmbed   func()
}

func (e *embeddingWorkerEmbedder) Embed(ctx context.Context, request EmbedRequest) ([][]float32, error) {
	request.Texts = append([]string(nil), request.Texts...)
	e.requests = append(e.requests, request)
	if e.onEmbed != nil {
		e.onEmbed()
	}
	index := len(e.requests) - 1
	var response [][]float32
	if index < len(e.responses) {
		response = e.responses[index]
	}
	if index < len(e.errors) {
		return response, e.errors[index]
	}
	return response, nil
}

func TestEmbeddingWorkerRunsOneStableBatch(t *testing.T) {
	store := &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()}
	embedder := &embeddingWorkerEmbedder{responses: [][][]float32{{
		{1, 0, 0},
		{0, 1, 0},
	}}}
	worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
		Store: store, Embedder: embedder, ProfileID: "test-3d", Dimensions: 3, BatchSize: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	count, err := worker.RunOnce(context.Background())
	if err != nil || count != 2 {
		t.Fatalf("RunOnce = %d, %v", count, err)
	}
	if !reflect.DeepEqual(store.pendingCalls, []PendingEmbeddingQuery{{ProfileID: "test-3d", Limit: 8}}) {
		t.Fatalf("pending calls = %#v", store.pendingCalls)
	}
	if !reflect.DeepEqual(embedder.requests, []EmbedRequest{{ProfileID: "test-3d", Texts: []string{"first", "second"}}}) {
		t.Fatalf("embed requests = %#v", embedder.requests)
	}
	if len(store.putRequests) != 2 {
		t.Fatalf("put requests = %d", len(store.putRequests))
	}
	for index, request := range store.putRequests {
		input := store.pendingInputs[index]
		if request.MemoryID != input.MemoryID || request.Scope != input.Scope || request.ContentHash != input.ContentHash ||
			request.ProfileID != "test-3d" || request.Dimensions != 3 {
			t.Fatalf("put request %d = %#v", index, request)
		}
		if request.EmbeddedAt.IsZero() {
			t.Fatalf("put request %d has zero EmbeddedAt", index)
		}
	}
	if !store.putRequests[0].EmbeddedAt.Equal(store.putRequests[1].EmbeddedAt) {
		t.Fatalf("EmbeddedAt values differ: %v != %v", store.putRequests[0].EmbeddedAt, store.putRequests[1].EmbeddedAt)
	}
	if !reflect.DeepEqual(store.putRequests[0].Vector, []float32{1, 0, 0}) ||
		!reflect.DeepEqual(store.putRequests[1].Vector, []float32{0, 1, 0}) {
		t.Fatalf("put vectors = %#v", store.putRequests)
	}
}

func TestEmbeddingWorkerReturnsWithoutEmbeddingWhenNoWorkIsPending(t *testing.T) {
	store := &embeddingWorkerStore{}
	embedder := &embeddingWorkerEmbedder{}
	worker := mustEmbeddingWorker(t, store, embedder)

	count, err := worker.RunOnce(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("RunOnce = %d, %v", count, err)
	}
	if len(store.pendingCalls) != 1 || len(embedder.requests) != 0 || len(store.putRequests) != 0 {
		t.Fatalf("calls = pending:%d embed:%d put:%d", len(store.pendingCalls), len(embedder.requests), len(store.putRequests))
	}
}

func TestEmbeddingWorkerRejectsStoreBatchAboveConfiguredLimit(t *testing.T) {
	inputs := append(embeddingWorkerInputs(), EmbeddingInput{
		MemoryID: embeddingWorkerMemoryThree, Scope: embeddingWorkerScope,
		Content: "third", ContentHash: "hash-third", Version: 1,
	})
	store := &embeddingWorkerStore{pendingInputs: inputs}
	embedder := &embeddingWorkerEmbedder{}
	worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
		Store: store, Embedder: embedder, ProfileID: "test-3d", Dimensions: 3, BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	count, err := worker.RunOnce(context.Background())
	if count != 0 || !errors.Is(err, ErrInvalidMemory) {
		t.Fatalf("RunOnce = %d, %v", count, err)
	}
	if len(embedder.requests) != 0 || len(store.putRequests) != 0 {
		t.Fatalf("calls = embed:%d put:%d", len(embedder.requests), len(store.putRequests))
	}
}

func TestEmbeddingWorkerValidatesWholeBatchBeforeWriting(t *testing.T) {
	tests := []struct {
		name     string
		response [][]float32
	}{
		{name: "count mismatch", response: [][]float32{{1, 0, 0}}},
		{name: "dimension mismatch in later item", response: [][]float32{{1, 0, 0}, {0, 1}}},
		{name: "NaN in later item", response: [][]float32{{1, 0, 0}, {0, float32(math.NaN()), 0}}},
		{name: "positive infinity in later item", response: [][]float32{{1, 0, 0}, {0, float32(math.Inf(1)), 0}}},
		{name: "negative infinity in later item", response: [][]float32{{1, 0, 0}, {0, float32(math.Inf(-1)), 0}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()}
			embedder := &embeddingWorkerEmbedder{responses: [][][]float32{test.response}}
			worker := mustEmbeddingWorker(t, store, embedder)

			count, err := worker.RunOnce(context.Background())
			if err == nil || count != 0 {
				t.Fatalf("RunOnce = %d, %v", count, err)
			}
			if !errors.Is(err, ErrInvalidMemory) {
				t.Fatalf("error = %v, want ErrInvalidMemory", err)
			}
			if len(store.putRequests) != 0 {
				t.Fatalf("PutEmbedding called %d times before whole-batch validation", len(store.putRequests))
			}
		})
	}
}

func TestEmbeddingWorkerIgnoresOnlyStaleConflicts(t *testing.T) {
	store := &embeddingWorkerStore{
		pendingInputs: embeddingWorkerInputs(),
		putErrors:     []error{errors.Join(errors.New("corrected"), ErrConflict), nil},
	}
	embedder := &embeddingWorkerEmbedder{responses: [][][]float32{{{1, 0, 0}, {0, 1, 0}}}}
	worker := mustEmbeddingWorker(t, store, embedder)

	count, err := worker.RunOnce(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("RunOnce = %d, %v", count, err)
	}
	if len(store.putRequests) != 2 {
		t.Fatalf("put requests = %d", len(store.putRequests))
	}
}

func TestEmbeddingWorkerReturnsSuccessfulCountBeforePutFailure(t *testing.T) {
	putErr := errors.New("put failed")
	store := &embeddingWorkerStore{
		pendingInputs: embeddingWorkerInputs(),
		putErrors:     []error{nil, putErr},
	}
	embedder := &embeddingWorkerEmbedder{responses: [][][]float32{{{1, 0, 0}, {0, 1, 0}}}}
	worker := mustEmbeddingWorker(t, store, embedder)

	count, err := worker.RunOnce(context.Background())
	if count != 1 || !errors.Is(err, putErr) {
		t.Fatalf("RunOnce = %d, %v", count, err)
	}
}

func TestEmbeddingWorkerReturnsEmbeddingAndStoreErrorsWithoutWriting(t *testing.T) {
	recoverableErr := &BackendError{Op: "embed", Recoverable: true, Err: errors.New("offline")}
	store := &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs(), pendingErr: recoverableErr}
	embedder := &embeddingWorkerEmbedder{}
	worker := mustEmbeddingWorker(t, store, embedder)
	count, err := worker.RunOnce(context.Background())
	if count != 0 || !errors.Is(err, recoverableErr) || len(embedder.requests) != 0 {
		t.Fatalf("pending failure = %d, %v, embed calls %d", count, err, len(embedder.requests))
	}

	store = &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()}
	embedder = &embeddingWorkerEmbedder{errors: []error{recoverableErr}}
	worker = mustEmbeddingWorker(t, store, embedder)
	count, err = worker.RunOnce(context.Background())
	if count != 0 || !errors.Is(err, recoverableErr) || len(store.putRequests) != 0 {
		t.Fatalf("embed failure = %d, %v, put calls %d", count, err, len(store.putRequests))
	}
}

func TestEmbeddingWorkerPropagatesCanceledContext(t *testing.T) {
	store := &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()}
	embedder := &embeddingWorkerEmbedder{}
	worker := mustEmbeddingWorker(t, store, embedder)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	count, err := worker.RunOnce(ctx)
	if count != 0 || !errors.Is(err, context.Canceled) || len(store.pendingCalls) != 0 {
		t.Fatalf("pre-canceled RunOnce = %d, %v; pending calls %d", count, err, len(store.pendingCalls))
	}

	ctx, cancel = context.WithCancel(context.Background())
	store = &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()}
	embedder = &embeddingWorkerEmbedder{
		responses: [][][]float32{{{1, 0, 0}, {0, 1, 0}}},
		onEmbed:   cancel,
	}
	worker = mustEmbeddingWorker(t, store, embedder)
	count, err = worker.RunOnce(ctx)
	if count != 0 || !errors.Is(err, context.Canceled) || len(store.putRequests) != 0 {
		t.Fatalf("canceled-after-embed RunOnce = %d, %v; put calls %d", count, err, len(store.putRequests))
	}
}

func TestEmbeddingWorkerConfigAndConstructorFailClosed(t *testing.T) {
	validStore := &embeddingWorkerStore{}
	validEmbedder := &embeddingWorkerEmbedder{}
	valid := EmbeddingWorkerConfig{
		Store: validStore, Embedder: validEmbedder, ProfileID: "test-3d", Dimensions: 3, BatchSize: 8,
	}
	var typedNilStore *embeddingWorkerStore
	var typedNilEmbedder *embeddingWorkerEmbedder
	tests := []struct {
		name   string
		mutate func(*EmbeddingWorkerConfig)
	}{
		{name: "nil store", mutate: func(config *EmbeddingWorkerConfig) { config.Store = nil }},
		{name: "typed nil store", mutate: func(config *EmbeddingWorkerConfig) { config.Store = typedNilStore }},
		{name: "nil embedder", mutate: func(config *EmbeddingWorkerConfig) { config.Embedder = nil }},
		{name: "typed nil embedder", mutate: func(config *EmbeddingWorkerConfig) { config.Embedder = typedNilEmbedder }},
		{name: "blank profile", mutate: func(config *EmbeddingWorkerConfig) { config.ProfileID = "" }},
		{name: "whitespace profile", mutate: func(config *EmbeddingWorkerConfig) { config.ProfileID = " test-3d " }},
		{name: "zero dimensions", mutate: func(config *EmbeddingWorkerConfig) { config.Dimensions = 0 }},
		{name: "negative dimensions", mutate: func(config *EmbeddingWorkerConfig) { config.Dimensions = -1 }},
		{name: "zero batch size", mutate: func(config *EmbeddingWorkerConfig) { config.BatchSize = 0 }},
		{name: "negative batch size", mutate: func(config *EmbeddingWorkerConfig) { config.BatchSize = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if err := config.Validate(); !errors.Is(err, ErrInvalidMemory) {
				t.Fatalf("Validate error = %v, want ErrInvalidMemory", err)
			}
			if worker, err := NewEmbeddingWorker(config); worker != nil || !errors.Is(err, ErrInvalidMemory) {
				t.Fatalf("NewEmbeddingWorker = %#v, %v", worker, err)
			}
		})
	}
}

func embeddingWorkerInputs() []EmbeddingInput {
	return []EmbeddingInput{
		{MemoryID: embeddingWorkerMemoryOne, Scope: embeddingWorkerScope, Content: "first", ContentHash: "hash-first", Version: 1},
		{MemoryID: embeddingWorkerMemoryTwo, Scope: embeddingWorkerScope, Content: "second", ContentHash: "hash-second", Version: 2},
	}
}

func mustEmbeddingWorker(t *testing.T, store EmbeddingStore, embedder Embedder) *EmbeddingWorker {
	t.Helper()
	worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
		Store: store, Embedder: embedder, ProfileID: "test-3d", Dimensions: 3, BatchSize: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}
