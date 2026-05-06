package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/eruca/artifactkit"
	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/ports"
	goagentadapter "github.com/eruca/llmkit/adapters/goagent"
	"github.com/eruca/llmkit/llmkit"
	"github.com/eruca/runkit"
	runsqlite "github.com/eruca/runkit/sqlitestore"
	"github.com/eruca/workflowkit"
	"github.com/eruca/workflowkit/agentstep"
	workflowsqlite "github.com/eruca/workflowkit/sqlitestore"
)

type Config struct {
	RuntimeHome    string
	LLMKitHome     string
	WorkflowDBPath string
	AgentRunDBPath string
	ArtifactRoot   string
}

type Server struct {
	artifacts artifactkit.Store
	runs      runkit.Store
	workflows workflowkit.Store
	executor  *workflowkit.Executor
	health    *llmkit.MemoryHealthStore
	llmHome   string
	models    []llmkit.Candidate
}

type createWorkflowRequest struct {
	ID          string              `json:"id"`
	Input       string              `json:"input"`
	RunMode     string              `json:"run_mode,omitempty"`
	TaskProfile *taskProfileRequest `json:"task_profile,omitempty"`
}

type approveWorkflowRequest struct {
	ApprovedBy string `json:"approved_by"`
	Note       string `json:"note"`
}

type taskProfileRequest struct {
	TaskType         string `json:"task_type,omitempty"`
	Complexity       string `json:"complexity,omitempty"`
	Latency          string `json:"latency,omitempty"`
	FailureCost      string `json:"failure_cost,omitempty"`
	Privacy          string `json:"privacy,omitempty"`
	NeedsReasoning   bool   `json:"needs_reasoning,omitempty"`
	NeedsTools       bool   `json:"needs_tools,omitempty"`
	NeedsJSON        bool   `json:"needs_json,omitempty"`
	NeedsLongContext bool   `json:"needs_long_context,omitempty"`
}

type workflowResponse struct {
	ID            string   `json:"id"`
	Status        string   `json:"status"`
	RunMode       string   `json:"run_mode"`
	InputRef      string   `json:"input_ref,omitempty"`
	OutputRef     string   `json:"output_ref,omitempty"`
	AgentRunID    string   `json:"agent_run_id,omitempty"`
	AuditRef      string   `json:"audit_ref,omitempty"`
	ApprovalRef   string   `json:"approval_ref,omitempty"`
	WaitingReason string   `json:"waiting_reason,omitempty"`
	Completed     []string `json:"completed_steps,omitempty"`
}

type RunMode string

const (
	RunModeSync   RunMode = "sync"
	RunModeQueued RunMode = "queued"
)

type agentRunResponse struct {
	RunID      string                 `json:"run_id"`
	WorkflowID string                 `json:"workflow_id,omitempty"`
	TaskID     string                 `json:"task_id,omitempty"`
	Status     string                 `json:"status"`
	Summary    runkit.TerminalSummary `json:"summary"`
	Events     []runkit.RunEvent      `json:"events,omitempty"`
}

type modelsResponse struct {
	Models []modelResponse               `json:"models"`
	Health llmkit.ProviderHealthSnapshot `json:"health"`
}

type llmRoutesResponse struct {
	WorkflowID string             `json:"workflow_id"`
	Routes     []llmRouteResponse `json:"routes"`
}

type llmRouteResponse struct {
	RouteID               string                   `json:"route_id,omitempty"`
	TaskID                string                   `json:"task_id,omitempty"`
	Attempt               int                      `json:"attempt,omitempty"`
	TaskType              string                   `json:"task_type,omitempty"`
	AccountAlias          string                   `json:"account_alias,omitempty"`
	ModelAlias            string                   `json:"model_alias,omitempty"`
	Provider              string                   `json:"provider,omitempty"`
	Selected              bool                     `json:"selected"`
	Reason                string                   `json:"reason,omitempty"`
	Score                 int                      `json:"score,omitempty"`
	ScoreBreakdown        map[string]int           `json:"score_breakdown,omitempty"`
	CandidateModelAliases []string                 `json:"candidate_model_aliases,omitempty"`
	Outcome               *llmRouteOutcomeResponse `json:"outcome,omitempty"`
}

