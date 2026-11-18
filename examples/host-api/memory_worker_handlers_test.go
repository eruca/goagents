package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
)

type memoryWorkerStatusPayload struct {
	Backlog            int64                      `json:"backlog"`
	SuccessTotal       uint64                     `json:"success_total"`
	FailureTotal       uint64                     `json:"failure_total"`
	LastErrorClass     memorykit.WorkerErrorClass `json:"last_error_class,omitempty"`
	LastTransitionTime *time.Time                 `json:"last_transition_time,omitempty"`
}

type memoryWorkersPayload struct {
	Embedding  memoryWorkerStatusPayload `json:"embedding"`
	Extraction memoryWorkerStatusPayload `json:"extraction"`
}

// memoryWorkerEmbeddingStore serves one fixed batch and counts a fixed backlog.
type memoryWorkerEmbeddingStore struct {
	inputs       []memorykit.EmbeddingInput
	backlog      int64
	backlogErr   error
	backlogCalls int
}

func (s *memoryWorkerEmbeddingStore) PendingEmbeddings(context.Context, memorykit.PendingEmbeddingQuery) ([]memorykit.EmbeddingInput, error) {
	return append([]memorykit.EmbeddingInput(nil), s.inputs...), nil
}

func (*memoryWorkerEmbeddingStore) PutEmbedding(context.Context, memorykit.PutEmbeddingRequest) error {
	return nil
}

func (s *memoryWorkerEmbeddingStore) CountPendingEmbeddings(context.Context, memorykit.EmbeddingBacklogQuery) (int64, error) {
	s.backlogCalls++
	return s.backlog, s.backlogErr
}

// memoryWorkerExtractionStore hands out at most one job and counts a fixed backlog.
type memoryWorkerExtractionStore struct {
	memoryExtractionJobStoreStub
	job        *memorykit.ExtractionJob
	backlog    int64
	backlogErr error
}

func (s *memoryWorkerExtractionStore) ClaimExtraction(context.Context, string, time.Duration, int, time.Time) (memorykit.ExtractionJob, error) {
	if s.job == nil {
		return memorykit.ExtractionJob{}, memorykit.ErrNoExtractionJob
	}
	job := *s.job
	s.job = nil
	return job, nil
}

func (s *memoryWorkerExtractionStore) CountPendingExtractions(context.Context, memorykit.ExtractionBacklogQuery) (int64, error) {
	return s.backlog, s.backlogErr
}

func TestMemoryWorkerStatusRouteRequiresMemoryRuntime(t *testing.T) {
	response := httptest.NewRecorder()
	(&Server{}).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workers/memory?project_id=project-a", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET /workers/memory without memory runtime = %d, want 404", response.Code)
	}
}

func TestMemoryWorkerStatusRequiresReviewAuthorization(t *testing.T) {
	embedding := &memoryWorkerEmbeddingStore{}
	authorizer := validMemoryAuthorizer()
	server := newMemoryWorkerStatusServer(t, authorizer, embedding, memoryRuntimeEmbedder{}, &memoryWorkerExtractionStore{})

	anonymous := httptest.NewRecorder()
	server.Handler().ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/workers/memory?project_id=project-a", nil))
	if anonymous.Code != http.StatusUnauthorized || anonymous.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("anonymous status = %d, headers %v", anonymous.Code, anonymous.Header())
	}
	if missing := memoryRequest(t, server.Handler(), http.MethodGet, "/workers/memory", ""); missing.Code != http.StatusForbidden {
		t.Fatalf("missing project_id status = %d, want 403", missing.Code)
	}
	if len(authorizer.calls) != 0 {
		t.Fatalf("authorizer called without credentials or project: %#v", authorizer.calls)
	}

	allowed := memoryRequest(t, server.Handler(), http.MethodGet, "/workers/memory?project_id=project-a", "")
	if allowed.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body %s", allowed.Code, allowed.Body.String())
	}
	if len(authorizer.calls) != 1 || authorizer.calls[0].projectID != "project-a" ||
		authorizer.calls[0].capability != memoryReview {
		t.Fatalf("authorizer calls = %#v, want memory.review on project-a", authorizer.calls)
	}

	authorizer.err = errMemoryDeniedForTest
	before := embedding.backlogCalls
	denied := memoryRequest(t, server.Handler(), http.MethodGet, "/workers/memory?project_id=project-a", "")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d, want 403", denied.Code)
	}
	if embedding.backlogCalls != before {
		t.Fatal("denied request read the worker backlog")
	}
}

