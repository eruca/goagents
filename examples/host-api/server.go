package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	"github.com/eruca/goagents/skillkit"
	"github.com/eruca/goagents/workflowkit"
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
	// ArtifactPostgresDSN stores artifacts in PostgreSQL instead of files
	// under ArtifactRoot.
	ArtifactPostgresDSN string
	// AgentRunPostgresDSN stores agent runs, their events, and approval
	// checkpoints in PostgreSQL instead of the SQLite file at AgentRunDBPath.
	AgentRunPostgresDSN string
	// LLMKitAuditPostgresDSN records LLM routes and outcomes in PostgreSQL
	// instead of JSONL files under LLMKitHome.
	LLMKitAuditPostgresDSN string
	// ContextMaxChars, when positive, compresses each agent model request
	// with contextkit to this many characters, the compiled system prompt
	// included; ContextMaxToolChars, when positive, truncates each tool
	// result in the model's view. Recalled memory is inserted after
	// compression and is bounded by the memory recall policy instead.
	ContextMaxChars     int
	ContextMaxToolChars int
	// AgentApprovalKeyring is a static keyring ("id:base64,..." of 32-byte
	// keys) used instead of Keychain. AgentApprovalKeyID names its active key.
	AgentApprovalKeyring string
	// WorkflowPostgresDSN stores workflows in PostgreSQL instead of the
	// SQLite file at WorkflowDBPath.
	WorkflowPostgresDSN string
	// LLMKitHealthPostgresDSN keeps provider health in PostgreSQL, shared by
	// every process using it, instead of in this process's memory.
	LLMKitHealthPostgresDSN string
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
	workerID  string
	artifacts artifactkit.Store
	runs      runkit.Store
	runCloser io.Closer
	workflows workflowkit.Store
	// creatingWorkflows holds the ids of workflows this process is creating,
	// so two concurrent requests for one id cannot both write its input.
	creatingWorkflows sync.Map
	workflowCloser    io.Closer
	queries           workflowkit.WorkflowQueryStore
	queue             workflowkit.QueueLeaseStore
	executor          *workflowkit.Executor
	executions        *executionRegistry
	health            llmkit.HealthStore
	healthCloser      io.Closer
	// backendClosers close the shared artifact and audit stores, then the
	// PostgreSQL pools every shared store uses.
	backendClosers []io.Closer
	// audit is nil in tests that build a Server directly; auditStore then
	// falls back to the JSONL files under llmHome.
	audit                   auditBackend
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
	healthClosed            bool
	healthCloseErr          error
	backendsClosed          bool
	backendCloseErr         error
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
	approvalCipher, approvalKeychain, err := resolveAgentApprovalKeys(config)
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
	// opened collects every backend opened so far, so a later failure closes
	// them all and Close closes the shared ones.
	// Stores close before the pools they share: opened closes in reverse.
	pools := &pgPools{}
	opened := []io.Closer{pools}
	closeStores := func() {
		for i := len(opened) - 1; i >= 0; i-- {
			_ = opened[i].Close()
		}
	}
	artifacts, artifactCloser, err := openArtifactStore(resolved, pools)
	if err != nil {
		return nil, err
	}
	if artifactCloser != nil {
		opened = append(opened, artifactCloser)
	}
	audit, auditCloser, err := openAuditBackend(resolved, pools)
	if err != nil {
		closeStores()
		return nil, err
	}
	if auditCloser != nil {
		opened = append(opened, auditCloser)
	}
	runs, err := openRunStore(resolved, pools)
	if err != nil {
		closeStores()
		return nil, err
	}
	opened = append(opened, runs)
	workflows, err := openWorkflowStore(resolved, pools)
	if err != nil {
		closeStores()
		return nil, err
	}
	opened = append(opened, workflows)
	health, healthCloser, err := openHealthStore(resolved, pools)
	if err != nil {
		closeStores()
		return nil, err
	}
	if healthCloser != nil {
		opened = append(opened, healthCloser)
	}
	var backendClosers []io.Closer
	for _, closer := range []io.Closer{artifactCloser, auditCloser} {
		if closer != nil {
			backendClosers = append(backendClosers, closer)
		}
	}
	// Close closes the stores first and the shared pools last.
	backendClosers = append(backendClosers, pools)
	var memoryConfig *memoryRuntime
	if config.Memory != nil {
		memoryConfig, err = newMemoryRuntime(config.Memory, runs, artifacts)
		if err != nil {
			closeStores()
			return nil, err
		}
	}
	runner := routingAgentRunner{
		llmkitHome:       resolved.LLMKitHome,
		audit:            audit,
		runs:             runs,
		artifacts:        artifacts,
		health:           health,
		candidates:       models,
		statsProvider:    audit.ModelStats,
		providers:        providers,
		skillCatalog:     config.SkillCatalog,
		skillGateContext: config.SkillGateContext,
		memory:           memoryConfig,
	}
	if config.ContextMaxChars > 0 {
		var next agentcore.ContextProjector
		if memoryConfig != nil {
			next = memoryConfig.Projector()
		}
		if runner.contextProjector, err = newContextProjector(config, next); err != nil {
			closeStores()
			return nil, err
		}
	}
	agentApprovals, err := newHostAgentApprovalService(runs, approvalCipher, runner, approvalKeychain)
	if err != nil {
		closeStores()
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
		healthCloser:            healthCloser,
		backendClosers:          backendClosers,
		audit:                   audit,
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
	if !s.healthClosed && s.healthCloser != nil {
		s.healthClosed = true
		s.healthCloseErr = s.healthCloser.Close()
	}
	if !s.backendsClosed {
		s.backendsClosed = true
		for _, closer := range s.backendClosers {
			s.backendCloseErr = errors.Join(s.backendCloseErr, closer.Close())
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(s.workflowCloseErr, s.runCloseErr, s.healthCloseErr, s.backendCloseErr, err)
	}
	return errors.Join(s.workflowCloseErr, s.runCloseErr, s.healthCloseErr, s.backendCloseErr)
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
	mux.HandleFunc("GET /workflows/{id}/trace", s.handleGetWorkflowTrace)
	mux.HandleFunc("GET /agent-runs/{id}", s.handleGetAgentRun)
	mux.HandleFunc("GET /llmkit/models", s.handleModels)
	mux.HandleFunc("GET /workers/queued", s.handleQueuedWorker)
	if s.memory != nil {
		mux.HandleFunc("GET /workers/memory", s.handleMemoryWorkers)
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
	return limitRequestBodies(mux)
}

func limitRequestBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
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
		writeDecodeError(w, err)
		return false
	}
	return true
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
}

