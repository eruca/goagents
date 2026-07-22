package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/memorykit"
	memoryagentadapter "github.com/eruca/goagents/memorykit/agentadapter"
	"github.com/eruca/goagents/runkit"
	"github.com/google/uuid"
)

const (
	memoryTenantMetadataKey      = "memory.tenant_id"
	memoryProjectMetadataKey     = "memory.project_id"
	memoryUserMetadataKey        = "memory.user_id"
	memoryWriteIntentMetadataKey = "memory.write_intent"
	hostSourceAgentIDRunes       = 36
)

var trustedMemoryMetadataKeys = map[string]struct{}{
	memoryTenantMetadataKey: {}, memoryProjectMetadataKey: {},
	memoryUserMetadataKey: {}, memoryWriteIntentMetadataKey: {},
}

// memoryRuntime is the validated Host composition. Its embedded configuration
// is a top-level copy; the Store and workers remain owned by the caller.
type memoryRuntime struct {
	memoryRuntimeConfig
	projector    *memoryagentadapter.Projector
	toolProvider *memoryagentadapter.ToolProvider
	observer     *hostMemoryObserver
	runs         runkit.Store

	workerStart sync.Once
	workerMu    sync.Mutex
	workerOn    bool
	embedDone   chan struct{}
	extractDone chan struct{}
}

func newMemoryRuntime(config *memoryRuntimeConfig, runs runkit.Store) (*memoryRuntime, error) {
	if err := validateMemoryManagementConfig(config); err != nil {
		return nil, err
	}
	if config == nil || nilMemoryDependency(config.AutoRecall) || nilMemoryDependency(config.DeepRecall) ||
		nilMemoryDependency(config.EmbeddingWorker) || nilMemoryDependency(config.ExtractionWorker) ||
		nilMemoryDependency(config.ExtractionJobs) || nilMemoryDependency(runs) ||
		config.EmbeddingInterval <= 0 || config.ExtractionInterval <= 0 ||
		config.Limits.MaxMetadataRunes < hostSourceAgentIDRunes ||
		!validMemoryRuntimeIdentity(config.ExtractorID, config.Limits.MaxMetadataRunes) ||
		!validMemoryRuntimeIdentity("extractor:"+config.ExtractorID, config.Limits.MaxMetadataRunes) {
		return nil, fmt.Errorf("invalid memory runtime configuration")
	}
	copy := *config
	observer := &hostMemoryObserver{runs: runs}
	projector, err := memoryagentadapter.NewProjector(memoryagentadapter.ProjectorConfig{
		Recall: copy.AutoRecall, ResolveScope: resolveTrustedMemoryScope,
		BuildQuery: memoryagentadapter.DefaultQueryBuilder, Observe: observer, Next: nil,
	})
	if err != nil {
		return nil, fmt.Errorf("build memory projector: %w", err)
	}
	provider, err := memoryagentadapter.NewToolProvider(memoryagentadapter.ToolProviderConfig{
		Store: copy.Store, DeepRecall: copy.DeepRecall, ResolveScope: resolveTrustedMemoryScope,
		AllowExplicitWrite: func(request agentcore.RunRequest) bool {
			intent, ok := request.Metadata[memoryWriteIntentMetadataKey].(bool)
			return ok && intent
		},
		ValidateContent: copy.ContentValidator, Limits: copy.Limits, Now: copy.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("build memory tool provider: %w", err)
	}
	runtime := &memoryRuntime{memoryRuntimeConfig: copy, projector: projector, toolProvider: provider, observer: observer, runs: runs}
	return runtime, nil
}

func (r *memoryRuntime) Projector() *memoryagentadapter.Projector { return r.projector }

func (r *memoryRuntime) ToolProvider() *memoryagentadapter.ToolProvider { return r.toolProvider }

func resolveTrustedMemoryScope(metadata map[string]any) (memorykit.Scope, error) {
	for key := range metadata {
		if strings.HasPrefix(key, "memory.") {
			if _, allowed := trustedMemoryMetadataKeys[key]; !allowed {
				return memorykit.Scope{}, fmt.Errorf("%w: unknown trusted memory metadata", memorykit.ErrInvalidScope)
			}
		}
	}
	tenantID, tenantOK := metadata[memoryTenantMetadataKey].(string)
	projectID, projectOK := metadata[memoryProjectMetadataKey].(string)
	userID, userOK := metadata[memoryUserMetadataKey].(string)
	_, intentOK := metadata[memoryWriteIntentMetadataKey].(bool)
	if !tenantOK || !projectOK || !userOK || !intentOK || strings.TrimSpace(userID) == "" || userID != strings.TrimSpace(userID) {
		return memorykit.Scope{}, fmt.Errorf("%w: malformed trusted memory metadata", memorykit.ErrInvalidScope)
	}
	scope := memorykit.Scope{TenantID: tenantID, SubjectType: memorykit.SubjectProject, SubjectID: projectID}
	if err := scope.Validate(); err != nil {
		return memorykit.Scope{}, err
	}
	return scope, nil
}

func (r *memoryRuntime) StartMemoryWorkers(intakeCtx, executionCtx context.Context) {
	if r == nil {
		return
	}
	r.workerStart.Do(func() {
		r.workerMu.Lock()
		r.workerOn = true
		r.embedDone = make(chan struct{})
		r.extractDone = make(chan struct{})
		r.workerMu.Unlock()
		go r.runEmbeddingLoop(intakeCtx, executionCtx)
		go r.runExtractionLoop(intakeCtx, executionCtx)
	})
}

func (r *memoryRuntime) runEmbeddingLoop(intakeCtx, executionCtx context.Context) {
	defer close(r.embedDone)
	ticker := time.NewTicker(r.EmbeddingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-intakeCtx.Done():
			return
		case <-ticker.C:
			if intakeCtx.Err() != nil {
				return
			}
			_, _ = r.EmbeddingWorker.RunOnce(executionCtx)
			if executionCtx.Err() != nil {
				return
			}
		}
	}
}