func TestMemoryWorkerStatusReturnsZeroSnapshots(t *testing.T) {
	server := newMemoryWorkerStatusServer(t, validMemoryAuthorizer(),
		&memoryWorkerEmbeddingStore{}, memoryRuntimeEmbedder{}, &memoryWorkerExtractionStore{})

	response := getMemoryWorkerStatus(t, server)
	body := response.Body.String()
	if strings.Contains(body, "last_error_class") || strings.Contains(body, "last_transition_time") {
		t.Fatalf("zero response contains optional fields: %s", body)
	}
	for _, field := range []string{`"backlog":0`, `"success_total":0`, `"failure_total":0`} {
		if strings.Count(body, field) != 2 {
			t.Fatalf("zero response does not contain two %s fields: %s", field, body)
		}
	}
	var got memoryWorkersPayload
	decodeMemoryWorkerStatus(t, response, &got)
	if got != (memoryWorkersPayload{}) {
		t.Fatalf("zero response = %#v", got)
	}
}

func TestMemoryWorkerStatusMapsSnapshotsWithoutContent(t *testing.T) {
	const (
		memorySentinel   = "secret-memory-content"
		sourceSentinel   = "secret-source-reference"
		providerSentinel = "secret-provider-payload"
		scopeSentinel    = "secret-tenant-scope"
		idSentinel       = "a7777777-7777-4777-8777-777777777777"
	)
	embedding := &memoryWorkerEmbeddingStore{
		backlog: 4,
		inputs: []memorykit.EmbeddingInput{{
			MemoryID:    idSentinel,
			Scope:       memorykit.Scope{TenantID: scopeSentinel, SubjectType: memorykit.SubjectProject, SubjectID: "project-a"},
			Content:     memorySentinel,
			ContentHash: "secret-content-hash",
			Version:     1,
		}},
	}
	job := memoryWorkerExtractionJob(sourceSentinel)
	extraction := &memoryWorkerExtractionStore{backlog: 1, job: &job}
	server := newMemoryWorkerStatusServer(t, validMemoryAuthorizer(),
		embedding, memoryRuntimeEmbedder{err: errors.New(providerSentinel)}, extraction)

	if _, err := server.memory.EmbeddingWorker.RunOnce(context.Background()); err == nil {
		t.Fatal("embedding RunOnce() error = nil, want provider failure")
	}
	if worked, err := server.memory.ExtractionWorker.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("extraction RunOnce() = %v, %v", worked, err)
	}

	response := getMemoryWorkerStatus(t, server)
	for _, sentinel := range []string{memorySentinel, sourceSentinel, providerSentinel, scopeSentinel, idSentinel, "test-3d"} {
		if strings.Contains(response.Body.String(), sentinel) {
			t.Fatalf("worker status leaked %q: %s", sentinel, response.Body.String())
		}
	}
	var got memoryWorkersPayload
	decodeMemoryWorkerStatus(t, response, &got)
	if got.Embedding.Backlog != 4 || got.Embedding.SuccessTotal != 0 || got.Embedding.FailureTotal != 1 ||
		got.Embedding.LastErrorClass != memorykit.WorkerErrorProviderFailed || got.Embedding.LastTransitionTime == nil {
		t.Fatalf("embedding status = %#v", got.Embedding)
	}
	wantExtractionTime := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	if got.Extraction.Backlog != 1 || got.Extraction.SuccessTotal != 1 || got.Extraction.FailureTotal != 0 ||
		got.Extraction.LastErrorClass != "" || got.Extraction.LastTransitionTime == nil ||
		!got.Extraction.LastTransitionTime.Equal(wantExtractionTime) {
		t.Fatalf("extraction status = %#v", got.Extraction)
	}
}

