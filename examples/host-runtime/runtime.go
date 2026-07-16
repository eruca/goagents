package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
	goagentadapter "github.com/eruca/goagents/llmkit/adapters/goagent"
	"github.com/eruca/goagents/llmkit/llmkit"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
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

type Runtime struct {
	Artifacts artifactkit.Store
	AgentRuns runkit.Store

	store    workflowkit.Store
	executor *workflowkit.Executor
}

func NewRuntime(config Config) (*Runtime, error) {
	if strings.TrimSpace(config.LLMKitHome) == "" {
		return nil, fmt.Errorf("LLMKitHome is required")
	}
	artifacts := artifactkit.NewMemoryStore()
	agentRuns := runkit.NewMemoryStore()
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
	if err := putTextArtifact(ctx, r.Artifacts, inputRef, task.Input); err != nil {
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
		runs:       r.AgentRuns,
	}
	return runtimeAgentStep{
		runner:    runner,
		artifacts: r.Artifacts,
		runs:      r.AgentRuns,
	}
}

type runtimeAgentStep struct {
	runner    routingAgentRunner
	artifacts artifactkit.Store
	runs      runkit.Store
}

func (s runtimeAgentStep) Name() string {
	return "agent_review"
}

func (s runtimeAgentStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	result, err := s.runner.RunDetailed(ctx, agentcore.RunRequest{
		Input: "Review input artifact " + run.InputRef,
		Metadata: map[string]any{
			"workflow_id": run.ID,
		},
	})
	if err != nil {
		return failedAgentStepResult(result, err), err
	}
	if result == nil {
		err := fmt.Errorf("agent returned nil result")
		return failedAgentStepResult(nil, err), err
	}

	outputRef := "artifact:" + run.ID + ":agent-output"
	if err := putTextArtifact(ctx, s.artifacts, outputRef, result.Content); err != nil {
		return failedAgentStepResult(result, err), err
	}
	if err := s.runs.Complete(ctx, result.RunID.String(), runkit.TerminalSummary{
		Status:       runkit.StatusSucceeded,
		ContentRef:   outputRef,
		InputTokens:  result.Usage.InputTokens,
		OutputTokens: result.Usage.OutputTokens,
		LLMCalls:     result.ExecutionSummary.LLMCalls,
		ToolCalls:    result.ExecutionSummary.ToolCalls,
		UsedTools:    result.ExecutionSummary.UsedTools,
	}); err != nil {
		return failedAgentStepResult(result, err), err
	}

	return workflowkit.StepResult{
		Status:        workflowkit.StatusWaitingApproval,
		OutputRef:     outputRef,
		AgentRunID:    result.RunID.String(),
		ApprovalRef:   "approval:" + run.ID,
		WaitingReason: "operator approval required before finalizing host runtime output",
		Metadata: map[string]any{
			"agent_output_ref": outputRef,
		},
	}, nil
}

func failedAgentStepResult(result *agentcore.RunResult, err error) workflowkit.StepResult {
	out := workflowkit.StepResult{
		Status: workflowkit.StatusFailed,
		Error:  err.Error(),
	}
	if result != nil {
		out.AgentRunID = result.RunID.String()
		if result.ExecutionSummary.AbortReason != "" {
			out.Error = result.ExecutionSummary.AbortReason
		}
	}
	return out
}

type ingestStep struct {
	artifacts artifactkit.Store
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
	artifacts artifactkit.Store
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
	content := string(source.Content) + "\n\napproved by " + approvedBy
	if err := putTextArtifact(ctx, s.artifacts, outputRef, content); err != nil {
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
	runs       runkit.Store
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
		agentcore.WithEventSink(runkit.NewGoagentEventSink(r.runs, func(event agentcore.Event) runkit.RunRecord {
			return runkit.RunRecord{
				RunID:      event.RunID.String(),
				WorkflowID: workflowID,
				TaskID:     workflowID,
				Status:     runkit.StatusRunning,
			}
		})),
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

func putTextArtifact(ctx context.Context, store artifactkit.Store, ref string, content string) error {
	return store.Put(ctx, artifactkit.Artifact{
		Ref:         ref,
		Content:     []byte(content),
		ContentType: "text/plain",
	})
}
