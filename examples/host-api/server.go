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
	"strings"
	"sync"
	"time"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/llmkit"
	goagentadapter "github.com/eruca/goagents/llmkit/adapters/goagent"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/runkit/goagentapproval"
	runsqlite "github.com/eruca/goagents/runkit/sqlitestore"
	"github.com/eruca/goagents/skillkit"
	"github.com/eruca/goagents/workflowkit"
	workflowsqlite "github.com/eruca/goagents/workflowkit/sqlitestore"
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
	// AgentApprovalKeychainService and AgentApprovalKeyID select the host-owned
	// Keychain item. They identify key material but never contain it.
	AgentApprovalKeychainService string
	AgentApprovalKeyID           string
	// SkillCatalog is a prebuilt, host-owned snapshot. The HTTP server never
	// discovers roots itself.
	SkillCatalog *skillkit.Catalog
	// SkillGateContext supplies host-owned facts used only to report availability.
	SkillGateContext skillkit.GateContext
	// Memory enables the optional governed project-memory boundary. A nil value
	// preserves the existing Host behavior and routes.
	Memory *memoryRuntimeConfig
}

type Server struct {
	// workerID owns this process's queue leases. It is unique per process so
	// several hosts sharing one database never mistake each other's leases.
	workerID                string
	artifacts               artifactkit.Store
	runs                    runkit.Store
	runCloser               io.Closer
	workflows               workflowkit.Store
	workflowCloser          io.Closer
	queries                 workflowkit.WorkflowQueryStore
	queue                   workflowkit.QueueLeaseStore
	executor                *workflowkit.Executor
	executions              *executionRegistry
	health                  *llmkit.MemoryHealthStore
	llmHome                 string
	models                  []llmkit.Candidate
	providers               map[string]goagentadapter.ProviderClient
	approvalAuthenticator   ApprovalAuthenticator
	agentApprovals          *hostAgentApprovalService
	skillCatalog            *skillkit.Catalog
	skillGateContext        skillkit.GateContext
	memory                  *memoryRuntime
	worker                  queuedWorkerStatus
	workerCfg               queuedWorkerConfig
	workerWake              chan struct{}
	workerStart             sync.Once
	workerDone              chan struct{}
	agentApprovalJanitorCfg agentApprovalJanitorConfig
	janitorStart            sync.Once
	janitorDone             chan struct{}
	closeMu                 sync.Mutex
	workflowClosed          bool
	workflowCloseErr        error
	runClosed               bool
	runCloseErr             error
}

type RunMode string

const (
	RunModeSync   RunMode = "sync"
	RunModeQueued RunMode = "queued"
)

const (
	defaultQueuedLeaseDuration        = time.Minute
	queuedLeaseDurationEnv            = "HOST_API_QUEUED_LEASE_DURATION"
	defaultAgentApprovalSweepInterval = time.Minute
	agentApprovalSweepIntervalEnv     = "HOST_API_AGENT_APPROVAL_SWEEP_INTERVAL"
	queuedPollInterval                = 100 * time.Millisecond
	minQueuedHeartbeatInterval        = time.Millisecond
	requeueEventsMetadataKey          = "requeue_events"
	queuedLeaseExpiredCode            = "queued_lease_expired"
)

var (
	errWorkflowNotRequeueable    = errors.New("workflow status cannot be requeued")
	errAgentApprovalNotPending   = errors.New("agent tool approval is not pending")
	errAgentApprovalResumeFailed = errors.New("agent tool approval resume failed")
)

type agentApprovalJanitorConfig struct {
	interval time.Duration
}

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

type hostDrainingResponse struct {
	Error hostDrainingError `json:"error"`
}

type hostDrainingError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewServer(config Config) (*Server, error) {
	if err := validateMemoryManagementConfig(config.Memory); err != nil {
		return nil, err
	}
	approvalKeychain, err := resolveAgentApprovalKeychainConfig(
		config.AgentApprovalKeychainService,
		config.AgentApprovalKeyID,
	)
	if err != nil {
		return nil, err
	}
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
	var memoryConfig *memoryRuntime
	if config.Memory != nil {
		memoryConfig, err = newMemoryRuntime(config.Memory, runs, artifacts)
		if err != nil {
			_ = workflows.Close()
			_ = runs.Close()
			return nil, err
		}
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
		providers:        providers,
		skillCatalog:     config.SkillCatalog,
		skillGateContext: config.SkillGateContext,
		memory:           memoryConfig,
	}
	agentApprovals, err := newHostAgentApprovalService(runs, config.AgentApprovalCipher, runner, approvalKeychain)
	if err != nil {
		_ = workflows.Close()
		_ = runs.Close()
		return nil, err
	}
	server := &Server{
		artifacts:               artifacts,
		runs:                    runs,
		runCloser:               runs,
		workflows:               workflows,
		workflowCloser:          workflows,
		queries:                 workflows,
		queue:                   workflows,
		executions:              newExecutionRegistry(),
		health:                  health,
		llmHome:                 resolved.LLMKitHome,
		models:                  models,
		providers:               providers,
		workerID:                "host-api:" + agentcore.NewRunID().String(),
		workerCfg:               workerCfg,
		workerWake:              make(chan struct{}, 1),
		workerDone:              make(chan struct{}),
		agentApprovalJanitorCfg: agentApprovalJanitorCfg,
		janitorDone:             make(chan struct{}),
		approvalAuthenticator:   config.ApprovalAuthenticator,
		agentApprovals:          agentApprovals,
		skillCatalog:            config.SkillCatalog,
		skillGateContext:        config.SkillGateContext,
		memory:                  memoryConfig,
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

func (s *Server) Close(ctx context.Context) error {
	if s.executions != nil {
		s.executions.BeginDrain()
		if err := s.executions.Wait(ctx); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	if !s.workflowClosed && s.workflowCloser != nil {
		s.workflowClosed = true
		s.workflowCloseErr = s.workflowCloser.Close()
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(s.workflowCloseErr, s.runCloseErr, err)
	}
	if !s.runClosed && s.runCloser != nil {
		s.runClosed = true
		s.runCloseErr = s.runCloser.Close()
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(s.workflowCloseErr, s.runCloseErr, err)
	}
	return errors.Join(s.workflowCloseErr, s.runCloseErr)
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
	if s.memory != nil {
		mux.HandleFunc("GET /projects/{projectID}/memories", s.handleListMemories)
		mux.HandleFunc("GET /projects/{projectID}/memories/{memoryID}", s.handleGetMemory)
		mux.HandleFunc("GET /projects/{projectID}/memories/{memoryID}/revisions", s.handleMemoryRevisions)
		mux.HandleFunc("POST /projects/{projectID}/memories", s.handleCreateMemory)
		mux.HandleFunc("POST /projects/{projectID}/memories/{memoryID}/activate", s.handleActivateMemory)
		mux.HandleFunc("POST /projects/{projectID}/memories/{memoryID}/dismiss", s.handleDismissMemory)
		mux.HandleFunc("POST /projects/{projectID}/memories/{memoryID}/correct", s.handleCorrectMemory)
		mux.HandleFunc("POST /projects/{projectID}/memories/{memoryID}/forget", s.handleForgetMemory)
		mux.HandleFunc("POST /projects/{projectID}/memories/{memoryID}/erase", s.handleEraseMemory)
	}
	return mux
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

func writeHostDraining(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, hostDrainingResponse{
		Error: hostDrainingError{
			Code:    "host_draining",
			Message: "host is draining",
		},
	})
}
