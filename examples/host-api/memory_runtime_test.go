package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/goagent/agentcore"
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
		{name: "extractor actor id", mutate: func(config *memoryRuntimeConfig) {
			config.ExtractorID = strings.Repeat("e", config.Limits.MaxMetadataRunes-len("extractor:")+1)
		}},
		{name: "host run id metadata limit", mutate: func(config *memoryRuntimeConfig) {
			config.ExtractorID = "v1"
			config.Limits.MaxMetadataRunes = len("00000000-0000-4000-8000-000000000000") - 1
		}},
		{name: "embedding interval", mutate: func(config *memoryRuntimeConfig) { config.EmbeddingInterval = 0 }},
		{name: "extraction interval", mutate: func(config *memoryRuntimeConfig) { config.ExtractionInterval = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := *valid
			test.mutate(&config)
			runtime, err := newMemoryRuntime(&config, runkit.NewMemoryStore(), artifactkit.NewMemoryStore())
			if err == nil || runtime != nil {
				t.Fatalf("newMemoryRuntime() = %#v, %v, want nil error result", runtime, err)
			}
		})
	}
}

func TestMemoryRuntimeAcceptsHostRunIDMetadataLimitBoundary(t *testing.T) {
	config := validCompleteMemoryRuntimeConfig(t)
	config.ExtractorID = "v1"
	config.Limits.MaxMetadataRunes = len("00000000-0000-4000-8000-000000000000")

	runtime, err := newMemoryRuntime(config, runkit.NewMemoryStore(), artifactkit.NewMemoryStore())
	if err != nil || runtime == nil {
		t.Fatalf("newMemoryRuntime() = %#v, %v, want valid exact Host run ID boundary", runtime, err)
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

func TestHostMemoryQueryBuilderUsesTrustedWorkflowArtifactTransiently(t *testing.T) {
	artifacts := artifactkit.NewMemoryStore()
	ref := "artifact:workflow-query:input"
	if err := artifacts.Put(t.Context(), artifactkit.Artifact{
		Ref: ref, Content: []byte("current user question"), ContentType: "text/plain",
	}); err != nil {
		t.Fatal(err)
	}
	builder, err := newHostMemoryQueryBuilder(artifacts, len([]rune("current user question")))
	if err != nil {
		t.Fatal(err)
	}
	request := agentcore.ContextProjectionRequest{
		Messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + ref}},
		Metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": ref},
	}
	text, keys, kinds, err := builder(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if text != "current user question" || len(keys) != 0 || len(kinds) != 0 {
		t.Fatalf("query = %q, %#v, %#v", text, keys, kinds)
	}
	if request.Messages[0].Content != "Review input artifact "+ref ||
		!reflect.DeepEqual(request.Metadata, map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": ref}) {
		t.Fatalf("builder mutated projection request: %#v", request)
	}
}

func TestHostMemoryQueryBuilderDelegatesNonWorkflowRequests(t *testing.T) {
	builder, err := newHostMemoryQueryBuilder(artifactkit.NewMemoryStore(), 100)
	if err != nil {
		t.Fatal(err)
	}
	request := agentcore.ContextProjectionRequest{
		Messages: []agentcore.Message{{Role: "user", Content: "direct question"}},
		Metadata: map[string]any{
			"workflow_id":        "direct-runner-audit",
			"memory.query_tags":  []string{"zeta", "alpha"},
			"memory.query_keys":  []any{"key.2", "key.1"},
			"memory.query_kinds": []string{"fact", "constraint"},
		},
	}
	text, keys, kinds, err := builder(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if text != "direct question\nalpha\nzeta" ||
		!reflect.DeepEqual(keys, []string{"key.2", "key.1"}) ||
		!reflect.DeepEqual(kinds, []memorykit.Kind{memorykit.KindFact, memorykit.KindConstraint}) {
		t.Fatalf("delegated query = %q, %#v, %#v", text, keys, kinds)
	}
}

func TestHostMemoryQueryBuilderFailsClosedOnUntrustedWorkflowEnvelope(t *testing.T) {
	const privateBody = "PRIVATE QUERY BODY"
	validRef := "artifact:workflow-query:input"
	tests := []struct {
		name     string
		store    artifactkit.Store
		maxRunes int
		messages []agentcore.Message
		metadata map[string]any
	}{
		{
			name: "workflow ID type", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: validRef, Content: []byte(privateBody), ContentType: "text/plain",
			}}, maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": 7, "workflow_input_ref": validRef},
		},
		{
			name: "workflow ID blank", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: validRef, Content: []byte(privateBody), ContentType: "text/plain",
			}}, maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": " ", "workflow_input_ref": validRef},
		},
		{
			name: "input ref type", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: validRef, Content: []byte(privateBody), ContentType: "text/plain",
			}}, maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": 7},
		},
		{
			name: "input ref metadata mismatch", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: validRef, Content: []byte(privateBody), ContentType: "text/plain",
			}}, maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": "artifact:other:input"},
		},
		{
			name: "user ref mismatch", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: validRef, Content: []byte(privateBody), ContentType: "text/plain",
			}}, maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact artifact:other:input"}},
			metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": validRef},
		},
		{
			name: "record ref mismatch", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: "artifact:other:input", Content: []byte(privateBody), ContentType: "text/plain",
			}}, maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": validRef},
		},
		{
			name: "record content type", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: validRef, Content: []byte(privateBody), ContentType: "application/octet-stream",
			}}, maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": validRef},
		},
		{
			name: "invalid UTF-8", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: validRef, Content: []byte{0xff}, ContentType: "text/plain",
			}}, maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": validRef},
		},
		{
			name: "blank", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: validRef, Content: []byte(" \n"), ContentType: "text/plain",
			}}, maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": validRef},
		},
		{
			name: "NUL", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: validRef, Content: []byte(privateBody + "\x00"), ContentType: "text/plain",
			}}, maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": validRef},
		},
		{
			name: "oversized", store: runtimeQueryArtifactStore{artifact: artifactkit.Artifact{
				Ref: validRef, Content: []byte(strings.Repeat("界", 4)), ContentType: "text/plain",
			}}, maxRunes: 3,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": validRef},
		},
		{
			name: "artifact unavailable", store: runtimeQueryArtifactStore{err: errors.New(privateBody)},
			maxRunes: 100,
			messages: []agentcore.Message{{Role: "user", Content: "Review input artifact " + validRef}},
			metadata: map[string]any{"workflow_id": "workflow-query", "workflow_input_ref": validRef},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builder, err := newHostMemoryQueryBuilder(test.store, test.maxRunes)
			if err != nil {
				t.Fatal(err)
			}
			text, keys, kinds, err := builder(t.Context(), agentcore.ContextProjectionRequest{
				Messages: test.messages, Metadata: test.metadata,
			})
			if err == nil || text != "" || keys != nil || kinds != nil {
				t.Fatalf("query = %q, %#v, %#v, %v, want zero fail closed", text, keys, kinds, err)
			}
			if strings.Contains(err.Error(), privateBody) {
				t.Fatalf("query error leaked private body: %v", err)
			}
		})
	}
}

