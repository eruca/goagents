package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
	"github.com/eruca/goagents/llmkit"
	goagentadapter "github.com/eruca/goagents/llmkit/adapters/goagent"
	memoryagentadapter "github.com/eruca/goagents/memorykit/agentadapter"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/skillkit"
	skillagentadapter "github.com/eruca/goagents/skillkit/agentadapter"
	"github.com/eruca/goagents/workflowkit"
)

func (s *Server) agentStep() workflowkit.Step {
	runner := routingAgentRunner{
		llmkitHome: s.llmHome,
		runs:       s.runs,
		artifacts:  s.artifacts,
		health:     s.health,
		candidates: s.models,
		statsProvider: func(ctx context.Context) (*llmkit.ModelStats, error) {
			return llmkit.RefreshModelStats(s.llmHome)
		},
		providers:        s.providers,
		skillCatalog:     s.skillCatalog,
		skillGateContext: s.skillGateContext,
		memory:           s.memory,
	}
	return hostAgentStep{
		runner:    runner,
		artifacts: s.artifacts,
		runs:      s.runs,
		approvals: s.agentApprovals,
	}
}

type hostAgentStep struct {
	runner    routingAgentRunner
	artifacts artifactkit.Store
	runs      runkit.Store
	approvals *hostAgentApprovalService
}

func (s hostAgentStep) Name() string {
	return "agent_review"
}

func (s hostAgentStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	request, err := buildAgentRequestForWorkflow(run, s.runner.memory != nil)
	if err != nil {
		return workflowkit.StepResult{Status: workflowkit.StatusFailed, Error: err.Error()}, err
	}
	if s.runner.memory != nil {
		request.Metadata[hostWorkflowInputRefKey] = run.InputRef
	}
	result, err := s.runner.RunDetailed(ctx, request)
	if err != nil {
		if errors.Is(err, agentcore.ErrApprovalPending) && result != nil && result.Interruption != nil && s.approvals != nil {
			approval, saveErr := s.approvals.SavePending(ctx, run.ID, result.Interruption.Checkpoint)
			if saveErr != nil {
				if completeErr := completeFailedAgentRun(ctx, s.runs, result, saveErr); completeErr != nil {
					saveErr = errors.Join(saveErr, fmt.Errorf("complete failed agent run: %w", completeErr))
				}
				return failedAgentStepResult(result, saveErr), saveErr
			}
			return workflowkit.StepResult{
				Status:        workflowkit.StatusWaitingApproval,
				AgentRunID:    result.RunID.String(),
				ApprovalRef:   "agent-approval:" + approval.CheckpointID,
				WaitingReason: "operator tool approval required before record_review executes",
				Metadata:      approval.workflowMetadata(),
			}, nil
		}
		if completeErr := completeFailedAgentRun(ctx, s.runs, result, err); completeErr != nil {
			err = errors.Join(err, fmt.Errorf("complete failed agent run: %w", completeErr))
		}
		return failedAgentStepResult(result, err), err
	}
	if result == nil {
		err := fmt.Errorf("agent returned nil result")
		return failedAgentStepResult(nil, err), err
	}

	outputRef := "artifact:" + result.RunID.String() + ":agent-output"
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
	s.enqueueAgentExtraction(ctx, run, outputRef, result.RunID.String())

	return workflowkit.StepResult{
		Status:        workflowkit.StatusWaitingApproval,
		OutputRef:     outputRef,
		AgentRunID:    result.RunID.String(),
		ApprovalRef:   "approval:" + run.ID,
		WaitingReason: "operator approval required before finalizing host API output",
		Metadata: map[string]any{
			"agent_output_ref": outputRef,
		},
	}, nil
}

func buildAgentRequestForWorkflow(run workflowkit.WorkflowRun, memoryEnabled bool) (agentcore.RunRequest, error) {
	profile := taskProfileFromMetadata(run.Metadata["task_profile"])
	request := agentcore.RunRequest{
		Input:    "Review input artifact " + run.InputRef,
		Metadata: map[string]any{"workflow_id": run.ID, "task_profile": profile},
	}
	if skillRefs, exists := run.Metadata["skill_refs"]; exists {
		request.Metadata["skill_refs"] = skillRefs
	}
	writeAllowed := profile.NeedsTools
	if memoryEnabled {
		scope, err := resolveTrustedMemoryScope(run.Metadata)
		if err != nil {
			return agentcore.RunRequest{}, err
		}
		userID := run.Metadata[memoryUserMetadataKey].(string)
		writeIntent := run.Metadata[memoryWriteIntentMetadataKey].(bool)
		request.UserID = userID
		request.SessionID = run.ID
		request.PolicyContext.TenantID = scope.TenantID
		request.PolicyContext.Labels = map[string]string{"project_id": scope.SubjectID}
		for key := range trustedMemoryMetadataKeys {
			request.Metadata[key] = run.Metadata[key]
		}
		encodedProfile, err := json.Marshal(profile)
		if err != nil {
			return agentcore.RunRequest{}, fmt.Errorf("encode trusted task profile: %w", err)
		}
		var safeProfile map[string]any
		if err := json.Unmarshal(encodedProfile, &safeProfile); err != nil {
			return agentcore.RunRequest{}, fmt.Errorf("decode trusted task profile: %w", err)
		}
		request.Metadata["task_profile"] = safeProfile
		writeAllowed = writeAllowed || writeIntent
	}
	if writeAllowed {
		request.AllowedPermissions = []policy.Permission{policy.PermissionWrite}
	}
	return request, nil
}

