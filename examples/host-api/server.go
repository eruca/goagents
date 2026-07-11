package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eruca/artifactkit"
	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/ports"
	goagentadapter "github.com/eruca/llmkit/adapters/goagent"
	"github.com/eruca/llmkit/llmkit"
	"github.com/eruca/runkit"
	"github.com/eruca/runkit/goagentapproval"
	runsqlite "github.com/eruca/runkit/sqlitestore"
	"github.com/eruca/skillkit"
	"github.com/eruca/workflowkit"
	workflowsqlite "github.com/eruca/workflowkit/sqlitestore"
)

type Config struct {
	RuntimeHome           string
	LLMKitHome            string
	WorkflowDBPath        string
	AgentRunDBPath        string
	ArtifactRoot          string
	ApprovalAuthenticator ApprovalAuthenticator
	// AgentApprovalCipher is test-only injection for the host's tool-approval
	// checkpoint encryption boundary. Real local runs lazily use macOS Keychain.
	AgentApprovalCipher goagentapproval.Cipher
	// SkillCatalog is a prebuilt, host-owned snapshot. The HTTP server never
	// discovers roots itself.
	SkillCatalog *skillkit.Catalog
	// SkillGateContext supplies host-owned facts used only to report availability.
	SkillGateContext skillkit.GateContext
}

type Server struct {
	artifacts               artifactkit.Store
	runs                    runkit.Store
	workflows               workflowkit.Store
	queries                 workflowkit.WorkflowQueryStore
	queue                   workflowkit.QueueLeaseStore
	executor                *workflowkit.Executor
	health                  *llmkit.MemoryHealthStore
	llmHome                 string
	models                  []llmkit.Candidate
	providers               map[string]goagentadapter.ProviderClient
	approvalAuthenticator   ApprovalAuthenticator
	agentApprovals          *hostAgentApprovalService
	skillCatalog            *skillkit.Catalog
	skillGateContext        skillkit.GateContext
	worker                  queuedWorkerStatus
	workerCfg               queuedWorkerConfig
	agentApprovalJanitorCfg agentApprovalJanitorConfig
}

type createWorkflowRequest struct {
	ID                string              `json:"id"`
	Input             string              `json:"input"`
	RunMode           string              `json:"run_mode,omitempty"`
	TaskProfilePreset string              `json:"task_profile_preset,omitempty"`
	TaskProfile       *taskProfileRequest `json:"task_profile,omitempty"`
	SkillRefs         []workflowSkillRef  `json:"skill_refs,omitempty"`
}

// workflowSkillRef is the JSON-safe identity of a skill selected for one
// workflow. Digest is optional in a request, but always present after resolve.
type workflowSkillRef struct {
	Name   string `json:"name"`
	Digest string `json:"digest,omitempty"`
}

type approveWorkflowRequest struct {
	Note string `json:"note"`
}

type taskProfileRequest struct {
	TaskType          *string `json:"task_type,omitempty"`
	Complexity        *string `json:"complexity,omitempty"`
	Latency           *string `json:"latency,omitempty"`
	FailureCost       *string `json:"failure_cost,omitempty"`
	Privacy           *string `json:"privacy,omitempty"`
	MaxEstimatedCents *int    `json:"max_estimated_cents,omitempty"`
	NeedsReasoning    *bool   `json:"needs_reasoning,omitempty"`
	NeedsTools        *bool   `json:"needs_tools,omitempty"`
	NeedsJSON         *bool   `json:"needs_json,omitempty"`
	NeedsLongContext  *bool   `json:"needs_long_context,omitempty"`
}

type workflowResponse struct {
	ID            string                 `json:"id"`
	Status        string                 `json:"status"`
	RunMode       string                 `json:"run_mode"`
	SkillRefs     []workflowSkillRef     `json:"skill_refs,omitempty"`
	InputRef      string                 `json:"input_ref,omitempty"`
	OutputRef     string                 `json:"output_ref,omitempty"`
	AgentRunID    string                 `json:"agent_run_id,omitempty"`
	AuditRef      string                 `json:"audit_ref,omitempty"`
	ApprovalRef   string                 `json:"approval_ref,omitempty"`
	WaitingReason string                 `json:"waiting_reason,omitempty"`
	AgentApproval *agentApprovalResponse `json:"agent_approval,omitempty"`
	Completed     []string               `json:"completed_steps,omitempty"`
}

type workflowListResponse struct {
	Workflows []workflowResponse `json:"workflows"`
}

// skillListResponse deliberately exposes only catalog-safe fields. In
// particular, it omits the manifest, instructions, resources, and root paths.
type skillListResponse struct {
	Skills []skillResponse `json:"skills"`
}

type skillResponse struct {
	Name         string                `json:"name"`
	Description  string                `json:"description"`
	Digest       string                `json:"digest"`
	Scope        skillkit.Scope        `json:"scope"`
	Availability skillkit.Availability `json:"availability"`
	Reasons      []skillReasonResponse `json:"reasons"`
}

type skillReasonResponse struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
}

type workflowEventsResponse struct {
	WorkflowID  string                  `json:"workflow_id"`
	Status      string                  `json:"status"`
	RunMode     string                  `json:"run_mode"`
	CurrentStep string                  `json:"current_step,omitempty"`
	Completed   []string                `json:"completed_steps,omitempty"`
	Events      []workflowEventResponse `json:"events"`
}

