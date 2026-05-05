package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/ports"
	goagentadapter "github.com/eruca/llmkit/adapters/goagent"
	"github.com/eruca/llmkit/llmkit"
	"github.com/eruca/workflowkit"
	"github.com/eruca/workflowkit/agentstep"
)

type Config struct {
	LLMKitHome string
}

type Task struct {
	ID    string
	Input string
}

type Approval struct {
	ApprovedBy string
	Note       string
}

type Artifact struct {
	Ref     string
	Content string
}

type Runtime struct {
	Artifacts *MemoryArtifactStore
	AgentRuns *MemoryAgentRunStore

	store    workflowkit.Store
	executor *workflowkit.Executor
}

func NewRuntime(config Config) (*Runtime, error) {
	if strings.TrimSpace(config.LLMKitHome) == "" {
		return nil, fmt.Errorf("LLMKitHome is required")
	}
	artifacts := NewMemoryArtifactStore()
	agentRuns := NewMemoryAgentRunStore()
	runtime := &Runtime{
		Artifacts: artifacts,
		AgentRuns: agentRuns,
		store:     workflowkit.NewMemoryStore(),
	}
	runtime.executor = workflowkit.NewExecutor(runtime.store, []workflowkit.Step{
		ingestStep{artifacts: artifacts},
		runtime.agentReviewStep(config.LLMKitHome),
		finalizeStep{artifacts: artifacts},
	})
	return runtime, nil
}

func (r *Runtime) Start(ctx context.Context, task Task) (workflowkit.WorkflowRun, error) {
	if strings.TrimSpace(task.ID) == "" {
		return workflowkit.WorkflowRun{}, fmt.Errorf("task ID is required")
	}
	inputRef := "artifact:" + task.ID + ":input"
	if err := r.Artifacts.Put(ctx, Artifact{Ref: inputRef, Content: task.Input}); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return r.executor.Run(ctx, workflowkit.WorkflowRun{
		ID:       task.ID,
		InputRef: inputRef,
		Metadata: map[string]any{
			"input_ref": inputRef,
		},
	})
}

func (r *Runtime) Approve(ctx context.Context, workflowID string, approval Approval) (workflowkit.WorkflowRun, error) {
	return r.executor.Approve(ctx, workflowID, workflowkit.Approval{
		AuditRef: "audit:" + workflowID + ":approval",
		Metadata: map[string]any{
			"approved_by":   approval.ApprovedBy,
			"approval_note": approval.Note,
		},
	})
}

func (r *Runtime) agentReviewStep(llmkitHome string) workflowkit.Step {
	runner := routingAgentRunner{
		llmkitHome: llmkitHome,
		events:     r.AgentRuns,
	}
	return agentstep.New("agent_review", runner, func(run workflowkit.WorkflowRun) agentcore.RunRequest {
		return agentcore.RunRequest{
			Input: "Review input artifact " + run.InputRef,
			Metadata: map[string]any{
				"workflow_id": run.ID,
			},
		}
	}, agentstep.WithResultMapper(func(run workflowkit.WorkflowRun, result *agentcore.RunResult) workflowkit.StepResult {
		outputRef := "artifact:" + run.ID + ":agent-output"
		if result != nil {
			_ = r.Artifacts.Put(context.Background(), Artifact{Ref: outputRef, Content: result.Content})
		}
		agentRunID := ""
		if result != nil {
			agentRunID = result.RunID.String()
		}
		return workflowkit.StepResult{
			Status:        workflowkit.StatusWaitingApproval,
			OutputRef:     outputRef,
			AgentRunID:    agentRunID,
			ApprovalRef:   "approval:" + run.ID,
			WaitingReason: "operator approval required before finalizing host runtime output",
			Metadata: map[string]any{
				"agent_output_ref": outputRef,
			},
		}
	}))
}

type ingestStep struct {
	artifacts *MemoryArtifactStore
}

func (s ingestStep) Name() string {
	return "ingest"
}

func (s ingestStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	artifact, err := s.artifacts.Get(ctx, run.InputRef)
	if err != nil {
		return workflowkit.StepResult{}, err
	}
	return workflowkit.StepResult{
		OutputRef: artifact.Ref,
		Metadata: map[string]any{
			"input_chars": len(artifact.Content),
		},
	}, nil
}

type finalizeStep struct {
	artifacts *MemoryArtifactStore
}

func (s finalizeStep) Name() string {
	return "finalize"
}