func TestHostMemoryQueryBuilderRejectsIncompleteConfiguration(t *testing.T) {
	var typedNil *artifactkit.MemoryStore
	for _, store := range []artifactkit.Store{nil, typedNil} {
		if builder, err := newHostMemoryQueryBuilder(store, 100); err == nil || builder != nil {
			t.Fatalf("newHostMemoryQueryBuilder(%#v) = %#v, %v", store, builder, err)
		}
	}
	if builder, err := newHostMemoryQueryBuilder(artifactkit.NewMemoryStore(), 0); err == nil || builder != nil {
		t.Fatalf("newHostMemoryQueryBuilder(max=0) = %#v, %v", builder, err)
	}
}

func TestHostMemoryQueryBuilderPreservesArtifactContextErrors(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		builder, err := newHostMemoryQueryBuilder(runtimeQueryArtifactStore{err: fmt.Errorf("private wrapper: %w", want)}, 100)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = builder(t.Context(), agentcore.ContextProjectionRequest{
			Messages: []agentcore.Message{{Role: "user", Content: "Review input artifact artifact:workflow-query:input"}},
			Metadata: map[string]any{
				"workflow_id": "workflow-query", "workflow_input_ref": "artifact:workflow-query:input",
			},
		})
		if !errors.Is(err, want) || strings.Contains(err.Error(), "private wrapper") {
			t.Fatalf("query error = %v, want sanitized %v", err, want)
		}
	}
}

