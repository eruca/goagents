package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/memorystore"
	"github.com/eruca/goagents/runkit"
)

func TestMemoryRuntimeRejectsIncompleteConfig(t *testing.T) {
	valid := validCompleteMemoryRuntimeConfig(t)
	tests := []struct {
		name   string
		mutate func(*memoryRuntimeConfig)
	}{
		{name: "auto recall", mutate: func(config *memoryRuntimeConfig) { config.AutoRecall = nil }},
		{name: "deep recall", mutate: func(config *memoryRuntimeConfig) { config.DeepRecall = nil }},
		{name: "embedding worker", mutate: func(config *memoryRuntimeConfig) { config.EmbeddingWorker = nil }},
		{name: "extraction worker", mutate: func(config *memoryRuntimeConfig) { config.ExtractionWorker = nil }},
		{name: "extraction jobs", mutate: func(config *memoryRuntimeConfig) { config.ExtractionJobs = nil }},
		{name: "extractor id", mutate: func(config *memoryRuntimeConfig) { config.ExtractorID = "" }},
		{name: "embedding interval", mutate: func(config *memoryRuntimeConfig) { config.EmbeddingInterval = 0 }},
		{name: "extraction interval", mutate: func(config *memoryRuntimeConfig) { config.ExtractionInterval = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := *valid
			test.mutate(&config)
			runtime, err := newMemoryRuntime(&config, runkit.NewMemoryStore())
			if err == nil || runtime != nil {
				t.Fatalf("newMemoryRuntime() = %#v, %v, want nil error result", runtime, err)
			}
		})
	}
}