func decodeJSONStrict(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeDecodeError(w, err)
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
	if errors.Is(err, workflowkit.ErrRunExists) {
		writeWorkflowExists(w)
		return
	}
	if errors.Is(err, workflowkit.ErrApprovalMismatch) {
		writeError(w, http.StatusConflict, "approval_mismatch", "workflow is not waiting on final approval")
		return
	}
	if errors.Is(err, workflowkit.ErrInvalidTransition) || errors.Is(err, workflowkit.ErrRunPreempted) {
		writeError(w, http.StatusConflict, "workflow_conflict", "workflow is not in a state that allows this operation")
		return
	}
	writeInternalError(w, "workflow_error", err)
}

// writeInternalError reports a server-side failure without its error text,
// which can hold file paths, SQL, or driver details. The error goes to the
// server log instead.
func writeInternalError(w http.ResponseWriter, code string, err error) {
	log.Printf("host-api %s: %v", code, err)
	writeError(w, http.StatusInternalServerError, code, "internal server error")
}

// maxRequestBodyBytes bounds every request body. Workflow input is text
// written to an artifact; larger documents belong behind an artifact ref.
const maxRequestBodyBytes = 4 << 20

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

func (s *Server) auditStore() auditBackend {
	if s.audit != nil {
		return s.audit
	}
	return fileAudit{home: s.llmHome}
}