func (r *memoryRuntime) runExtractionLoop(intakeCtx, executionCtx context.Context) {
	defer close(r.extractDone)
	ticker := time.NewTicker(r.ExtractionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-intakeCtx.Done():
			return
		case <-ticker.C:
			if intakeCtx.Err() != nil {
				return
			}
			_, _ = r.ExtractionWorker.RunOnce(executionCtx)
			if executionCtx.Err() != nil {
				return
			}
		}
	}
}

func (r *memoryRuntime) WaitMemoryWorkers(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.workerMu.Lock()
	if !r.workerOn {
		r.workerMu.Unlock()
		return nil
	}
	embedDone, extractDone := r.embedDone, r.extractDone
	r.workerMu.Unlock()
	for _, done := range []<-chan struct{}{embedDone, extractDone} {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *Server) StartMemoryWorkers(intakeCtx, executionCtx context.Context) {
	if s != nil && s.memory != nil {
		s.memory.StartMemoryWorkers(intakeCtx, executionCtx)
	}
}

func (s *Server) WaitMemoryWorkers(ctx context.Context) error {
	if s == nil || s.memory == nil {
		return nil
	}
	return s.memory.WaitMemoryWorkers(ctx)
}

func (r *memoryRuntime) EnqueueExtraction(ctx context.Context, scope memorykit.Scope, outputRef, sourceAgentID string) error {
	if r == nil || strings.TrimSpace(outputRef) == "" || !strings.HasPrefix(outputRef, "artifact:") ||
		memorykit.ValidateMemoryID(sourceAgentID) != nil || outputRef != "artifact:"+sourceAgentID+":agent-output" {
		return fmt.Errorf("%w: invalid Host extraction source", memorykit.ErrInvalidMemory)
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	completed, err := r.runs.Get(ctx, sourceAgentID)
	if err != nil {
		return err
	}
	if completed.RunID != sourceAgentID || completed.Status != runkit.StatusSucceeded ||
		completed.Summary.Status != runkit.StatusSucceeded || completed.Summary.ContentRef != outputRef || completed.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: extraction source is not a matching completed Agent run", memorykit.ErrInvalidMemory)
	}
	now := completed.UpdatedAt.UTC()
	identity := strings.Join([]string{"goagents.host.memory-extraction.v1", scope.TenantID, string(scope.SubjectType), scope.SubjectID, sourceAgentID, outputRef}, "\x00")
	job := memorykit.ExtractionJob{
		ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(identity)).String(), Scope: scope,
		Source:        memorykit.Source{Kind: "artifact", Ref: outputRef},
		SourceAgentID: sourceAgentID, ExtractorID: r.ExtractorID,
		Status: memorykit.ExtractionPending, CreatedAt: now, UpdatedAt: now,
	}
	return r.ExtractionJobs.EnqueueExtraction(ctx, job)
}

type hostArtifactSourceReader struct {
	artifacts artifactkit.Store
	limits    memorykit.Limits
}

func newHostArtifactSourceReader(artifacts artifactkit.Store, limits memorykit.Limits) (*hostArtifactSourceReader, error) {
	if nilMemoryDependency(artifacts) {
		return nil, fmt.Errorf("%w: artifact source store is required", memorykit.ErrInvalidMemory)
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &hostArtifactSourceReader{artifacts: artifacts, limits: limits}, nil
}

func (r *hostArtifactSourceReader) ReadSource(ctx context.Context, source memorykit.Source) (string, error) {
	if source.Kind != "artifact" || !strings.HasPrefix(source.Ref, "artifact:") || source.Ref != strings.TrimSpace(source.Ref) {
		return "", fmt.Errorf("%w: unsupported Host extraction source", memorykit.ErrInvalidMemory)
	}
	artifact, err := r.artifacts.Get(ctx, source.Ref)
	if err != nil {
		return "", err
	}
	if artifact.Ref != source.Ref || !utf8.Valid(artifact.Content) {
		return "", fmt.Errorf("%w: malformed Host extraction artifact", memorykit.ErrInvalidMemory)
	}
	text := string(artifact.Content)
	if utf8.RuneCountInString(text) > r.limits.MaxContentRunes {
		return "", fmt.Errorf("%w: Host extraction artifact exceeds content limit", memorykit.ErrInvalidMemory)
	}
	return text, nil
}

type memoryObserverRunIDKey struct{}

func withMemoryObserverRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, memoryObserverRunIDKey{}, runID)
}

type hostMemoryObserver struct{ runs runkit.Store }

func (o *hostMemoryObserver) RecordMemoryEvent(ctx context.Context, event string, payload map[string]any) {
	runID, _ := ctx.Value(memoryObserverRunIDKey{}).(string)
	if strings.TrimSpace(runID) == "" || o == nil || o.runs == nil {
		return
	}
	metadata := make(map[string]any)
	for _, key := range []string{"memory.policy_version", "memory.item_count", "memory.ids", "memory.degraded_channels"} {
		if value, ok := payload[key]; ok {
			metadata[key] = value
		}
	}
	_ = o.runs.AppendEvent(ctx, runkit.RunEvent{RunID: runID, Type: event, Metadata: metadata})
}

func validMemoryRuntimeIdentity(value string, maxRunes int) bool {
	return value != "" && value == strings.TrimSpace(value) && utf8.RuneCountInString(value) <= maxRunes &&
		strings.IndexFunc(value, func(r rune) bool { return r <= 0x1f || r == 0x7f }) < 0
}