func TestMemoryWorkerStatusBacklogFailureIsContentFree(t *testing.T) {
	const secret = "secret-backlog-error"
	for _, test := range []struct {
		name       string
		embedding  *memoryWorkerEmbeddingStore
		extraction *memoryWorkerExtractionStore
	}{
		{name: "embedding", embedding: &memoryWorkerEmbeddingStore{backlogErr: errors.New(secret)}, extraction: &memoryWorkerExtractionStore{}},
		{name: "extraction", embedding: &memoryWorkerEmbeddingStore{}, extraction: &memoryWorkerExtractionStore{backlogErr: errors.New(secret)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newMemoryWorkerStatusServer(t, validMemoryAuthorizer(), test.embedding, memoryRuntimeEmbedder{}, test.extraction)
			response := memoryRequest(t, server.Handler(), http.MethodGet, "/workers/memory?project_id=project-a", "")
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), secret) {
				t.Fatalf("status = %d, body %s; want content-free 503", response.Code, response.Body.String())
			}
			var payload struct {
				Code string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Code != "memory_unavailable" {
				t.Fatalf("error body = %s (%v), want memory_unavailable", response.Body.String(), err)
			}
		})
	}
}

func newMemoryWorkerStatusServer(
	t *testing.T,
	authorizer MemoryAuthorizer,
	embedding *memoryWorkerEmbeddingStore,
	embedder memorykit.Embedder,
	extraction *memoryWorkerExtractionStore,
) *Server {
	t.Helper()
	config := validCompleteMemoryRuntimeConfig(t)
	config.Authorizer = authorizer
	embeddingWorker, err := memorykit.NewEmbeddingWorker(memorykit.EmbeddingWorkerConfig{
		Store: embedding, Embedder: embedder, ProfileID: "test-3d", Dimensions: 3, BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	extractionWorker, err := memorykit.NewExtractionWorker(memorykit.ExtractionWorkerConfig{
		Jobs: extraction, Memories: config.Store, Reader: memoryRuntimeSourceReader{},
		Extractor: memoryRuntimeExtractor{}, ValidateContent: &acceptingMemoryContentValidator{},
		Limits: config.Limits, WorkerID: "host-test", LeaseDuration: time.Minute, MaxAttempts: 3,
		Now: config.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	config.EmbeddingWorker = embeddingWorker
	config.ExtractionWorker = extractionWorker
	config.ExtractionJobs = extraction
	return &Server{memory: &memoryRuntime{memoryRuntimeConfig: *config}}
}

func memoryWorkerExtractionJob(sourceRef string) memorykit.ExtractionJob {
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	return memorykit.ExtractionJob{
		ID:            "a8888888-8888-4888-8888-888888888888",
		Scope:         memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"},
		Source:        memorykit.Source{Kind: "artifact", Ref: sourceRef},
		ExtractorID:   "host-test-extractor-v1",
		SourceAgentID: "agent-a",
		Status:        memorykit.ExtractionLeased,
		Attempts:      1,
		LeaseOwner:    "host-test",
		LeaseUntil:    now.Add(time.Minute),
		CreatedAt:     now.Add(-time.Minute),
		UpdatedAt:     now.Add(-time.Minute),
	}
}

func getMemoryWorkerStatus(t *testing.T, server *Server) *httptest.ResponseRecorder {
	t.Helper()
	response := memoryRequest(t, server.Handler(), http.MethodGet, "/workers/memory?project_id=project-a", "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /workers/memory = %d, body %s", response.Code, response.Body.String())
	}
	return response
}

func decodeMemoryWorkerStatus(t *testing.T, response *httptest.ResponseRecorder, target *memoryWorkersPayload) {
	t.Helper()
	decoder := json.NewDecoder(response.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode worker status: %v; body %s", err, response.Body.String())
	}
}