func TestMemoryRuntimeArtifactSourceReaderRejectsUnsupportedAndOversizedSource(t *testing.T) {
	artifacts := artifactkit.NewMemoryStore()
	limits := memoryLimitsForTest()
	reader, err := newHostArtifactSourceReader(artifacts, limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := artifacts.Put(t.Context(), artifactkit.Artifact{
		Ref: "artifact:bounded", Content: []byte("bounded source"), ContentType: "text/plain",
	}); err != nil {
		t.Fatal(err)
	}
	text, err := reader.ReadSource(t.Context(), memorykit.Source{Kind: "artifact", Ref: "artifact:bounded"})
	if err != nil || text != "bounded source" {
		t.Fatalf("ReadSource() = %q, %v", text, err)
	}
	if _, err := reader.ReadSource(t.Context(), memorykit.Source{Kind: "git", Ref: "artifact:bounded"}); err == nil {
		t.Fatal("ReadSource accepted non-artifact source kind")
	}
	if _, err := reader.ReadSource(t.Context(), memorykit.Source{Kind: "artifact", Ref: "run:bounded"}); err == nil {
		t.Fatal("ReadSource accepted non-artifact source ref")
	}
	oversized := "artifact:oversized"
	if err := artifacts.Put(t.Context(), artifactkit.Artifact{
		Ref: oversized, Content: []byte(strings.Repeat("界", limits.MaxContentRunes+1)), ContentType: "text/plain",
	}); err != nil {
		t.Fatal(err)
	}
	if text, err := reader.ReadSource(t.Context(), memorykit.Source{Kind: "artifact", Ref: oversized}); err == nil || text != "" {
		t.Fatalf("ReadSource oversized = %q, %v, want empty error", text, err)
	}
}

func TestMemoryRuntimeEnqueueUsesStableTrustedJobIdentity(t *testing.T) {
	config := validCompleteMemoryRuntimeConfig(t)
	queue := config.ExtractionJobs.(*memoryExtractionJobStoreStub)
	runtime, err := newMemoryRuntime(config, runkit.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	runID := "00000000-0000-4000-8000-000000000888"
	for range 2 {
		if err := runtime.EnqueueExtraction(t.Context(), scope, "artifact:workflow-a:agent-output", runID); err != nil {
			t.Fatal(err)
		}
	}
	if len(queue.enqueued) != 2 {
		t.Fatalf("enqueued=%d, want 2 replay attempts", len(queue.enqueued))
	}
	first, second := queue.enqueued[0], queue.enqueued[1]
	if first != second {
		t.Fatalf("stable enqueue changed job: first=%#v second=%#v", first, second)
	}
	if first.ID == runID || first.Scope != scope || first.Source != (memorykit.Source{Kind: "artifact", Ref: "artifact:workflow-a:agent-output"}) ||
		first.SourceAgentID != runID || first.ExtractorID != config.ExtractorID || first.Status != memorykit.ExtractionPending ||
		first.Attempts != 0 || first.CreatedAt.IsZero() || first.UpdatedAt != first.CreatedAt {
		t.Fatalf("enqueued job=%#v", first)
	}
}

func TestMemoryRuntimeCopiesConfigAndBuildsAdapters(t *testing.T) {
	config := validCompleteMemoryRuntimeConfig(t)
	runtime, err := newMemoryRuntime(config, runkit.NewMemoryStore())
	if err != nil {
		t.Fatalf("newMemoryRuntime() error = %v", err)
	}
	if runtime.Projector() == nil || runtime.ToolProvider() == nil {
		t.Fatal("newMemoryRuntime() did not build Agent adapters")
	}

	originalStore := runtime.Store
	config.Store = nil
	config.Authorizer = nil
	config.EmbeddingInterval = 0
	if runtime.Store != originalStore || runtime.EmbeddingInterval <= 0 || runtime.Authorizer == nil {
		t.Fatal("runtime aliases caller-owned top-level configuration")
	}
}

func TestMemoryRuntimeTrustedScopeRejectsMalformedOrInjectedMetadata(t *testing.T) {
	want := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	metadata := trustedMemoryMetadataForTest("tenant-a", "project-a", "user-a", true)
	got, err := resolveTrustedMemoryScope(metadata)
	if err != nil || got != want {
		t.Fatalf("resolveTrustedMemoryScope() = %+v, %v, want %+v", got, err, want)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing tenant", mutate: func(values map[string]any) { delete(values, memoryTenantMetadataKey) }},
		{name: "blank project", mutate: func(values map[string]any) { values[memoryProjectMetadataKey] = " " }},
		{name: "wrong user type", mutate: func(values map[string]any) { values[memoryUserMetadataKey] = 7 }},
		{name: "wrong intent type", mutate: func(values map[string]any) { values[memoryWriteIntentMetadataKey] = "true" }},
		{name: "unknown memory key", mutate: func(values map[string]any) { values["memory.tenant_override"] = "other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := trustedMemoryMetadataForTest("tenant-a", "project-a", "user-a", true)
			test.mutate(values)
			if scope, err := resolveTrustedMemoryScope(values); err == nil || scope != (memorykit.Scope{}) {
				t.Fatalf("resolveTrustedMemoryScope() = %+v, %v, want zero scope error", scope, err)
			}
		})
	}
}

func TestMemoryRuntimeWorkerLoopsStopClaimsOnIntakeAndCancelCurrentOnExecution(t *testing.T) {
	embeddings := &blockingEmbeddingStore{entered: make(chan struct{}), returned: make(chan struct{})}
	config := validCompleteMemoryRuntimeConfig(t)
	worker, err := memorykit.NewEmbeddingWorker(memorykit.EmbeddingWorkerConfig{
		Store: embeddings, Embedder: memoryRuntimeEmbedder{}, ProfileID: "test-3d", Dimensions: 3, BatchSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	config.EmbeddingWorker = worker
	config.EmbeddingInterval = time.Millisecond
	runtime, err := newMemoryRuntime(config, runkit.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}

	intakeCtx, cancelIntake := context.WithCancel(context.Background())
	executionCtx, cancelExecution := context.WithCancel(context.Background())
	runtime.StartMemoryWorkers(intakeCtx, executionCtx)
	select {
	case <-embeddings.entered:
	case <-time.After(time.Second):
		t.Fatal("embedding worker did not start")
	}
	cancelIntake()
	select {
	case <-embeddings.returned:
		t.Fatal("intake cancellation interrupted the current batch")
	case <-time.After(20 * time.Millisecond):
	}
	cancelExecution()
	select {
	case <-embeddings.returned:
	case <-time.After(time.Second):
		t.Fatal("execution cancellation did not interrupt current batch")
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if err := runtime.WaitMemoryWorkers(waitCtx); err != nil {
		t.Fatalf("WaitMemoryWorkers() error = %v", err)
	}
}

func TestMemoryRuntimeWorkerLoopRetriesDependencyDeadlineWhileExecutionIsActive(t *testing.T) {
	store := &deadlineEmbeddingStore{calls: make(chan struct{}, 2)}
	config := validCompleteMemoryRuntimeConfig(t)
	worker, err := memorykit.NewEmbeddingWorker(memorykit.EmbeddingWorkerConfig{
		Store: store, Embedder: memoryRuntimeEmbedder{}, ProfileID: "test-3d", Dimensions: 3, BatchSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	config.EmbeddingWorker = worker
	config.EmbeddingInterval = time.Millisecond
	runtime, err := newMemoryRuntime(config, runkit.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	intakeCtx, cancelIntake := context.WithCancel(context.Background())
	executionCtx, cancelExecution := context.WithCancel(context.Background())
	runtime.StartMemoryWorkers(intakeCtx, executionCtx)
	for call := 0; call < 2; call++ {
		select {
		case <-store.calls:
		case <-time.After(time.Second):
			t.Fatalf("worker stopped after dependency deadline; calls=%d", call+1)
		}
	}
	cancelIntake()
	cancelExecution()
	if err := runtime.WaitMemoryWorkers(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func trustedMemoryMetadataForTest(tenantID, projectID, userID string, writeIntent bool) map[string]any {
	return map[string]any{
		memoryTenantMetadataKey:      tenantID,
		memoryProjectMetadataKey:     projectID,
		memoryUserMetadataKey:        userID,
		memoryWriteIntentMetadataKey: writeIntent,
	}
}

func validCompleteMemoryRuntimeConfig(t *testing.T) *memoryRuntimeConfig {
	t.Helper()
	limits := memoryLimitsForTest()
	store, err := memorystore.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	queue := &memoryExtractionJobStoreStub{}
	autoRecall := newMemoryRuntimeRecaller(t, store)
	deepRecall := newMemoryRuntimeRecaller(t, store)
	embeddingWorker, err := memorykit.NewEmbeddingWorker(memorykit.EmbeddingWorkerConfig{
		Store: store, Embedder: memoryRuntimeEmbedder{}, ProfileID: "test-3d", Dimensions: 3, BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	extractionWorker, err := memorykit.NewExtractionWorker(memorykit.ExtractionWorkerConfig{
		Jobs: queue, Memories: store, Reader: memoryRuntimeSourceReader{},
		Extractor: memoryRuntimeExtractor{}, ValidateContent: &acceptingMemoryContentValidator{},
		Limits: limits, WorkerID: "host-test", LeaseDuration: time.Second, MaxAttempts: 3,
		Now: func() time.Time { return time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &memoryRuntimeConfig{
		Store: store, Authorizer: validMemoryAuthorizer(), ContentValidator: &acceptingMemoryContentValidator{},
		Limits: limits, AutoRecall: autoRecall, DeepRecall: deepRecall,
		EmbeddingWorker: embeddingWorker, ExtractionWorker: extractionWorker, ExtractionJobs: queue,
		ExtractorID:      "host-test-extractor-v1",
		NewID:            func() string { return "00000000-0000-4000-8000-000000000123" },
		Now:              func() time.Time { return time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC) },
		MaxHTTPBodyBytes: 4096, EmbeddingInterval: time.Hour, ExtractionInterval: time.Hour,
	}
}

func newMemoryRuntimeRecaller(t *testing.T, store memorykit.RecallStore) *memorykit.Recaller {
	t.Helper()
	recaller, err := memorykit.NewRecaller(memorykit.RecallConfig{
		Store: store, Embedder: memoryRuntimeEmbedder{}, CountTokens: func(string) int { return 1 },
		Policy: memoryRuntimeRecallPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return recaller
}

func memoryRuntimeRecallPolicy() memorykit.RecallPolicy {
	return memorykit.RecallPolicy{
		Version: "test-v1", ExactLimit: 4, FullTextLimit: 4, VectorLimit: 4,
		RRFK: 60, MinVectorSimilarity: 0.5, MaxItems: 4, MaxTokens: 64,
		MaxQueryRunes: 1024, MaxKeys: 8, Deadline: time.Second,
		EmbeddingProfileID: "test-3d", EmbeddingDimensions: 3,
	}
}

type memoryRuntimeEmbedder struct{ err error }

func (e memoryRuntimeEmbedder) Embed(context.Context, memorykit.EmbedRequest) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	return [][]float32{{1, 0, 0}}, nil
}

type memoryRuntimeSourceReader struct{}

func (memoryRuntimeSourceReader) ReadSource(context.Context, memorykit.Source) (string, error) {
	return "source", nil
}

type memoryRuntimeExtractor struct{}

func (memoryRuntimeExtractor) Extract(context.Context, memorykit.ExtractionRequest) ([]memorykit.CandidateDraft, error) {
	return nil, nil
}

type memoryExtractionJobStoreStub struct {
	enqueued []memorykit.ExtractionJob
	err      error
}

func (s *memoryExtractionJobStoreStub) EnqueueExtraction(_ context.Context, job memorykit.ExtractionJob) error {
	s.enqueued = append(s.enqueued, job)
	return s.err
}

func (*memoryExtractionJobStoreStub) ClaimExtraction(context.Context, string, time.Duration, int, time.Time) (memorykit.ExtractionJob, error) {
	return memorykit.ExtractionJob{}, memorykit.ErrNoExtractionJob
}

func (*memoryExtractionJobStoreStub) CompleteExtraction(context.Context, string, string, int, time.Time) error {
	return nil
}

func (*memoryExtractionJobStoreStub) FailExtraction(context.Context, string, string, int, string, int, time.Time) error {
	return nil
}

type blockingEmbeddingStore struct {
	entered  chan struct{}
	returned chan struct{}
}

type deadlineEmbeddingStore struct{ calls chan struct{} }

func (s *deadlineEmbeddingStore) PendingEmbeddings(context.Context, memorykit.PendingEmbeddingQuery) ([]memorykit.EmbeddingInput, error) {
	s.calls <- struct{}{}
	return nil, context.DeadlineExceeded
}

func (*deadlineEmbeddingStore) PutEmbedding(context.Context, memorykit.PutEmbeddingRequest) error {
	return errors.New("unexpected PutEmbedding")
}

func (s *blockingEmbeddingStore) PendingEmbeddings(ctx context.Context, _ memorykit.PendingEmbeddingQuery) ([]memorykit.EmbeddingInput, error) {
	select {
	case <-s.entered:
	default:
		close(s.entered)
	}
	<-ctx.Done()
	close(s.returned)
	return nil, ctx.Err()
}

func (*blockingEmbeddingStore) PutEmbedding(context.Context, memorykit.PutEmbeddingRequest) error {
	return errors.New("unexpected PutEmbedding")
}

func TestMemoryRuntimeTrustedMetadataHelperDoesNotAlias(t *testing.T) {
	values := trustedMemoryMetadataForTest("tenant-a", "project-a", "user-a", true)
	want := map[string]any{
		memoryTenantMetadataKey: "tenant-a", memoryProjectMetadataKey: "project-a",
		memoryUserMetadataKey: "user-a", memoryWriteIntentMetadataKey: true,
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("metadata = %#v, want %#v", values, want)
	}
}