type llmRouteOutcomeResponse struct {
	Success        bool   `json:"success"`
	ErrorCode      string `json:"error_code,omitempty"`
	LatencyMillis  int    `json:"latency_ms,omitempty"`
	InputTokens    int    `json:"input_tokens,omitempty"`
	OutputTokens   int    `json:"output_tokens,omitempty"`
	EstimatedCents int    `json:"estimated_cents,omitempty"`
}

type modelResponse struct {
	Alias        string `json:"alias"`
	Provider     string `json:"provider"`
	AccountAlias string `json:"account_alias"`
	IsLocal      bool   `json:"is_local"`
	PriceClass   string `json:"price_class"`
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func NewServer(config Config) (*Server, error) {
	resolved, err := resolveRuntimeConfig(config)
	if err != nil {
		return nil, err
	}
	artifacts, err := artifactkit.NewFileStore(resolved.ArtifactRoot)
	if err != nil {
		return nil, err
	}
	runs, err := runsqlite.Open(resolved.AgentRunDBPath)
	if err != nil {
		return nil, err
	}
	workflows, err := workflowsqlite.Open(resolved.WorkflowDBPath)
	if err != nil {
		_ = runs.Close()
		return nil, err
	}
	server := &Server{
		artifacts: artifacts,
		runs:      runs,
		workflows: workflows,
		health:    llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{}),
		llmHome:   resolved.LLMKitHome,
		models:    defaultCandidates(),
	}
	server.executor = workflowkit.NewExecutor(workflows, []workflowkit.Step{
		ingestStep{artifacts: artifacts},
		server.agentStep(),
		finalizeStep{artifacts: artifacts},
	})
	return server, nil
}

func resolveRuntimeConfig(config Config) (Config, error) {
	if strings.TrimSpace(config.RuntimeHome) == "" {
		temp, err := os.MkdirTemp("", "goagents-host-api-*")
		if err != nil {
			return Config{}, err
		}
		config.RuntimeHome = temp
	}
	runtimeHome := filepath.Clean(config.RuntimeHome)
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(config.WorkflowDBPath) == "" {
		config.WorkflowDBPath = filepath.Join(runtimeHome, "workflow.db")
	}
	if strings.TrimSpace(config.AgentRunDBPath) == "" {
		config.AgentRunDBPath = filepath.Join(runtimeHome, "agent-runs.db")
	}
	if strings.TrimSpace(config.ArtifactRoot) == "" {
		config.ArtifactRoot = filepath.Join(runtimeHome, "artifacts")
	}
	if strings.TrimSpace(config.LLMKitHome) == "" {
		config.LLMKitHome = filepath.Join(runtimeHome, ".llmkit")
	}
	return config, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workflows", s.handleCreateWorkflow)
	mux.HandleFunc("GET /workflows/{id}", s.handleGetWorkflow)
	mux.HandleFunc("POST /workflows/{id}/approve", s.handleApproveWorkflow)
	mux.HandleFunc("GET /workflows/{id}/llm-routes", s.handleGetWorkflowLLMRoutes)
	mux.HandleFunc("GET /agent-runs/{id}", s.handleGetAgentRun)
	mux.HandleFunc("GET /llmkit/models", s.handleModels)
	return mux
}

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req createWorkflowRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "id is required")
		return
	}
	runMode, ok := parseRunMode(req.RunMode)
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported_run_mode", "only sync run_mode is supported")
		return
	}
	profile, err := parseTaskProfile(req.TaskProfile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_task_profile", err.Error())
		return
	}
	inputRef := "artifact:" + req.ID + ":input"
	if err := putTextArtifact(r.Context(), s.artifacts, inputRef, req.Input); err != nil {
		writeError(w, http.StatusInternalServerError, "artifact_error", err.Error())
		return
	}
	run, err := s.executor.Run(r.Context(), workflowkit.WorkflowRun{
		ID:       req.ID,
		InputRef: inputRef,
		Metadata: map[string]any{
			"input_ref":    inputRef,
			"task_profile": profile,
		},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workflow_error", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, workflowToResponse(run, runMode))
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	run, err := s.workflows.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowToResponse(run, RunModeSync))
}