func (s hostAgentStep) enqueueAgentExtraction(ctx context.Context, run workflowkit.WorkflowRun, outputRef, agentRunID string) {
	if s.runner.memory == nil {
		return
	}
	scope, err := resolveTrustedMemoryScope(run.Metadata)
	if err == nil {
		err = s.runner.memory.EnqueueExtraction(ctx, scope, outputRef, agentRunID)
	}
	if err != nil {
		s.runner.memory.observer.RecordMemoryEvent(withMemoryObserverRunID(ctx, agentRunID), "memory.degraded", map[string]any{
			"memory.degraded_channels": []string{"extraction_enqueue"},
		})
	}
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

func completeFailedAgentRun(ctx context.Context, store runkit.Store, result *agentcore.RunResult, cause error) error {
	if result == nil {
		return nil
	}
	abortReason := cause.Error()
	if result.ExecutionSummary.AbortReason != "" {
		abortReason = result.ExecutionSummary.AbortReason
	}
	return store.Complete(ctx, result.RunID.String(), runkit.TerminalSummary{
		Status:       runkit.StatusFailed,
		AbortReason:  abortReason,
		InputTokens:  result.Usage.InputTokens,
		OutputTokens: result.Usage.OutputTokens,
		LLMCalls:     result.ExecutionSummary.LLMCalls,
		ToolCalls:    result.ExecutionSummary.ToolCalls,
		UsedTools:    result.ExecutionSummary.UsedTools,
	})
}

type routingAgentRunner struct {
	llmkitHome       string
	runs             runkit.Store
	artifacts        artifactkit.Store
	health           llmkit.HealthStore
	candidates       []llmkit.Candidate
	statsProvider    goagentadapter.ModelStatsProvider
	providers        map[string]goagentadapter.ProviderClient
	skillCatalog     *skillkit.Catalog
	skillGateContext skillkit.GateContext
	memory           *memoryRuntime
}

func (r routingAgentRunner) RunDetailed(ctx context.Context, req agentcore.RunRequest) (*agentcore.RunResult, error) {
	workflowID, _ := req.Metadata["workflow_id"].(string)
	if workflowID == "" {
		return nil, fmt.Errorf("workflow_id metadata is required")
	}
	if req.RunID.IsZero() {
		req.RunID = agentcore.NewRunID()
	}
	profile := taskProfileFromMetadata(req.Metadata["task_profile"])
	activation, err := r.activateSkillRefs(req.Metadata)
	if err != nil {
		return nil, err
	}
	agent, err := r.newAgent(workflowID, req.RunID.String(), 0, profile, activation)
	if err != nil {
		return nil, err
	}
	if r.memory != nil {
		ctx = withMemoryObserverRunID(ctx, req.RunID.String())
	}
	return agent.RunDetailed(ctx, req)
}

// ResumeDetailed rebuilds host-owned skills and the tool registry from
// checkpoint request metadata before validating the exact pending tool batch.
func (r routingAgentRunner) ResumeDetailed(ctx context.Context, checkpoint agentcore.RunCheckpoint, resolutions []agentcore.ToolApprovalResolution) (*agentcore.RunResult, error) {
	workflowID, _ := checkpoint.Request.Metadata["workflow_id"].(string)
	if workflowID == "" {
		return nil, fmt.Errorf("workflow_id metadata is required in checkpoint")
	}
	profile := taskProfileFromMetadata(checkpoint.Request.Metadata["task_profile"])
	activation, err := r.activateSkillRefs(checkpoint.Request.Metadata)
	if err != nil {
		return nil, err
	}
	agent, err := r.newAgent(workflowID, checkpoint.RunID, checkpoint.LLMCalls, profile, activation)
	if err != nil {
		return nil, err
	}
	if r.memory != nil {
		ctx = withMemoryObserverRunID(ctx, checkpoint.RunID)
	}
	return agent.ResumeDetailed(ctx, checkpoint, resolutions)
}

func (r routingAgentRunner) newAgent(workflowID, agentRunID string, completedLLMCalls int, profile llmkit.TaskProfile, activation *skillkit.Activation) (*agentcore.Agent, error) {
	recorder, err := llmkit.NewJSONLRecorder(r.llmkitHome)
	if err != nil {
		return nil, err
	}
	var routeSequence atomic.Int64
	routeSequence.Store(int64(completedLLMCalls))
	client := goagentadapter.NewClient(goagentadapter.Config{
		Candidates: r.candidates,
		Providers:  r.providers,
		ProfileProvider: func(context.Context, ports.ChatRequest) llmkit.TaskProfile {
			return profile
		},
		RouteMetadataProvider: func(context.Context, ports.ChatRequest) goagentadapter.RouteMetadata {
			// RouteID must identify one logical LLM call. AgentRunID separates
			// workflow requeues, while the persisted call count keeps resumed
			// tool-approval runs from reusing an earlier call's audit identity.
			call := routeSequence.Add(1)
			return goagentadapter.RouteMetadata{
				RouteID: "route:" + workflowID + ":" + agentRunID + ":" + strconv.FormatInt(call, 10),
				TaskID:  workflowID,
				Attempt: 1,
			}
		},
		Recorder:           recorder,
		RecordOutcomes:     true,
		ModelStatsProvider: r.statsProvider,
		HealthStore:        r.health,
	})
	options := []agentcore.Option{
		agentcore.WithLLM(client),
		agentcore.WithEventSink(runkit.NewGoagentEventSink(r.runs, func(event agentcore.Event) runkit.RunRecord {
			return runkit.RunRecord{
				RunID:      event.RunID.String(),
				WorkflowID: workflowID,
				TaskID:     workflowID,
				Status:     runkit.StatusRunning,
			}
		})),
	}
	if activation != nil {
		options = append(options, agentcore.WithSkillProvider(skillagentadapter.Provider{
			Resolve: func(context.Context, agentcore.RunRequest) (*skillkit.Activation, error) {
				return activation, nil
			},
		}))
	}
	options = append(options, r.toolOptions(profile)...)
	if r.memory != nil {
		options = append(options,
			agentcore.WithPromptBlocks([]prompt.Block{memoryagentadapter.GuardPromptBlock()}),
			agentcore.WithContextProjector(r.memory.Projector()),
			agentcore.WithToolProvider(r.memory.ToolProvider()),
			agentcore.WithOutputValidator(r.memory.ToolProvider().OutputValidator()),
		)
	}
	return agentcore.NewAgent(options...)
}

// activateSkillRefs only accepts the host's complete, persisted name@digest
// maps. A malformed present value is never treated as an empty selection.
func (r routingAgentRunner) activateSkillRefs(metadata map[string]any) (*skillkit.Activation, error) {
	refs, present, err := persistedSkillRefs(metadata)
	if err != nil {
		return nil, fmt.Errorf("invalid persisted skill_refs: %w", err)
	}
	if !present {
		return nil, nil
	}
	if r.skillCatalog == nil {
		return nil, errors.New("skill catalog is unavailable for persisted skill_refs")
	}
	activation, err := r.skillCatalog.Activate(skillkit.ActivationRequest{
		Skills:      refs,
		GateContext: r.skillGateContext,
	})
	if err != nil {
		return nil, fmt.Errorf("activate persisted skill_refs: %w", err)
	}
	return activation, nil
}

func persistedSkillRefs(metadata map[string]any) ([]skillkit.Ref, bool, error) {
	raw, present := metadata["skill_refs"]
	if !present {
		return nil, false, nil
	}

	refs := make([]skillkit.Ref, 0)
	switch values := raw.(type) {
	case []map[string]string:
		for _, value := range values {
			ref, err := persistedSkillRef(value)
			if err != nil {
				return nil, true, err
			}
			refs = append(refs, ref)
		}
	case []any:
		for _, rawValue := range values {
			value, ok := rawValue.(map[string]any)
			if !ok {
				return nil, true, errors.New("each skill reference must be a name/digest object")
			}
			name, nameOK := value["name"].(string)
			digest, digestOK := value["digest"].(string)
			if len(value) != 2 || !nameOK || !digestOK {
				return nil, true, errors.New("each skill reference must contain only string name and digest")
			}
			ref, err := persistedSkillRef(map[string]string{"name": name, "digest": digest})
			if err != nil {
				return nil, true, err
			}
			refs = append(refs, ref)
		}
	default:
		return nil, true, errors.New("skill_refs must be an array of name/digest objects")
	}
	if len(refs) == 0 {
		return nil, true, errors.New("skill_refs must contain at least one complete reference")
	}
	return refs, true, nil
}

func persistedSkillRef(value map[string]string) (skillkit.Ref, error) {
	name, nameOK := value["name"]
	digest, digestOK := value["digest"]
	if len(value) != 2 || !nameOK || !digestOK || strings.TrimSpace(name) == "" || strings.TrimSpace(digest) == "" {
		return skillkit.Ref{}, errors.New("each skill reference must contain only non-empty name and digest")
	}
	return skillkit.Ref{Name: name, Digest: digest}, nil
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

type staticProvider struct {
	content string
}

func (p staticProvider) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	if len(req.Tools) > 0 && !hasToolObservation(req.Messages) {
		return &ports.ChatResponse{
			ToolCalls: []ports.ToolCall{{
				ID:    "call-record-review",
				Name:  recordReviewToolName,
				Input: json.RawMessage(`{}`),
			}},
			Usage: ports.Usage{InputTokens: 5, OutputTokens: 7},
		}, nil
	}
	return &ports.ChatResponse{
		Content: p.content,
		Usage: ports.Usage{
			InputTokens:  5,
			OutputTokens: 7,
		},
	}, nil
}

func hasToolObservation(messages []ports.ChatMessage) bool {
	for _, message := range messages {
		if message.Role == "tool" {
			return true
		}
	}
	return false
}
