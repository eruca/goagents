package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/eruca/artifactkit"
	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/ports"
	goagentadapter "github.com/eruca/llmkit/adapters/goagent"
	"github.com/eruca/llmkit/llmkit"
	"github.com/eruca/runkit"
	"github.com/eruca/workflowkit"
	"github.com/eruca/workflowkit/agentstep"
)

type Config struct {
	LLMKitHome string
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
	ID    string `json:"id"`
	Input string `json:"input"`
}

type approveWorkflowRequest struct {
	ApprovedBy string `json:"approved_by"`
	Note       string `json:"note"`
}

type workflowResponse struct {
	ID            string   `json:"id"`
	Status        string   `json:"status"`
	InputRef      string   `json:"input_ref,omitempty"`
	OutputRef     string   `json:"output_ref,omitempty"`
	AgentRunID    string   `json:"agent_run_id,omitempty"`
	AuditRef      string   `json:"audit_ref,omitempty"`
	ApprovalRef   string   `json:"approval_ref,omitempty"`
	WaitingReason string   `json:"waiting_reason,omitempty"`
	Completed     []string `json:"completed_steps,omitempty"`
}

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
	if strings.TrimSpace(config.LLMKitHome) == "" {
		return nil, fmt.Errorf("LLMKitHome is required")
	}
	artifacts := artifactkit.NewMemoryStore()
	runs := runkit.NewMemoryStore()
	workflows := workflowkit.NewMemoryStore()
	server := &Server{
		artifacts: artifacts,
		runs:      runs,
		workflows: workflows,
		health:    llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{}),
		llmHome:   config.LLMKitHome,
		models:    defaultCandidates(),
	}
	server.executor = workflowkit.NewExecutor(workflows, []workflowkit.Step{
		ingestStep{artifacts: artifacts},
		server.agentStep(),
		finalizeStep{artifacts: artifacts},
	})
	return server, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workflows", s.handleCreateWorkflow)
	mux.HandleFunc("GET /workflows/{id}", s.handleGetWorkflow)
	mux.HandleFunc("POST /workflows/{id}/approve", s.handleApproveWorkflow)
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
	inputRef := "artifact:" + req.ID + ":input"
	if err := putTextArtifact(r.Context(), s.artifacts, inputRef, req.Input); err != nil {
		writeError(w, http.StatusInternalServerError, "artifact_error", err.Error())
		return
	}
	run, err := s.executor.Run(r.Context(), workflowkit.WorkflowRun{
		ID:       req.ID,
		InputRef: inputRef,
		Metadata: map[string]any{
			"input_ref": inputRef,
		},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workflow_error", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, workflowToResponse(run))
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	run, err := s.workflows.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowToResponse(run))
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
	writeJSON(w, http.StatusOK, workflowToResponse(run))
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
		return agentcore.RunRequest{
			Input: "Review input artifact " + run.InputRef,
			Metadata: map[string]any{
				"workflow_id": run.ID,
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
			profile := llmkit.DefaultTaskProfile()
			profile.Source = llmkit.ProfileSourceHost
			profile.TaskType = "host_api_review"
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

func workflowToResponse(run workflowkit.WorkflowRun) workflowResponse {
	return workflowResponse{
		ID:            run.ID,
		Status:        string(run.Status),
		InputRef:      run.InputRef,
		OutputRef:     run.OutputRef,
		AgentRunID:    run.AgentRunID,
		AuditRef:      run.AuditRef,
		ApprovalRef:   run.ApprovalRef,
		WaitingReason: run.WaitingReason,
		Completed:     append([]string(nil), run.CompletedSteps...),
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