type runtimeQueryArtifactStore struct {
	artifact artifactkit.Artifact
	err      error
}

func (s runtimeQueryArtifactStore) Put(context.Context, artifactkit.Artifact) error { return nil }

func (s runtimeQueryArtifactStore) Get(context.Context, string) (artifactkit.Artifact, error) {
	return s.artifact, s.err
}

func TestMemoryRuntimeEnqueueUsesStableTrustedJobIdentity(t *testing.T) {
	config := validCompleteMemoryRuntimeConfig(t)
	queue := config.ExtractionJobs.(*memoryExtractionJobStoreStub)
	runID := "00000000-0000-4000-8000-000000000888"
	outputRef := "artifact:" + runID + ":agent-output"
	runs := runkit.NewMemoryStore()
	if err := runs.Create(t.Context(), runkit.RunRecord{RunID: runID, WorkflowID: "workflow-a", Status: runkit.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := runs.Complete(t.Context(), runID, runkit.TerminalSummary{Status: runkit.StatusSucceeded, ContentRef: outputRef}); err != nil {
		t.Fatal(err)
	}
	completed, err := runs.Get(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	clockCalls := 0
	config.Now = func() time.Time {
		clockCalls++
		return time.Date(2026, 7, 21, 8, clockCalls, 0, 0, time.UTC)
	}
	runtime, err := newMemoryRuntime(config, runs, artifactkit.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	for range 2 {
		if err := runtime.EnqueueExtraction(t.Context(), scope, outputRef, runID); err != nil {
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
	if first.ID == runID || first.Scope != scope || first.Source != (memorykit.Source{Kind: "artifact", Ref: outputRef}) ||
		first.SourceAgentID != runID || first.ExtractorID != config.ExtractorID || first.Status != memorykit.ExtractionPending ||
		first.Attempts != 0 || !first.CreatedAt.Equal(completed.UpdatedAt) || first.UpdatedAt != first.CreatedAt {
		t.Fatalf("enqueued job=%#v", first)
	}
}

func TestMemoryRuntimeEnqueueRejectsSourceNotBoundToAgentRun(t *testing.T) {
	config := validCompleteMemoryRuntimeConfig(t)
	runs := runkit.NewMemoryStore()
	runID := "00000000-0000-4000-8000-000000000889"
	legacyRef := "artifact:workflow-a:agent-output"
	if err := runs.Create(t.Context(), runkit.RunRecord{RunID: runID, WorkflowID: "workflow-a", Status: runkit.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := runs.Complete(t.Context(), runID, runkit.TerminalSummary{Status: runkit.StatusSucceeded, ContentRef: legacyRef}); err != nil {
		t.Fatal(err)
	}
	runtime, err := newMemoryRuntime(config, runs, artifactkit.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}

	if err := runtime.EnqueueExtraction(t.Context(), scope, legacyRef, runID); err == nil {
		t.Fatal("EnqueueExtraction accepted source ref not bound to Agent run ID")
	}
	if queue := config.ExtractionJobs.(*memoryExtractionJobStoreStub); len(queue.enqueued) != 0 {
		t.Fatalf("invalid source enqueued jobs=%#v", queue.enqueued)
	}
}

func TestMemoryRuntimeCopiesConfigAndBuildsAdapters(t *testing.T) {
	config := validCompleteMemoryRuntimeConfig(t)
	runtime, err := newMemoryRuntime(config, runkit.NewMemoryStore(), artifactkit.NewMemoryStore())
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
	runtime, err := newMemoryRuntime(config, runkit.NewMemoryStore(), artifactkit.NewMemoryStore())
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
	runtime, err := newMemoryRuntime(config, runkit.NewMemoryStore(), artifactkit.NewMemoryStore())
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