func (s finalizeStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	approvedBy, _ := run.Metadata["approved_by"].(string)
	if approvedBy == "" {
		approvedBy = "unknown"
	}
	source, err := s.artifacts.Get(ctx, run.OutputRef)
	if err != nil {
		return workflowkit.StepResult{}, err
	}
	outputRef := "artifact:" + run.ID + ":final"
	if err := s.artifacts.Put(ctx, Artifact{
		Ref:     outputRef,
		Content: source.Content + "\n\napproved by " + approvedBy,
	}); err != nil {
		return workflowkit.StepResult{}, err
	}
	return workflowkit.StepResult{
		Status:    workflowkit.StatusSucceeded,
		OutputRef: outputRef,
		AuditRef:  run.AuditRef,
	}, nil
}

type routingAgentRunner struct {
	llmkitHome string
	events     *MemoryAgentRunStore
}

func (r routingAgentRunner) RunDetailed(ctx context.Context, req agentcore.RunRequest) (*agentcore.RunResult, error) {
	workflowID, _ := req.Metadata["workflow_id"].(string)
	if workflowID == "" {
		return nil, fmt.Errorf("workflow_id metadata is required")
	}

	recorder, err := llmkit.NewJSONLRecorder(r.llmkitHome)
	if err != nil {
		return nil, err
	}
	client := goagentadapter.NewClient(goagentadapter.Config{
		Candidates: []llmkit.Candidate{
			{
				Model: llmkit.ModelCapability{
					Alias:              "local-free",
					Provider:           "local",
					IsLocal:            true,
					CapabilityLevel:    llmkit.CapabilitySimple,
					ContextWindowClass: llmkit.ContextMedium,
					PriceClass:         llmkit.PriceFree,
					LatencyClass:       llmkit.LatencyFastClass,
				},
				AccountAlias: "local-dev",
			},
			{
				Model: llmkit.ModelCapability{
					Alias:              "cloud-advanced",
					Provider:           "openai",
					CapabilityLevel:    llmkit.CapabilityAdvanced,
					ContextWindowClass: llmkit.ContextLong,
					PriceClass:         llmkit.PriceHigh,
					LatencyClass:       llmkit.LatencyNormalClass,
				},
				AccountAlias: "cloud-prod",
			},
		},
		Providers: map[string]goagentadapter.ProviderClient{
			"local-free":     staticProvider{content: "host runtime draft from local-free"},
			"cloud-advanced": staticProvider{content: "host runtime draft from cloud-advanced"},
		},
		ProfileProvider: func(context.Context, ports.ChatRequest) llmkit.TaskProfile {
			profile := llmkit.DefaultTaskProfile()
			profile.Source = llmkit.ProfileSourceHost
			profile.TaskType = "host_runtime_review"
			profile.Complexity = llmkit.ComplexitySimple
			profile.FailureCost = llmkit.FailureCostLow
			profile.Privacy = llmkit.PrivacyLocalPreferred
			return profile
		},
		RouteMetadataProvider: func(context.Context, ports.ChatRequest) goagentadapter.RouteMetadata {
			return goagentadapter.RouteMetadata{
				RouteID: "route:" + workflowID + ":1",
				TaskID:  workflowID,
				Attempt: 1,
			}
		},
		Recorder:       recorder,
		RecordOutcomes: true,
	})
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(client),
		agentcore.WithEventSink(r.events),
	)
	if err != nil {
		return nil, err
	}
	return agent.RunDetailed(ctx, req)
}

type staticProvider struct {
	content string
}

func (p staticProvider) Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
	return &ports.ChatResponse{
		Content: p.content,
		Usage: ports.Usage{
			InputTokens:  5,
			OutputTokens: 7,
		},
	}, nil
}

type MemoryArtifactStore struct {
	mu        sync.RWMutex
	artifacts map[string]Artifact
}

func NewMemoryArtifactStore() *MemoryArtifactStore {
	return &MemoryArtifactStore{artifacts: map[string]Artifact{}}
}

func (s *MemoryArtifactStore) Put(ctx context.Context, artifact Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(artifact.Ref) == "" {
		return fmt.Errorf("artifact ref is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts[artifact.Ref] = artifact
	return nil
}

func (s *MemoryArtifactStore) Get(ctx context.Context, ref string) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	artifact, ok := s.artifacts[ref]
	if !ok {
		return Artifact{}, fmt.Errorf("artifact %q not found", ref)
	}
	return artifact, nil
}

type MemoryAgentRunStore struct {
	mu     sync.RWMutex
	events map[string][]agentcore.Event
}

func NewMemoryAgentRunStore() *MemoryAgentRunStore {
	return &MemoryAgentRunStore{events: map[string][]agentcore.Event{}}
}

func (s *MemoryAgentRunStore) Emit(ctx context.Context, event agentcore.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := event.RunID.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[key] = append(s.events[key], event)
	return nil
}

func (s *MemoryAgentRunStore) Events(agentRunID string) []agentcore.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]agentcore.Event(nil), s.events[agentRunID]...)
}