func (s *Server) handleApproveWorkflow(w http.ResponseWriter, r *http.Request) {
	var req approveWorkflowRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	run, err := s.executor.Approve(r.Context(), id, workflowkit.Approval{
		AuditRef: "audit:" + id + ":approval",
		Metadata: map[string]any{
			"approved_by":   req.ApprovedBy,
			"approval_note": req.Note,
		},
	})
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowToResponse(run, RunModeSync))
}

func (s *Server) handleGetWorkflowLLMRoutes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.workflows.Get(r.Context(), id); err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	records, err := llmkit.ReadRouteAudits(s.llmHome, llmkit.AuditFilter{TaskID: id})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "llm_audit_error", err.Error())
		return
	}
	routes := make([]llmRouteResponse, 0, len(records))
	for _, record := range records {
		routes = append(routes, llmRouteToResponse(record))
	}
	writeJSON(w, http.StatusOK, llmRoutesResponse{
		WorkflowID: id,
		Routes:     routes,
	})
}

func (s *Server) handleGetAgentRun(w http.ResponseWriter, r *http.Request) {
	record, err := s.runs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, runkit.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "agent run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "run_error", err.Error())
		return
	}
	events, err := s.runs.Events(r.Context(), record.RunID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "run_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, agentRunResponse{
		RunID:      record.RunID,
		WorkflowID: record.WorkflowID,
		TaskID:     record.TaskID,
		Status:     string(record.Status),
		Summary:    record.Summary,
		Events:     events,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := make([]modelResponse, 0, len(s.models))
	for _, candidate := range s.models {
		models = append(models, modelResponse{
			Alias:        candidate.Model.Alias,
			Provider:     candidate.Model.Provider,
			AccountAlias: candidate.AccountAlias,
			IsLocal:      candidate.Model.IsLocal,
			PriceClass:   string(candidate.Model.PriceClass),
		})
	}
	writeJSON(w, http.StatusOK, modelsResponse{
		Models: models,
		Health: s.health.Snapshot(),
	})
}

func (s *Server) agentStep() workflowkit.Step {
	runner := routingAgentRunner{
		llmkitHome: s.llmHome,
		runs:       s.runs,
		health:     s.health,
		candidates: s.models,
	}
	return agentstep.New("agent_review", runner, func(run workflowkit.WorkflowRun) agentcore.RunRequest {
		profile := taskProfileFromMetadata(run.Metadata["task_profile"])
		return agentcore.RunRequest{
			Input: "Review input artifact " + run.InputRef,
			Metadata: map[string]any{
				"workflow_id":  run.ID,
				"task_profile": profile,
			},
		}
	}, agentstep.WithResultMapper(func(run workflowkit.WorkflowRun, result *agentcore.RunResult) workflowkit.StepResult {
		outputRef := "artifact:" + run.ID + ":agent-output"
		agentRunID := ""
		if result != nil {
			agentRunID = result.RunID.String()
			_ = putTextArtifact(context.Background(), s.artifacts, outputRef, result.Content)
			_ = s.runs.Complete(context.Background(), result.RunID.String(), runkit.TerminalSummary{
				Status:       runkit.StatusSucceeded,
				ContentRef:   outputRef,
				InputTokens:  result.Usage.InputTokens,
				OutputTokens: result.Usage.OutputTokens,
				LLMCalls:     result.ExecutionSummary.LLMCalls,
				ToolCalls:    result.ExecutionSummary.ToolCalls,
				UsedTools:    result.ExecutionSummary.UsedTools,
			})
		}
		return workflowkit.StepResult{
			Status:        workflowkit.StatusWaitingApproval,
			OutputRef:     outputRef,
			AgentRunID:    agentRunID,
			ApprovalRef:   "approval:" + run.ID,
			WaitingReason: "operator approval required before finalizing host API output",
			Metadata: map[string]any{
				"agent_output_ref": outputRef,
			},
		}
	}))
}

type routingAgentRunner struct {
	llmkitHome string
	runs       runkit.Store
	health     llmkit.HealthStore
	candidates []llmkit.Candidate
}