type workflowEventResponse struct {
	Type          string    `json:"type"`
	Name          string    `json:"name,omitempty"`
	Status        string    `json:"status,omitempty"`
	Attempt       int       `json:"attempt,omitempty"`
	OutputRef     string    `json:"output_ref,omitempty"`
	AgentRunID    string    `json:"agent_run_id,omitempty"`
	AuditRef      string    `json:"audit_ref,omitempty"`
	Error         string    `json:"error,omitempty"`
	ApprovalRef   string    `json:"approval_ref,omitempty"`
	WaitingReason string    `json:"waiting_reason,omitempty"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	EndedAt       time.Time `json:"ended_at,omitempty"`
	FromStatus    string    `json:"from_status,omitempty"`
	ToStatus      string    `json:"to_status,omitempty"`
	At            time.Time `json:"at,omitempty"`
}

type RunMode string

const (
	RunModeSync   RunMode = "sync"
	RunModeQueued RunMode = "queued"
)

const (
	queuedWorkerID                    = "host-api-inprocess-worker"
	defaultQueuedLeaseDuration        = time.Minute
	queuedLeaseDurationEnv            = "HOST_API_QUEUED_LEASE_DURATION"
	defaultAgentApprovalSweepInterval = time.Minute
	agentApprovalSweepIntervalEnv     = "HOST_API_AGENT_APPROVAL_SWEEP_INTERVAL"
	queuedPollInterval                = 100 * time.Millisecond
	minQueuedHeartbeatInterval        = time.Millisecond
	requeueEventsMetadataKey          = "requeue_events"
)

var (
	errWorkflowNotRequeueable    = errors.New("workflow status cannot be requeued")
	errAgentApprovalNotPending   = errors.New("agent tool approval is not pending")
	errAgentApprovalResumeFailed = errors.New("agent tool approval resume failed")
)

type queuedWorkerConfig struct {
	leaseDuration time.Duration
}

type agentApprovalJanitorConfig struct {
	interval time.Duration
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
	Stats  []modelStatsResponse          `json:"stats,omitempty"`
}

type queuedWorkerResponse struct {
	Started                 bool   `json:"started"`
	WorkerID                string `json:"worker_id"`
	ClaimAttempts           int    `json:"claim_attempts"`
	Claimed                 int    `json:"claimed"`
	Completed               int    `json:"completed"`
	Idle                    int    `json:"idle"`
	Errors                  int    `json:"errors"`
	LeaseExtensions         int    `json:"lease_extensions"`
	HeartbeatErrors         int    `json:"heartbeat_errors"`
	LastWorkflowID          string `json:"last_workflow_id,omitempty"`
	LastError               string `json:"last_error,omitempty"`
	LastErrorWorkflowID     string `json:"last_error_workflow_id,omitempty"`
	LastHeartbeatWorkflowID string `json:"last_heartbeat_workflow_id,omitempty"`
	LastHeartbeatError      string `json:"last_heartbeat_error,omitempty"`
}

type queuedWorkerStatus struct {
	mu                      sync.RWMutex
	started                 bool
	workerID                string
	claimAttempts           int
	claimed                 int
	completed               int
	idle                    int
	errors                  int
	leaseExtensions         int
	heartbeatErrors         int
	lastWorkflowID          string
	lastError               string
	lastErrorWorkflowID     string
	lastHeartbeatWorkflowID string
	lastHeartbeatError      string
}

func (s *queuedWorkerStatus) markStarted(workerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	s.workerID = workerID
}

func (s *queuedWorkerStatus) recordClaimAttempt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimAttempts++
}

func (s *queuedWorkerStatus) recordClaimed(workflowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimed++
	s.lastWorkflowID = workflowID
}

func (s *queuedWorkerStatus) recordCompleted(workflowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed++
	s.lastWorkflowID = workflowID
}

func (s *queuedWorkerStatus) recordIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idle++
}

func (s *queuedWorkerStatus) recordError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors++
	s.lastError = err.Error()
}

func (s *queuedWorkerStatus) recordWorkflowError(workflowID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors++
	s.lastWorkflowID = workflowID
	s.lastErrorWorkflowID = workflowID
	s.lastError = err.Error()
}

func (s *queuedWorkerStatus) recordLeaseExtended(workflowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaseExtensions++
	s.lastHeartbeatWorkflowID = workflowID
	s.lastHeartbeatError = ""
}

func (s *queuedWorkerStatus) recordHeartbeatError(workflowID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeatErrors++
	s.lastHeartbeatWorkflowID = workflowID
	s.lastHeartbeatError = err.Error()
}

func (s *queuedWorkerStatus) snapshot() queuedWorkerResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return queuedWorkerResponse{
		Started:                 s.started,
		WorkerID:                s.workerID,
		ClaimAttempts:           s.claimAttempts,
		Claimed:                 s.claimed,
		Completed:               s.completed,
		Idle:                    s.idle,
		Errors:                  s.errors,
		LeaseExtensions:         s.leaseExtensions,
		HeartbeatErrors:         s.heartbeatErrors,
		LastWorkflowID:          s.lastWorkflowID,
		LastError:               s.lastError,
		LastErrorWorkflowID:     s.lastErrorWorkflowID,
		LastHeartbeatWorkflowID: s.lastHeartbeatWorkflowID,
		LastHeartbeatError:      s.lastHeartbeatError,
	}
}

type llmRoutesResponse struct {
	WorkflowID string             `json:"workflow_id"`
	Routes     []llmRouteResponse `json:"routes"`
}

type llmRouteResponse struct {
	RouteID               string                      `json:"route_id,omitempty"`
	TaskID                string                      `json:"task_id,omitempty"`
	Attempt               int                         `json:"attempt,omitempty"`
	TaskType              string                      `json:"task_type,omitempty"`
	TaskProfile           *taskProfileResponse        `json:"task_profile,omitempty"`
	AccountAlias          string                      `json:"account_alias,omitempty"`
	ModelAlias            string                      `json:"model_alias,omitempty"`
	Provider              string                      `json:"provider,omitempty"`
	Selected              bool                        `json:"selected"`
	Reason                string                      `json:"reason,omitempty"`
	Score                 int                         `json:"score,omitempty"`
	ScoreBreakdown        map[string]int              `json:"score_breakdown,omitempty"`
	CandidateModelAliases []string                    `json:"candidate_model_aliases,omitempty"`
	Candidates            []llmRouteCandidateResponse `json:"candidates,omitempty"`
	Outcome               *llmRouteOutcomeResponse    `json:"outcome,omitempty"`
}

type llmRouteCandidateResponse struct {
	Alias          string         `json:"alias,omitempty"`
	AccountAlias   string         `json:"account_alias,omitempty"`
	Available      bool           `json:"available"`
	Score          int            `json:"score,omitempty"`
	ScoreBreakdown map[string]int `json:"score_breakdown,omitempty"`
	Reason         string         `json:"reason,omitempty"`
}

type llmRouteOutcomeResponse struct {
	Success         bool   `json:"success"`
	ErrorCode       string `json:"error_code,omitempty"`
	ErrorClass      string `json:"error_class,omitempty"`
	LatencyMillis   int    `json:"latency_ms,omitempty"`
	InputTokens     int    `json:"input_tokens,omitempty"`
	OutputTokens    int    `json:"output_tokens,omitempty"`
	EstimatedCents  int    `json:"estimated_cents,omitempty"`
	BusinessOutcome string `json:"business_outcome,omitempty"`
	SuccessSignal   string `json:"success_signal,omitempty"`
	FailureReason   string `json:"failure_reason,omitempty"`
}

type taskProfileResponse struct {
	TaskType          string `json:"task_type,omitempty"`
	Complexity        string `json:"complexity,omitempty"`
	Latency           string `json:"latency,omitempty"`
	FailureCost       string `json:"failure_cost,omitempty"`
	Privacy           string `json:"privacy,omitempty"`
	MaxEstimatedCents int    `json:"max_estimated_cents,omitempty"`
	NeedsReasoning    bool   `json:"needs_reasoning,omitempty"`
	NeedsTools        bool   `json:"needs_tools,omitempty"`
	NeedsJSON         bool   `json:"needs_json,omitempty"`
	NeedsLongContext  bool   `json:"needs_long_context,omitempty"`
}

type modelResponse struct {
	Alias        string `json:"alias"`
	Provider     string `json:"provider"`
	AccountAlias string `json:"account_alias"`
	IsLocal      bool   `json:"is_local"`
	PriceClass   string `json:"price_class"`
}

type modelStatsResponse struct {
	TaskType          string  `json:"task_type,omitempty"`
	AccountAlias      string  `json:"account_alias,omitempty"`
	ModelAlias        string  `json:"model_alias,omitempty"`
	Provider          string  `json:"provider,omitempty"`
	RouteAttempts     int     `json:"route_attempts"`
	OutcomeCount      int     `json:"outcome_count"`
	PendingOutcomes   int     `json:"pending_outcomes"`
	Successes         int     `json:"successes"`
	Failures          int     `json:"failures"`
	SuccessRate       float64 `json:"success_rate"`
	FailureRate       float64 `json:"failure_rate"`
	AvgLatencyMillis  int     `json:"avg_latency_ms,omitempty"`
	AvgInputTokens    int     `json:"avg_input_tokens,omitempty"`
	AvgOutputTokens   int     `json:"avg_output_tokens,omitempty"`
	AvgEstimatedCents int     `json:"avg_estimated_cents,omitempty"`
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
	workerCfg, err := loadQueuedWorkerConfig(os.Getenv)
	if err != nil {
		return nil, err
	}
	agentApprovalJanitorCfg, err := loadAgentApprovalJanitorConfig(os.Getenv)
	if err != nil {
		return nil, err
	}
	models, providers, _, err := loadLLMKitComposition(resolved.LLMKitHome)
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
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
	runner := routingAgentRunner{
		llmkitHome: resolved.LLMKitHome,
		runs:       runs,
		artifacts:  artifacts,
		health:     health,
		candidates: models,
		statsProvider: func(ctx context.Context) (*llmkit.ModelStats, error) {
			return llmkit.RefreshModelStats(resolved.LLMKitHome)
		},
		providers: providers,
	}
	agentApprovals, err := newHostAgentApprovalService(runs, config.AgentApprovalCipher, runner)
	if err != nil {
		_ = workflows.Close()
		_ = runs.Close()
		return nil, err
	}
	server := &Server{
		artifacts:               artifacts,
		runs:                    runs,
		workflows:               workflows,
		queries:                 workflows,
		queue:                   workflows,
		health:                  health,
		llmHome:                 resolved.LLMKitHome,
		models:                  models,
		providers:               providers,
		workerCfg:               workerCfg,
		agentApprovalJanitorCfg: agentApprovalJanitorCfg,
		approvalAuthenticator:   config.ApprovalAuthenticator,
		agentApprovals:          agentApprovals,
		skillCatalog:            config.SkillCatalog,
		skillGateContext:        config.SkillGateContext,
	}
	if server.approvalAuthenticator == nil {
		server.approvalAuthenticator = rejectingApprovalAuthenticator{}
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

func loadQueuedWorkerConfig(getenv func(string) string) (queuedWorkerConfig, error) {
	config := queuedWorkerConfig{leaseDuration: defaultQueuedLeaseDuration}
	rawLease := strings.TrimSpace(getenv(queuedLeaseDurationEnv))
	if rawLease == "" {
		return config, nil
	}
	lease, err := time.ParseDuration(rawLease)
	if err != nil {
		return queuedWorkerConfig{}, fmt.Errorf("%s must be a Go duration such as 1m or 500ms: %w", queuedLeaseDurationEnv, err)
	}
	if lease <= 0 {
		return queuedWorkerConfig{}, fmt.Errorf("%s must be greater than zero", queuedLeaseDurationEnv)
	}
	config.leaseDuration = lease
	return config, nil
}

func loadAgentApprovalJanitorConfig(getenv func(string) string) (agentApprovalJanitorConfig, error) {
	config := agentApprovalJanitorConfig{interval: defaultAgentApprovalSweepInterval}
	rawInterval := strings.TrimSpace(getenv(agentApprovalSweepIntervalEnv))
	if rawInterval == "" {
		return config, nil
	}
	interval, err := time.ParseDuration(rawInterval)
	if err != nil {
		return agentApprovalJanitorConfig{}, fmt.Errorf("%s must be a Go duration such as 1m or 500ms: %w", agentApprovalSweepIntervalEnv, err)
	}
	if interval <= 0 {
		return agentApprovalJanitorConfig{}, fmt.Errorf("%s must be greater than zero", agentApprovalSweepIntervalEnv)
	}
	config.interval = interval
	return config, nil
}

func queuedHeartbeatInterval(lease time.Duration) time.Duration {
	interval := lease / 2
	if interval < minQueuedHeartbeatInterval {
		return minQueuedHeartbeatInterval
	}
	return interval
}

func (s *Server) queuedLeaseDuration() time.Duration {
	if s.workerCfg.leaseDuration <= 0 {
		return defaultQueuedLeaseDuration
	}
	return s.workerCfg.leaseDuration
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skills", s.handleListSkills)
	mux.HandleFunc("POST /workflows", s.handleCreateWorkflow)
	mux.HandleFunc("GET /workflows", s.handleListWorkflows)
	mux.HandleFunc("GET /workflows/{id}", s.handleGetWorkflow)
	mux.HandleFunc("GET /workflows/{id}/events", s.handleWorkflowEvents)
	mux.HandleFunc("POST /workflows/{id}/approve", s.handleApproveWorkflow)
	mux.HandleFunc("POST /workflows/{id}/agent-approve", s.handleApproveAgentTool)
	mux.HandleFunc("POST /workflows/{id}/requeue", s.handleRequeueWorkflow)
	mux.HandleFunc("GET /workflows/{id}/llm-routes", s.handleGetWorkflowLLMRoutes)
	mux.HandleFunc("GET /agent-runs/{id}", s.handleGetAgentRun)
	mux.HandleFunc("GET /llmkit/models", s.handleModels)
	mux.HandleFunc("GET /workers/queued", s.handleQueuedWorker)
	return mux
}

func (s *Server) handleListSkills(w http.ResponseWriter, _ *http.Request) {
	entries := s.skillCatalog.List()
	skills := make([]skillResponse, 0, len(entries))
	for _, entry := range entries {
		report := skillkit.Evaluate(entry, s.skillGateContext)
		reasons := make([]skillReasonResponse, 0, len(report.Reasons))
		for _, reason := range report.Reasons {
			reasons = append(reasons, skillReasonResponse{Code: reason.Code, Subject: reason.Subject})
		}
		skills = append(skills, skillResponse{
			Name:         entry.Ref.Name,
			Description:  entry.Manifest.Description,
			Digest:       entry.Ref.Digest,
			Scope:        entry.Scope,
			Availability: report.State,
			Reasons:      reasons,
		})
	}
	writeJSON(w, http.StatusOK, skillListResponse{Skills: skills})
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
		writeError(w, http.StatusBadRequest, "unsupported_run_mode", "supported run_mode values are sync and queued")
		return
	}
	profile, err := parseTaskProfile(req.TaskProfilePreset, req.TaskProfile, s.models)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_task_profile", err.Error())
		return
	}
	skillRefs, err := s.resolveSkillRefs(req.SkillRefs)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_skill_refs", err.Error())
		return
	}
	inputRef := "artifact:" + req.ID + ":input"
	if err := putTextArtifact(r.Context(), s.artifacts, inputRef, req.Input); err != nil {
		writeError(w, http.StatusInternalServerError, "artifact_error", err.Error())
		return
	}
	metadata := map[string]any{
		"input_ref":    inputRef,
		"run_mode":     string(runMode),
		"task_profile": profile,
	}
	if len(skillRefs) > 0 {
		metadata["skill_refs"] = skillRefs
	}
	run := workflowkit.WorkflowRun{
		ID:       req.ID,
		InputRef: inputRef,
		Metadata: metadata,
	}
	if runMode == RunModeQueued {
		run.Status = workflowkit.StatusPending
		if err := s.workflows.Save(r.Context(), run); err != nil {
			writeError(w, http.StatusInternalServerError, "workflow_error", err.Error())
			return
		}
		s.runQueuedWorkflow(run)
		writeJSON(w, http.StatusAccepted, workflowToResponse(run, runMode))
		return
	}
	run, err = s.executor.Run(r.Context(), run)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workflow_error", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, workflowToResponse(run, runMode))
}

// resolveSkillRefs accepts caller-supplied names only as lookup inputs. The
// host reuses its immutable catalog and GateContext, then persists the catalog
// digest so a future workflow run cannot silently select a newer skill.
func (s *Server) resolveSkillRefs(requested []workflowSkillRef) ([]map[string]string, error) {
	if len(requested) == 0 {
		return nil, nil
	}
	if s.skillCatalog == nil {
		return nil, errors.New("skill catalog is unavailable")
	}

	resolved := make([]map[string]string, 0, len(requested))
	for _, ref := range requested {
		if strings.TrimSpace(ref.Name) == "" {
			return nil, errors.New("skill reference name is required")
		}
		entry, err := s.skillCatalog.Resolve(skillkit.Ref{Name: ref.Name, Digest: ref.Digest})
		if err != nil {
			return nil, fmt.Errorf("resolve skill %q: %w", ref.Name, err)
		}
		if report := skillkit.Evaluate(entry, s.skillGateContext); report.State != skillkit.AvailabilityEligible {
			return nil, fmt.Errorf("skill %q is unavailable", entry.Ref.Name)
		}
		resolved = append(resolved, map[string]string{
			"name":   entry.Ref.Name,
			"digest": entry.Ref.Digest,
		})
	}
	return resolved, nil
}

func (s *Server) runQueuedWorkflow(run workflowkit.WorkflowRun) {
	go func() {
		if s.queue == nil {
			_, _ = s.executor.Run(context.Background(), run)
			return
		}
		_, _ = s.runOneQueuedWorkflow(context.Background())
	}()
}

// StartQueuedWorker starts the host-owned in-process worker loop for pending workflows.
func (s *Server) StartQueuedWorker(ctx context.Context) {
	s.worker.markStarted(queuedWorkerID)
	go s.runQueuedWorkerLoop(ctx)
}

func (s *Server) runQueuedWorkerLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		ran, _ := s.runOneQueuedWorkflow(ctx)
		if ran {
			continue
		}
		timer := time.NewTimer(queuedPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Server) runOneQueuedWorkflow(ctx context.Context) (bool, error) {
	if s.queue == nil {
		return false, nil
	}
	s.worker.recordClaimAttempt()
	claimed, err := s.queue.ClaimRunnable(ctx, queuedWorkerID, s.queuedLeaseDuration())
	if err != nil {
		if errors.Is(err, workflowkit.ErrNoRunnableWorkflow) {
			s.worker.recordIdle()
		} else {
			s.worker.recordError(err)
		}
		return false, err
	}
	s.worker.recordClaimed(claimed.ID)
	stopHeartbeat := s.startQueuedLeaseHeartbeat(ctx, claimed.ID)
	defer func() {
		stopHeartbeat()
		_, _ = s.queue.ReleaseLease(context.Background(), claimed.ID, queuedWorkerID)
	}()
	_, err = s.executor.Run(ctx, claimed)
	if err != nil {
		s.worker.recordWorkflowError(claimed.ID, err)
		return true, err
	}
	s.worker.recordCompleted(claimed.ID)
	return true, err
}

func (s *Server) startQueuedLeaseHeartbeat(ctx context.Context, workflowID string) func() {
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		leaseDuration := s.queuedLeaseDuration()
		ticker := time.NewTicker(queuedHeartbeatInterval(leaseDuration))
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if _, err := s.queue.ExtendLease(heartbeatCtx, workflowID, queuedWorkerID, leaseDuration); err != nil {
					if heartbeatCtx.Err() != nil {
						return
					}
					s.worker.recordHeartbeatError(workflowID, err)
					continue
				}
				s.worker.recordLeaseExtended(workflowID)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	if s.queries == nil {
		writeError(w, http.StatusInternalServerError, "workflow_error", "workflow query store is not configured")
		return
	}
	query, err := parseWorkflowQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	runs, err := s.queries.ListWorkflows(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workflow_error", err.Error())
		return
	}
	workflows := make([]workflowResponse, 0, len(runs))
	for _, run := range runs {
		workflows = append(workflows, workflowToResponse(run, RunModeSync))
	}
	writeJSON(w, http.StatusOK, workflowListResponse{Workflows: workflows})
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	run, err := s.workflows.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowToResponse(run, RunModeSync))
}

func (s *Server) handleWorkflowEvents(w http.ResponseWriter, r *http.Request) {
	run, err := s.workflows.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowToEventsResponse(run))
}

func (s *Server) handleRequeueWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.workflows.Update(r.Context(), id, func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if run.Status != workflowkit.StatusFailed && run.Status != workflowkit.StatusCancelled {
			return run, errWorkflowNotRequeueable
		}
		fromStatus := run.Status
		run.Status = workflowkit.StatusPending
		run.Error = ""
		run.CurrentStep = ""
		run.ApprovalRef = ""
		run.WaitingReason = ""
		run.LeaseOwner = ""
		run.LeaseUntil = time.Time{}
		if run.Metadata == nil {
			run.Metadata = make(map[string]any)
		}
		run.Metadata["run_mode"] = string(RunModeQueued)
		appendRequeueEvent(run.Metadata, fromStatus, workflowkit.StatusPending, time.Now().UTC())
		return run, nil
	})
	if err != nil {
		if errors.Is(err, errWorkflowNotRequeueable) {
			writeError(w, http.StatusBadRequest, "invalid_request", "only failed or cancelled workflows can be requeued")
			return
		}
		writeWorkflowStoreError(w, err)
		return
	}
	s.runQueuedWorkflow(run)
	writeJSON(w, http.StatusAccepted, workflowToResponse(run, RunModeQueued))
}

func (s *Server) handleApproveWorkflow(w http.ResponseWriter, r *http.Request) {
	identity, err := s.approvalAuthenticator.AuthenticateApproval(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		writeApprovalUnauthorized(w)
		return
	}
	var req approveWorkflowRequest
	if !decodeJSONStrict(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	run, err := s.executor.Approve(r.Context(), id, workflowkit.Approval{
		AuditRef: "audit:" + id + ":approval",
		Metadata: map[string]any{
			"approved_by":   identity.Subject,
			"approval_note": req.Note,
		},
	})
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	if err := s.recordBusinessOutcome(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "llm_audit_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, workflowToResponse(run, RunModeSync))
}

func (s *Server) handleApproveAgentTool(w http.ResponseWriter, r *http.Request) {
	identity, err := s.approvalAuthenticator.AuthenticateApproval(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		writeApprovalUnauthorized(w)
		return
	}
	var req agentApprovalRequest
	if !decodeJSONStrict(w, r, &req) {
		return
	}
	if len(req.Resolutions) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "at least one tool resolution is required")
		return
	}
	resolutions := req.coreResolutions()
	workflowID := r.PathValue("id")
	run, err := s.workflows.Get(r.Context(), workflowID)
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	approval := agentApprovalFromMetadata(run.Metadata)
	if run.Status == workflowkit.StatusWaitingApproval && approval == nil && run.ApprovalRef == "approval:"+run.ID && run.OutputRef != "" {
		if completed := completedAgentApprovalFromMetadata(run.Metadata); completed != nil && resolutionsMatchCompletedApproval(resolutions, completed.Tools) {
			writeJSON(w, http.StatusOK, workflowToResponse(run, RunModeSync))
			return
		}
	}
	if run.Status != workflowkit.StatusWaitingApproval || approval == nil || s.agentApprovals == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", errAgentApprovalNotPending.Error())
		return
	}
	if hasDeniedResolution(resolutions) {
		if err := s.agentApprovals.Reject(r.Context(), workflowID, *approval, identity.Subject); err != nil {
			writeError(w, http.StatusConflict, "workflow_error", "agent tool approval could not be rejected")
			return
		}
		updated, err := s.rejectAgentApprovalWorkflow(r.Context(), run, "agent tool approval rejected")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "workflow_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, workflowToResponse(updated, RunModeSync))
		return
	}

	next, result, resumeErr := s.agentApprovals.ApproveAndResume(r.Context(), workflowID, *approval, identity.Subject, resolutions)
	if errors.Is(resumeErr, runkit.ErrCheckpointNotClaimable) {
		writeError(w, http.StatusConflict, "approval_conflict", "agent tool approval is already being processed")
		return
	}
	if errors.Is(resumeErr, agentcore.ErrApprovalPending) && result != nil && result.Interruption != nil && len(next.Tools) > 0 {
		updated, err := s.replacePendingAgentApproval(r.Context(), run, approval.CheckpointID, next)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "workflow_error", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, workflowToResponse(updated, RunModeSync))
		return
	}
	if resumeErr != nil || result == nil {
		_ = s.failAgentApprovalWorkflow(r.Context(), run, approval.CheckpointID, result, errAgentApprovalResumeFailed)
		if errors.Is(resumeErr, agentcore.ErrInvalidApprovalResolution) {
			writeError(w, http.StatusBadRequest, "invalid_request", "tool resolutions do not match the pending approval")
			return
		}
		writeError(w, http.StatusInternalServerError, "workflow_error", errAgentApprovalResumeFailed.Error())
		return
	}

	updated, err := s.persistResumedAgentResult(r.Context(), run, approval.CheckpointID, result)
	if err != nil {
		_ = s.failAgentApprovalWorkflow(r.Context(), run, approval.CheckpointID, result, err)
		writeError(w, http.StatusInternalServerError, "workflow_error", "agent approval result persistence failed")
		return
	}
	writeJSON(w, http.StatusOK, workflowToResponse(updated, RunModeSync))
}

func hasDeniedResolution(resolutions []agentcore.ToolApprovalResolution) bool {
	for _, resolution := range resolutions {
		if !resolution.Allowed {
			return true
		}
	}
	return false
}

func (s *Server) persistResumedAgentResult(ctx context.Context, run workflowkit.WorkflowRun, checkpointID string, result *agentcore.RunResult) (workflowkit.WorkflowRun, error) {
	outputRef := "artifact:" + run.ID + ":agent-output"
	if err := putTextArtifact(ctx, s.artifacts, outputRef, result.Content); err != nil {
		return workflowkit.WorkflowRun{}, err
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
		return workflowkit.WorkflowRun{}, err
	}
	return s.workflows.Update(ctx, run.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		pending := agentApprovalFromMetadata(current.Metadata)
		if current.Status != workflowkit.StatusWaitingApproval || pending == nil || pending.CheckpointID != checkpointID {
			return current, errAgentApprovalNotPending
		}
		rememberCompletedAgentApprovalMetadata(current.Metadata, *pending)
		clearAgentApprovalMetadata(current.Metadata)
		current.OutputRef = outputRef
		current.AgentRunID = result.RunID.String()
		current.ApprovalRef = "approval:" + current.ID
		current.WaitingReason = "operator approval required before finalizing host API output"
		current.Metadata["agent_output_ref"] = outputRef
		return current, nil
	})
}

func (s *Server) replacePendingAgentApproval(ctx context.Context, run workflowkit.WorkflowRun, previousCheckpointID string, approval agentApprovalResponse) (workflowkit.WorkflowRun, error) {
	return s.workflows.Update(ctx, run.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if !workflowHasPendingAgentApproval(current, previousCheckpointID) {
			return current, errAgentApprovalNotPending
		}
		for key, value := range approval.workflowMetadata() {
			current.Metadata[key] = value
		}
		current.ApprovalRef = "agent-approval:" + approval.CheckpointID
		current.WaitingReason = "operator tool approval required before record_review executes"
		return current, nil
	})
}

func (s *Server) rejectAgentApprovalWorkflow(ctx context.Context, run workflowkit.WorkflowRun, reason string) (workflowkit.WorkflowRun, error) {
	if err := s.runs.Complete(ctx, run.AgentRunID, runkit.TerminalSummary{
		Status:      runkit.StatusFailed,
		AbortReason: reason,
	}); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return s.workflows.Update(ctx, run.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if current.Status != workflowkit.StatusWaitingApproval || agentApprovalFromMetadata(current.Metadata) == nil {
			return current, errAgentApprovalNotPending
		}
		clearAgentApprovalMetadata(current.Metadata)
		current.Status = workflowkit.StatusCancelled
		current.Error = reason
		current.ApprovalRef = ""
		current.WaitingReason = ""
		return current, nil
	})
}

func (s *Server) failAgentApprovalWorkflow(ctx context.Context, run workflowkit.WorkflowRun, checkpointID string, result *agentcore.RunResult, cause error) error {
	if result != nil {
		_ = s.runs.Complete(ctx, result.RunID.String(), runkit.TerminalSummary{
			Status:       runkit.StatusFailed,
			AbortReason:  cause.Error(),
			InputTokens:  result.Usage.InputTokens,
			OutputTokens: result.Usage.OutputTokens,
			LLMCalls:     result.ExecutionSummary.LLMCalls,
			ToolCalls:    result.ExecutionSummary.ToolCalls,
			UsedTools:    result.ExecutionSummary.UsedTools,
		})
	}
	_, err := s.workflows.Update(ctx, run.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if !workflowHasPendingAgentApproval(current, checkpointID) {
			return current, errAgentApprovalNotPending
		}
		clearAgentApprovalMetadata(current.Metadata)
		current.Status = workflowkit.StatusFailed
		current.Error = "agent tool approval processing failed"
		current.ApprovalRef = ""
		current.WaitingReason = ""
		return current, nil
	})
	return err
}

func clearAgentApprovalMetadata(metadata map[string]any) {
	delete(metadata, agentApprovalMetadataID)
	delete(metadata, agentApprovalMetadataTools)
}

func workflowHasPendingAgentApproval(run workflowkit.WorkflowRun, checkpointID string) bool {
	approval := agentApprovalFromMetadata(run.Metadata)
	return run.Status == workflowkit.StatusWaitingApproval && approval != nil && approval.CheckpointID == checkpointID
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
	stats, err := llmkit.RefreshModelStats(s.llmHome)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "llm_audit_error", err.Error())
		return
	}
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
		Stats:  modelStatsToResponse(stats),
	})
}

func (s *Server) handleQueuedWorker(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.worker.snapshot())
}

func (s *Server) recordBusinessOutcome(ctx context.Context, workflowID string) error {
	records, err := llmkit.ReadRouteAudits(s.llmHome, llmkit.AuditFilter{TaskID: workflowID})
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	record := records[len(records)-1]
	outcome := llmkit.TaskOutcome{
		RouteID:         record.Route.RouteID,
		TaskID:          record.Route.TaskID,
		Attempt:         record.Route.Attempt,
		TaskType:        record.Route.TaskType,
		AccountAlias:    record.Route.AccountAlias,
		ModelAlias:      record.Route.ModelAlias,
		Provider:        record.Route.Provider,
		Success:         true,
		BusinessOutcome: llmkit.BusinessOutcomeSuccess,
		SuccessSignal:   llmkit.SuccessSignalHumanAccepted,
	}
	if record.Outcome != nil {
		outcome = *record.Outcome
		outcome.BusinessOutcome = llmkit.BusinessOutcomeSuccess
		outcome.SuccessSignal = llmkit.SuccessSignalHumanAccepted
		outcome.FailureReason = ""
	}
	recorder, err := llmkit.NewJSONLRecorder(s.llmHome)
	if err != nil {
		return err
	}
	return recorder.RecordOutcome(ctx, outcome)
}

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
		providers: s.providers,
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
	profile := taskProfileFromMetadata(run.Metadata["task_profile"])
	request := agentcore.RunRequest{
		Input: "Review input artifact " + run.InputRef,
		Metadata: map[string]any{
			"workflow_id":  run.ID,
			"task_profile": profile,
		},
	}
	if profile.NeedsTools {
		request.AllowedPermissions = []policy.Permission{policy.PermissionWrite}
	}
	result, err := s.runner.RunDetailed(ctx, request)
	if err != nil {
		if errors.Is(err, agentcore.ErrApprovalPending) && result != nil && result.Interruption != nil && s.approvals != nil {
			approval, saveErr := s.approvals.SavePending(ctx, run.ID, result.Interruption.Checkpoint)
			if saveErr != nil {
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
		WaitingReason: "operator approval required before finalizing host API output",
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

type routingAgentRunner struct {
	llmkitHome    string
	runs          runkit.Store
	artifacts     artifactkit.Store
	health        llmkit.HealthStore
	candidates    []llmkit.Candidate
	statsProvider goagentadapter.ModelStatsProvider
	providers     map[string]goagentadapter.ProviderClient
}

func (r routingAgentRunner) RunDetailed(ctx context.Context, req agentcore.RunRequest) (*agentcore.RunResult, error) {
	workflowID, _ := req.Metadata["workflow_id"].(string)
	if workflowID == "" {
		return nil, fmt.Errorf("workflow_id metadata is required")
	}
	profile := taskProfileFromMetadata(req.Metadata["task_profile"])
	agent, err := r.newAgent(workflowID, profile)
	if err != nil {
		return nil, err
	}
	return agent.RunDetailed(ctx, req)
}

// ResumeDetailed rebuilds the host-owned tool registry from checkpoint request
// metadata before goagent validates and resumes the exact pending tool batch.
func (r routingAgentRunner) ResumeDetailed(ctx context.Context, checkpoint agentcore.RunCheckpoint, resolutions []agentcore.ToolApprovalResolution) (*agentcore.RunResult, error) {
	workflowID, _ := checkpoint.Request.Metadata["workflow_id"].(string)
	if workflowID == "" {
		return nil, fmt.Errorf("workflow_id metadata is required in checkpoint")
	}
	profile := taskProfileFromMetadata(checkpoint.Request.Metadata["task_profile"])
	agent, err := r.newAgent(workflowID, profile)
	if err != nil {
		return nil, err
	}
	return agent.ResumeDetailed(ctx, checkpoint, resolutions)
}

func (r routingAgentRunner) newAgent(workflowID string, profile llmkit.TaskProfile) (*agentcore.Agent, error) {
	recorder, err := llmkit.NewJSONLRecorder(r.llmkitHome)
	if err != nil {
		return nil, err
	}
	client := goagentadapter.NewClient(goagentadapter.Config{
		Candidates: r.candidates,
		Providers:  r.providers,
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
	options = append(options, r.toolOptions(profile)...)
	return agentcore.NewAgent(options...)
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

func loadLLMKitComposition(home string) ([]llmkit.Candidate, map[string]goagentadapter.ProviderClient, *llmkit.ModelStats, error) {
	stats, err := llmkit.RefreshModelStats(home)
	if err != nil {
		return nil, nil, nil, err
	}

	configPath := filepath.Join(home, "config.yaml")
	if _, err := os.Stat(configPath); err != nil {
		if os.IsNotExist(err) {
			return defaultCandidates(), defaultProviders(), stats, nil
		}
		return nil, nil, nil, err
	}

	config, err := llmkit.LoadConfig(home)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateConfiguredAPIKeyEnvs(*config, os.Getenv); err != nil {
		return nil, nil, nil, err
	}

	candidates := config.Candidates()
	if len(candidates) == 0 {
		return nil, nil, nil, fmt.Errorf("llmkit config has no models")
	}
	providers, err := goagentadapter.OpenAICompatibleProvidersFromConfig(*config, os.Getenv, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateProvidersForCandidates(candidates, providers); err != nil {
		return nil, nil, nil, err
	}
	return candidates, providers, stats, nil
}

func validateConfiguredAPIKeyEnvs(config llmkit.Config, getenv func(string) string) error {
	for _, account := range config.Accounts {
		envName := strings.TrimSpace(account.APIKeyEnv)
		if envName == "" {
			continue
		}
		if getenv == nil || strings.TrimSpace(getenv(envName)) == "" {
			return fmt.Errorf("account %q api_key_env %q is not set", account.Alias, envName)
		}
	}
	return nil
}

func validateProvidersForCandidates(candidates []llmkit.Candidate, providers map[string]goagentadapter.ProviderClient) error {
	for _, candidate := range candidates {
		if providers[candidate.Model.Alias] == nil {
			return fmt.Errorf("model %q has no configured provider client", candidate.Model.Alias)
		}
	}
	return nil
}

func defaultProviders() map[string]goagentadapter.ProviderClient {
	return map[string]goagentadapter.ProviderClient{
		"local-free":     staticProvider{content: "host API draft from local-free"},
		"cloud-advanced": staticProvider{content: "host API draft from cloud-advanced"},
	}
}

func defaultCandidates() []llmkit.Candidate {
	return []llmkit.Candidate{
		{
			Model: llmkit.ModelCapability{
				Alias:              "local-free",
				Provider:           "local",
				IsLocal:            true,
				CapabilityLevel:    llmkit.CapabilitySimple,
				SupportsTools:      true,
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
				SupportsTools:      true,
				ContextWindowClass: llmkit.ContextLong,
				PriceClass:         llmkit.PriceHigh,
				LatencyClass:       llmkit.LatencyNormalClass,
			},
			AccountAlias: "cloud-prod",
		},
	}
}

func workflowToResponse(run workflowkit.WorkflowRun, runMode RunMode) workflowResponse {
	runMode = workflowRunMode(run, runMode)
	return workflowResponse{
		ID:            run.ID,
		Status:        string(run.Status),
		RunMode:       string(runMode),
		SkillRefs:     workflowSkillRefsFromMetadata(run.Metadata),
		InputRef:      run.InputRef,
		OutputRef:     run.OutputRef,
		AgentRunID:    run.AgentRunID,
		AuditRef:      run.AuditRef,
		ApprovalRef:   run.ApprovalRef,
		WaitingReason: run.WaitingReason,
		AgentApproval: agentApprovalFromMetadata(run.Metadata),
		Completed:     append([]string(nil), run.CompletedSteps...),
	}
}

func workflowSkillRefsFromMetadata(metadata map[string]any) []workflowSkillRef {
	switch refs := metadata["skill_refs"].(type) {
	case []map[string]string:
		response := make([]workflowSkillRef, 0, len(refs))
		for _, ref := range refs {
			if ref["name"] == "" || ref["digest"] == "" {
				return nil
			}
			response = append(response, workflowSkillRef{Name: ref["name"], Digest: ref["digest"]})
		}
		return response
	case []any:
		response := make([]workflowSkillRef, 0, len(refs))
		for _, value := range refs {
			ref, ok := value.(map[string]any)
			if !ok {
				return nil
			}
			name, nameOK := ref["name"].(string)
			digest, digestOK := ref["digest"].(string)
			if !nameOK || !digestOK || name == "" || digest == "" {
				return nil
			}
			response = append(response, workflowSkillRef{Name: name, Digest: digest})
		}
		return response
	default:
		return nil
	}
}

func workflowToEventsResponse(run workflowkit.WorkflowRun) workflowEventsResponse {
	events := make([]workflowEventResponse, 0, len(run.StepRecords))
	for _, record := range run.StepRecords {
		events = append(events, stepRecordToEvent(record))
	}
	events = append(events, requeueEventsFromMetadata(run.Metadata)...)
	sort.SliceStable(events, func(i, j int) bool {
		left := workflowEventTime(events[i])
		right := workflowEventTime(events[j])
		if left.IsZero() || right.IsZero() {
			return false
		}
		return left.Before(right)
	})
	return workflowEventsResponse{
		WorkflowID:  run.ID,
		Status:      string(run.Status),
		RunMode:     string(workflowRunMode(run, RunModeSync)),
		CurrentStep: run.CurrentStep,
		Completed:   append([]string(nil), run.CompletedSteps...),
		Events:      events,
	}
}

func stepRecordToEvent(record workflowkit.StepRecord) workflowEventResponse {
	return workflowEventResponse{
		Type:          "step",
		Name:          record.Name,
		Status:        string(record.Status),
		Attempt:       record.Attempt,
		OutputRef:     record.OutputRef,
		AgentRunID:    record.AgentRunID,
		AuditRef:      record.AuditRef,
		Error:         record.Error,
		ApprovalRef:   record.ApprovalRef,
		WaitingReason: record.WaitingReason,
		StartedAt:     record.StartedAt,
		EndedAt:       record.EndedAt,
	}
}

func workflowEventTime(event workflowEventResponse) time.Time {
	if !event.StartedAt.IsZero() {
		return event.StartedAt
	}
	return event.At
}

func appendRequeueEvent(metadata map[string]any, from workflowkit.Status, to workflowkit.Status, at time.Time) {
	event := map[string]any{
		"type":        "workflow_requeued",
		"from_status": string(from),
		"to_status":   string(to),
		"at":          at.Format(time.RFC3339Nano),
	}
	metadata[requeueEventsMetadataKey] = appendMetadataEvent(metadata[requeueEventsMetadataKey], event)
}

func appendMetadataEvent(existing any, event map[string]any) []any {
	events := metadataEventSlice(existing)
	return append(events, event)
}

func metadataEventSlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return append([]any(nil), typed...)
	case []map[string]any:
		events := make([]any, 0, len(typed))
		for _, event := range typed {
			events = append(events, event)
		}
		return events
	default:
		return nil
	}
}

func requeueEventsFromMetadata(metadata map[string]any) []workflowEventResponse {
	rawEvents := metadataEventSlice(metadata[requeueEventsMetadataKey])
	events := make([]workflowEventResponse, 0, len(rawEvents))
	for _, raw := range rawEvents {
		event, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if metadataString(event, "type") != "workflow_requeued" {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, metadataString(event, "at"))
		events = append(events, workflowEventResponse{
			Type:       "workflow_requeued",
			FromStatus: metadataString(event, "from_status"),
			ToStatus:   metadataString(event, "to_status"),
			At:         at,
		})
	}
	return events
}

func metadataString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func workflowRunMode(run workflowkit.WorkflowRun, fallback RunMode) RunMode {
	if value, ok := run.Metadata["run_mode"].(string); ok {
		if runMode, valid := parseRunMode(value); valid {
			return runMode
		}
	}
	if fallback == "" {
		return RunModeSync
	}
	return fallback
}

func llmRouteToResponse(record llmkit.RouteAuditRecord) llmRouteResponse {
	route := record.Route
	response := llmRouteResponse{
		RouteID:               route.RouteID,
		TaskID:                route.TaskID,
		Attempt:               route.Attempt,
		TaskType:              route.TaskType,
		TaskProfile:           taskProfileToResponse(route.TaskProfile),
		AccountAlias:          route.AccountAlias,
		ModelAlias:            route.ModelAlias,
		Provider:              route.Provider,
		Selected:              route.Selected,
		Reason:                route.Reason,
		Score:                 route.Score,
		ScoreBreakdown:        copyScoreBreakdown(route.ScoreBreakdown),
		CandidateModelAliases: append([]string(nil), route.CandidateModelAliases...),
		Candidates:            routeCandidatesToResponse(route.Candidates),
	}
	if record.Outcome != nil {
		outcome := record.Outcome
		response.Outcome = &llmRouteOutcomeResponse{
			Success:         outcome.Success,
			ErrorCode:       outcome.ErrorCode,
			ErrorClass:      string(outcome.ErrorClass),
			LatencyMillis:   outcome.LatencyMillis,
			InputTokens:     outcome.InputTokens,
			OutputTokens:    outcome.OutputTokens,
			EstimatedCents:  outcome.EstimatedCents,
			BusinessOutcome: string(outcome.BusinessOutcome),
			SuccessSignal:   string(outcome.SuccessSignal),
			FailureReason:   outcome.FailureReason,
		}
	}
	return response
}

func routeCandidatesToResponse(candidates []llmkit.CandidateScore) []llmRouteCandidateResponse {
	if len(candidates) == 0 {
		return nil
	}
	response := make([]llmRouteCandidateResponse, 0, len(candidates))
	for _, candidate := range candidates {
		response = append(response, llmRouteCandidateResponse{
			Alias:          candidate.Alias,
			AccountAlias:   candidate.AccountAlias,
			Available:      candidate.Available,
			Score:          candidate.Score,
			ScoreBreakdown: copyScoreBreakdown(candidate.ScoreBreakdown),
			Reason:         candidate.Reason,
		})
	}
	return response
}

func modelStatsToResponse(stats *llmkit.ModelStats) []modelStatsResponse {
	if stats == nil || len(stats.Models) == 0 {
		return nil
	}
	keys := make([]string, 0, len(stats.Models))
	for key := range stats.Models {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	response := make([]modelStatsResponse, 0, len(keys))
	for _, key := range keys {
		entry := stats.Models[key]
		response = append(response, modelStatsResponse{
			TaskType:          entry.TaskType,
			AccountAlias:      entry.AccountAlias,
			ModelAlias:        entry.ModelAlias,
			Provider:          entry.Provider,
			RouteAttempts:     entry.RouteAttempts,
			OutcomeCount:      entry.OutcomeCount,
			PendingOutcomes:   entry.PendingOutcomes,
			Successes:         entry.Successes,
			Failures:          entry.Failures,
			SuccessRate:       entry.SuccessRate,
			FailureRate:       entry.FailureRate,
			AvgLatencyMillis:  entry.AvgLatencyMillis,
			AvgInputTokens:    entry.AvgInputTokens,
			AvgOutputTokens:   entry.AvgOutputTokens,
			AvgEstimatedCents: entry.AvgEstimatedCents,
		})
	}
	return response
}

func taskProfileToResponse(profile *llmkit.TaskProfile) *taskProfileResponse {
	if profile == nil {
		return nil
	}
	return &taskProfileResponse{
		TaskType:          profile.TaskType,
		Complexity:        string(profile.Complexity),
		Latency:           string(profile.Latency),
		FailureCost:       string(profile.FailureCost),
		Privacy:           string(profile.Privacy),
		MaxEstimatedCents: profile.MaxEstimatedCents,
		NeedsReasoning:    profile.NeedsReasoning,
		NeedsTools:        profile.NeedsTools,
		NeedsJSON:         profile.NeedsJSON,
		NeedsLongContext:  profile.NeedsLongContext,
	}
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

func parseTaskProfile(preset string, req *taskProfileRequest, candidates []llmkit.Candidate) (llmkit.TaskProfile, error) {
	profile, err := taskProfilePreset(preset)
	if err != nil {
		return llmkit.TaskProfile{}, err
	}
	if err := applyTaskProfileOverride(&profile, req); err != nil {
		return llmkit.TaskProfile{}, err
	}
	if err := validateTaskProfile(profile, candidates); err != nil {
		return llmkit.TaskProfile{}, err
	}
	return profile, nil
}

func applyTaskProfileOverride(profile *llmkit.TaskProfile, req *taskProfileRequest) error {
	if req == nil {
		return nil
	}
	if req.TaskType != nil {
		value, err := nonEmptyTaskProfileString("task_type", *req.TaskType)
		if err != nil {
			return err
		}
		profile.TaskType = value
	}
	if req.Complexity != nil {
		raw, err := nonEmptyTaskProfileString("complexity", *req.Complexity)
		if err != nil {
			return err
		}
		value := llmkit.Complexity(raw)
		if value != llmkit.ComplexitySimple && value != llmkit.ComplexityMedium && value != llmkit.ComplexityHard {
			return fmt.Errorf("unsupported complexity %q", *req.Complexity)
		}
		profile.Complexity = value
	}
	if req.Latency != nil {
		raw, err := nonEmptyTaskProfileString("latency", *req.Latency)
		if err != nil {
			return err
		}
		value := llmkit.LatencyRequirement(raw)
		if value != llmkit.LatencyNone && value != llmkit.LatencyNormal && value != llmkit.LatencyUrgent {
			return fmt.Errorf("unsupported latency %q", *req.Latency)
		}
		profile.Latency = value
	}
	if req.FailureCost != nil {
		raw, err := nonEmptyTaskProfileString("failure_cost", *req.FailureCost)
		if err != nil {
			return err
		}
		value := llmkit.FailureCost(raw)
		if value != llmkit.FailureCostLow && value != llmkit.FailureCostMedium && value != llmkit.FailureCostHigh {
			return fmt.Errorf("unsupported failure_cost %q", *req.FailureCost)
		}
		profile.FailureCost = value
	}
	if req.Privacy != nil {
		raw, err := nonEmptyTaskProfileString("privacy", *req.Privacy)
		if err != nil {
			return err
		}
		value := llmkit.PrivacyLevel(raw)
		if value != llmkit.PrivacyLocalPreferred && value != llmkit.PrivacyCloudAllowed && value != llmkit.PrivacyLocalOnly {
			return fmt.Errorf("unsupported privacy %q", *req.Privacy)
		}
		profile.Privacy = value
	}
	if req.MaxEstimatedCents != nil {
		if *req.MaxEstimatedCents <= 0 {
			return fmt.Errorf("max_estimated_cents must be greater than zero")
		}
		profile.MaxEstimatedCents = *req.MaxEstimatedCents
	}
	if req.NeedsReasoning != nil {
		profile.NeedsReasoning = *req.NeedsReasoning
	}
	if req.NeedsTools != nil {
		profile.NeedsTools = *req.NeedsTools
	}
	if req.NeedsJSON != nil {
		profile.NeedsJSON = *req.NeedsJSON
	}
	if req.NeedsLongContext != nil {
		profile.NeedsLongContext = *req.NeedsLongContext
	}
	return nil
}

func nonEmptyTaskProfileString(field string, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s must not be empty", field)
	}
	return trimmed, nil
}

func taskProfilePreset(name string) (llmkit.TaskProfile, error) {
	switch strings.TrimSpace(name) {
	case "":
		return defaultHostTaskProfile(), nil
	case "simple_local":
		profile := defaultHostTaskProfile()
		profile.TaskType = "simple_local"
		return profile, nil
	case "balanced":
		profile := defaultHostTaskProfile()
		profile.TaskType = "balanced"
		profile.Complexity = llmkit.ComplexityMedium
		profile.FailureCost = llmkit.FailureCostMedium
		profile.Privacy = llmkit.PrivacyCloudAllowed
		return profile, nil
	case "high_success":
		profile := defaultHostTaskProfile()
		profile.TaskType = "high_success"
		profile.Complexity = llmkit.ComplexityHard
		profile.FailureCost = llmkit.FailureCostHigh
		profile.Privacy = llmkit.PrivacyCloudAllowed
		profile.NeedsReasoning = true
		return profile, nil
	case "local_only":
		profile := defaultHostTaskProfile()
		profile.TaskType = "local_only"
		profile.Privacy = llmkit.PrivacyLocalOnly
		return profile, nil
	default:
		return llmkit.TaskProfile{}, fmt.Errorf("unsupported task_profile_preset %q", name)
	}
}

func validateTaskProfile(profile llmkit.TaskProfile, candidates []llmkit.Candidate) error {
	if _, err := (llmkit.RoutePolicy{}).Select(profile, candidates); err != nil {
		return fmt.Errorf("task profile %q cannot route to an available model: %w", profile.TaskType, err)
	}
	return nil
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
	if value, ok := values["max_estimated_cents"].(float64); ok && value > 0 {
		profile.MaxEstimatedCents = int(value)
	}
	profile.NeedsReasoning, _ = values["needs_reasoning"].(bool)
	profile.NeedsTools, _ = values["needs_tools"].(bool)
	profile.NeedsJSON, _ = values["needs_json"].(bool)
	profile.NeedsLongContext, _ = values["needs_long_context"].(bool)
	return profile
}

func parseWorkflowQuery(r *http.Request) (workflowkit.WorkflowQuery, error) {
	values := r.URL.Query()
	query := workflowkit.WorkflowQuery{Limit: 50}
	if rawStatus := strings.TrimSpace(values.Get("status")); rawStatus != "" {
		status := workflowkit.Status(rawStatus)
		if !status.IsValid() {
			return workflowkit.WorkflowQuery{}, fmt.Errorf("unsupported workflow status %q", rawStatus)
		}
		query.Status = status
	}
	if rawRunMode := strings.TrimSpace(values.Get("run_mode")); rawRunMode != "" {
		runMode, ok := parseRunMode(rawRunMode)
		if !ok {
			return workflowkit.WorkflowQuery{}, fmt.Errorf("unsupported run_mode %q", rawRunMode)
		}
		query.MetadataEquals = map[string]string{"run_mode": string(runMode)}
	}
	if rawOrder := strings.TrimSpace(values.Get("order")); rawOrder != "" {
		order := workflowkit.WorkflowOrder(rawOrder)
		if !order.IsValid() || order == "" {
			return workflowkit.WorkflowQuery{}, fmt.Errorf("unsupported workflow order %q", rawOrder)
		}
		query.Order = order
	}
	if rawLimit := strings.TrimSpace(values.Get("limit")); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit <= 0 {
			return workflowkit.WorkflowQuery{}, fmt.Errorf("limit must be a positive integer")
		}
		query.Limit = limit
	}
	if query.Limit > 100 {
		query.Limit = 100
	}
	return query, nil
}

func parseRunMode(value string) (RunMode, bool) {
	switch RunMode(strings.TrimSpace(value)) {
	case "", RunModeSync:
		return RunModeSync, true
	case RunModeQueued:
		return RunModeQueued, true
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

func decodeJSONStrict(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
		return false
	}
	return true
}

func writeApprovalUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized", "approval authentication required")
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