func (r routingAgentRunner) RunDetailed(ctx context.Context, req agentcore.RunRequest) (*agentcore.RunResult, error) {
	workflowID, _ := req.Metadata["workflow_id"].(string)
	if workflowID == "" {
		return nil, fmt.Errorf("workflow_id metadata is required")
	}
	profile := taskProfileFromMetadata(req.Metadata["task_profile"])
	recorder, err := llmkit.NewJSONLRecorder(r.llmkitHome)
	if err != nil {
		return nil, err
	}
	client := goagentadapter.NewClient(goagentadapter.Config{
		Candidates: r.candidates,
		Providers: map[string]goagentadapter.ProviderClient{
			"local-free":     staticProvider{content: "host API draft from local-free"},
			"cloud-advanced": staticProvider{content: "host API draft from cloud-advanced"},
		},
		ProfileProvider: func(context.Context, ports.ChatRequest) llmkit.TaskProfile {
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
		HealthStore:    r.health,
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

func (p staticProvider) Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
	return &ports.ChatResponse{
		Content: p.content,
		Usage: ports.Usage{
			InputTokens:  5,
			OutputTokens: 7,
		},
	}, nil
}

func defaultCandidates() []llmkit.Candidate {
	return []llmkit.Candidate{
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
	}
}

func workflowToResponse(run workflowkit.WorkflowRun, runMode RunMode) workflowResponse {
	if runMode == "" {
		runMode = RunModeSync
	}
	return workflowResponse{
		ID:            run.ID,
		Status:        string(run.Status),
		RunMode:       string(runMode),
		InputRef:      run.InputRef,
		OutputRef:     run.OutputRef,
		AgentRunID:    run.AgentRunID,
		AuditRef:      run.AuditRef,
		ApprovalRef:   run.ApprovalRef,
		WaitingReason: run.WaitingReason,
		Completed:     append([]string(nil), run.CompletedSteps...),
	}
}

func llmRouteToResponse(record llmkit.RouteAuditRecord) llmRouteResponse {
	route := record.Route
	response := llmRouteResponse{
		RouteID:               route.RouteID,
		TaskID:                route.TaskID,
		Attempt:               route.Attempt,
		TaskType:              route.TaskType,
		AccountAlias:          route.AccountAlias,
		ModelAlias:            route.ModelAlias,
		Provider:              route.Provider,
		Selected:              route.Selected,
		Reason:                route.Reason,
		Score:                 route.Score,
		ScoreBreakdown:        copyScoreBreakdown(route.ScoreBreakdown),
		CandidateModelAliases: append([]string(nil), route.CandidateModelAliases...),
	}
	if record.Outcome != nil {
		outcome := record.Outcome
		response.Outcome = &llmRouteOutcomeResponse{
			Success:        outcome.Success,
			ErrorCode:      outcome.ErrorCode,
			LatencyMillis:  outcome.LatencyMillis,
			InputTokens:    outcome.InputTokens,
			OutputTokens:   outcome.OutputTokens,
			EstimatedCents: outcome.EstimatedCents,
		}
	}
	return response
}

func copyScoreBreakdown(values map[string]int) map[string]int {
	if values == nil {
		return nil
	}
	copied := make(map[string]int, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func parseTaskProfile(req *taskProfileRequest) (llmkit.TaskProfile, error) {
	profile := defaultHostTaskProfile()
	if req == nil {
		return profile, nil
	}
	if strings.TrimSpace(req.TaskType) != "" {
		profile.TaskType = strings.TrimSpace(req.TaskType)
	}
	if strings.TrimSpace(req.Complexity) != "" {
		value := llmkit.Complexity(strings.TrimSpace(req.Complexity))
		if value != llmkit.ComplexitySimple && value != llmkit.ComplexityMedium && value != llmkit.ComplexityHard {
			return llmkit.TaskProfile{}, fmt.Errorf("unsupported complexity %q", req.Complexity)
		}
		profile.Complexity = value
	}
	if strings.TrimSpace(req.Latency) != "" {
		value := llmkit.LatencyRequirement(strings.TrimSpace(req.Latency))
		if value != llmkit.LatencyNone && value != llmkit.LatencyNormal && value != llmkit.LatencyUrgent {
			return llmkit.TaskProfile{}, fmt.Errorf("unsupported latency %q", req.Latency)
		}
		profile.Latency = value
	}
	if strings.TrimSpace(req.FailureCost) != "" {
		value := llmkit.FailureCost(strings.TrimSpace(req.FailureCost))
		if value != llmkit.FailureCostLow && value != llmkit.FailureCostMedium && value != llmkit.FailureCostHigh {
			return llmkit.TaskProfile{}, fmt.Errorf("unsupported failure_cost %q", req.FailureCost)
		}
		profile.FailureCost = value
	}
	if strings.TrimSpace(req.Privacy) != "" {
		value := llmkit.PrivacyLevel(strings.TrimSpace(req.Privacy))
		if value != llmkit.PrivacyLocalPreferred && value != llmkit.PrivacyCloudAllowed && value != llmkit.PrivacyLocalOnly {
			return llmkit.TaskProfile{}, fmt.Errorf("unsupported privacy %q", req.Privacy)
		}
		profile.Privacy = value
	}
	profile.NeedsReasoning = req.NeedsReasoning
	profile.NeedsTools = req.NeedsTools
	profile.NeedsJSON = req.NeedsJSON
	profile.NeedsLongContext = req.NeedsLongContext
	return profile, nil
}

func defaultHostTaskProfile() llmkit.TaskProfile {
	profile := llmkit.DefaultTaskProfile()
	profile.Source = llmkit.ProfileSourceHost
	profile.TaskType = "host_api_review"
	profile.Complexity = llmkit.ComplexitySimple
	profile.FailureCost = llmkit.FailureCostLow
	profile.Privacy = llmkit.PrivacyLocalPreferred
	return profile
}

func taskProfileFromMetadata(value any) llmkit.TaskProfile {
	switch profile := value.(type) {
	case llmkit.TaskProfile:
		return profile
	case map[string]any:
		return taskProfileFromMap(profile)
	default:
		return defaultHostTaskProfile()
	}
}

func taskProfileFromMap(values map[string]any) llmkit.TaskProfile {
	profile := defaultHostTaskProfile()
	if value, ok := values["task_type"].(string); ok && strings.TrimSpace(value) != "" {
		profile.TaskType = strings.TrimSpace(value)
	}
	if value, ok := values["source"].(string); ok && strings.TrimSpace(value) != "" {
		profile.Source = llmkit.ProfileSource(strings.TrimSpace(value))
	}
	if value, ok := values["complexity"].(string); ok && strings.TrimSpace(value) != "" {
		profile.Complexity = llmkit.Complexity(strings.TrimSpace(value))
	}
	if value, ok := values["latency_requirement"].(string); ok && strings.TrimSpace(value) != "" {
		profile.Latency = llmkit.LatencyRequirement(strings.TrimSpace(value))
	}
	if value, ok := values["failure_cost"].(string); ok && strings.TrimSpace(value) != "" {
		profile.FailureCost = llmkit.FailureCost(strings.TrimSpace(value))
	}
	if value, ok := values["privacy_level"].(string); ok && strings.TrimSpace(value) != "" {
		profile.Privacy = llmkit.PrivacyLevel(strings.TrimSpace(value))
	}
	profile.NeedsReasoning, _ = values["needs_reasoning"].(bool)
	profile.NeedsTools, _ = values["needs_tools"].(bool)
	profile.NeedsJSON, _ = values["needs_json"].(bool)
	profile.NeedsLongContext, _ = values["needs_long_context"].(bool)
	return profile
}

func parseRunMode(value string) (RunMode, bool) {
	switch RunMode(strings.TrimSpace(value)) {
	case "", RunModeSync:
		return RunModeSync, true
	case RunModeQueued:
		return RunModeQueued, false
	default:
		return "", false
	}
}

func putTextArtifact(ctx context.Context, store artifactkit.Store, ref string, content string) error {
	return store.Put(ctx, artifactkit.Artifact{
		Ref:         ref,
		Content:     []byte(content),
		ContentType: "text/plain",
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func writeWorkflowStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, workflowkit.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "workflow not found")
		return
	}
	writeError(w, http.StatusBadRequest, "workflow_error", err.Error())
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: code, Message: message})
}
