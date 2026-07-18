//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eruca/goagents/hostkit"
	"github.com/eruca/goagents/runkit"
	runsql "github.com/eruca/goagents/runkit/sqlitestore"
	"github.com/eruca/goagents/workflowkit"
	workflowsql "github.com/eruca/goagents/workflowkit/sqlitestore"
)

const (
	sideEffectHelperEnvironment       = "GOAGENTS_TEST_SIDE_EFFECT_HELPER"
	sideEffectRuntimeHomeEnvironment  = "GOAGENTS_TEST_SIDE_EFFECT_RUNTIME_HOME"
	sideEffectAddressEnvironment      = "GOAGENTS_TEST_SIDE_EFFECT_ADDRESS"
	sideEffectSinkURLEnvironment      = "GOAGENTS_TEST_SIDE_EFFECT_SINK_URL"
	sideEffectToolCallIDEnvironment   = "GOAGENTS_TEST_SIDE_EFFECT_TOOL_CALL_ID"
	sideEffectWorkflowIDEnvironment   = "GOAGENTS_TEST_SIDE_EFFECT_WORKFLOW_ID"
	sideEffectAgentRunIDEnvironment   = "GOAGENTS_TEST_SIDE_EFFECT_AGENT_RUN_ID"
	sideEffectProcessModeEnvironment  = "GOAGENTS_TEST_SIDE_EFFECT_PROCESS_MODE"
	sideEffectProcessModeBlocked      = "blocked"
	sideEffectProcessModeComplete     = "complete"
	sideEffectToolCallIDMetadataKey   = "external_tool_call_id"
	sideEffectRequestMetadataKey      = "request_metadata"
	sideEffectTestStepName            = "test_external_side_effect"
	sideEffectCheckpointProbeIDPrefix = "checkpoint-probe:"
)

// This proves fail-closed replay and explicit external idempotency. It does not
// prove cross-system exactly-once delivery by the Host.
func TestHostSideEffectBoundaryExplicitRequeueIsExternallyIdempotent(t *testing.T) {
	assertHostSideEffectTestIsolation(t)
	evidence := runHostSideEffectBoundarySmoke(t)
	if evidence.requestCount != 2 || evidence.appliedCount != 1 {
		t.Fatalf(
			"external side-effect evidence = requests %d, applied %d; want requests 2, applied 1",
			evidence.requestCount,
			evidence.appliedCount,
		)
	}
}

type hostSideEffectEvidence struct {
	requestCount int
	appliedCount int
}

func runHostSideEffectBoundarySmoke(t *testing.T) hostSideEffectEvidence {
	t.Helper()
	sink := newSideEffectSink()
	control := newSideEffectProcessControl(sink)
	sinkServer := httptest.NewServer(control.Handler())
	t.Cleanup(sinkServer.Close)
	t.Cleanup(control.Release)

	runtimeHome := t.TempDir()
	workflowID := "wf-side-effect-boundary"
	agentRunID := "agent-run-side-effect-boundary"
	toolCallID := fmt.Sprintf("tool-call-side-effect-%d", time.Now().UnixNano())

	first := startSideEffectHostProcess(t, sideEffectHostProcessConfig{
		runtimeHome: runtimeHome,
		sinkURL:     sinkServer.URL,
		toolCallID:  toolCallID,
		workflowID:  workflowID,
		agentRunID:  agentRunID,
		mode:        sideEffectProcessModeBlocked,
	})
	waitForSideEffectSignal(t, sink.committed, "first external side effect")
	waitForSideEffectSignal(t, control.blocked, "post-commit workflow barrier")

	if err := signalHostProcess(first, syscall.SIGTERM); err != nil {
		t.Fatalf("signal first side-effect host for drain: %v", err)
	}
	if err := waitForHostListenerClosed(first, 2*time.Second); err != nil {
		t.Fatalf("wait for first side-effect host listener to close: %v", err)
	}
	if err := signalHostProcess(first, os.Interrupt); err != nil {
		t.Fatalf("send second signal to side-effect host: %v", err)
	}
	waitForSideEffectSignal(t, control.executionCancelled, "execution context cancellation")
	control.Release()

	exitCode, err := waitHostProcess(first, 8*time.Second)
	if err != nil || exitCode != 5 {
		t.Fatalf(
			"wait first side-effect host = (%d, %v), want (5, nil)\n%s",
			exitCode,
			err,
			first.output.String(),
		)
	}
	assertSideEffectShutdownExit(t, first, sinkServer.URL, toolCallID)
	firstState := readSideEffectPersistedState(t, runtimeHome, workflowID, agentRunID)
	assertFailedSideEffectState(t, firstState, toolCallID)

	firstSnapshot := sink.Snapshot(toolCallID)
	if firstSnapshot.requestCount != 1 || firstSnapshot.appliedCount != 1 {
		t.Fatalf(
			"first external side-effect snapshot = requests %d, applied %d; want 1, 1",
			firstSnapshot.requestCount,
			firstSnapshot.appliedCount,
		)
	}

	second := startSideEffectHostProcess(t, sideEffectHostProcessConfig{
		runtimeHome: runtimeHome,
		sinkURL:     sinkServer.URL,
		workflowID:  workflowID,
		agentRunID:  agentRunID,
		mode:        sideEffectProcessModeComplete,
		redactions:  []string{toolCallID},
	})
	waitForLifecycleWorkerClaimAttempt(t, second)
	observeNoAdditionalSideEffectRequests(t, sink, toolCallID, 1, 500*time.Millisecond)

	requeued, status := processJSON[workflowResponse](
		t,
		second,
		http.MethodPost,
		"/workflows/"+workflowID+"/requeue",
		nil,
		"",
	)
	if status != http.StatusAccepted ||
		requeued.Status != string(workflowkit.StatusPending) ||
		requeued.RunMode != string(RunModeQueued) {
		t.Fatalf(
			"explicit requeue status=%d workflow status=%q run mode=%q, want 202 pending queued",
			status,
			requeued.Status,
			requeued.RunMode,
		)
	}
	waitForSideEffectRequestCount(t, sink, toolCallID, 2)
	waitForLifecycleWorkflowStatus(t, second, workflowID, workflowkit.StatusSucceeded)

	finalSnapshot := sink.Snapshot(toolCallID)
	if finalSnapshot.requestCount != 2 || finalSnapshot.appliedCount != 1 {
		t.Fatalf(
			"explicit retry snapshot = requests %d, applied %d; want 2, 1",
			finalSnapshot.requestCount,
			finalSnapshot.appliedCount,
		)
	}

	stopHostProcess(t, second)
	assertSideEffectCleanExit(t, second, sinkServer.URL, toolCallID)
	secondState := readSideEffectPersistedState(t, runtimeHome, workflowID, agentRunID)
	assertSucceededSideEffectState(t, secondState, toolCallID)
	return hostSideEffectEvidence{
		requestCount: finalSnapshot.requestCount,
		appliedCount: finalSnapshot.appliedCount,
	}
}

type sideEffectSink struct {
	mu                   sync.Mutex
	requestsByToolCallID map[string]int
	appliedToolCallIDs   map[string]struct{}
	committed            chan struct{}
}

type sideEffectSnapshot struct {
	requestCount int
	appliedCount int
}

func TestSideEffectSinkCommitsOnlyOnceAcrossToolCallIDs(t *testing.T) {
	sink := newSideEffectSink()
	if err := sink.Apply("first"); err != nil {
		t.Fatalf("apply first side effect: %v", err)
	}
	if err := sink.Apply("second"); err != nil {
		t.Fatalf("apply second side effect: %v", err)
	}
	if got := sink.Snapshot("second"); got.requestCount != 1 || got.appliedCount != 2 {
		t.Fatalf(
			"multi-identity sink snapshot = requests %d applied %d, want 1 and 2",
			got.requestCount,
			got.appliedCount,
		)
	}
}

func newSideEffectSink() *sideEffectSink {
	return &sideEffectSink{
		requestsByToolCallID: make(map[string]int),
		appliedToolCallIDs:   make(map[string]struct{}),
		committed:            make(chan struct{}),
	}
}

func (s *sideEffectSink) Apply(toolCallID string) error {
	if strings.TrimSpace(toolCallID) == "" {
		return errors.New("tool call id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestsByToolCallID[toolCallID]++
	if _, exists := s.appliedToolCallIDs[toolCallID]; !exists {
		firstCommit := len(s.appliedToolCallIDs) == 0
		s.appliedToolCallIDs[toolCallID] = struct{}{}
		if firstCommit {
			close(s.committed)
		}
	}
	return nil
}

func (s *sideEffectSink) Snapshot(toolCallID string) sideEffectSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sideEffectSnapshot{
		requestCount: s.requestsByToolCallID[toolCallID],
		appliedCount: len(s.appliedToolCallIDs),
	}
}

type sideEffectProcessControl struct {
	sink               *sideEffectSink
	blocked            chan struct{}
	executionCancelled chan struct{}
	release            chan struct{}
	blockedOnce        sync.Once
	cancelledOnce      sync.Once
	releaseOnce        sync.Once
}

func newSideEffectProcessControl(sink *sideEffectSink) *sideEffectProcessControl {
	return &sideEffectProcessControl{
		sink:               sink,
		blocked:            make(chan struct{}),
		executionCancelled: make(chan struct{}),
		release:            make(chan struct{}),
	}
}

func (c *sideEffectProcessControl) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apply", c.handleApply)
	mux.HandleFunc("POST /barrier/blocked", func(http.ResponseWriter, *http.Request) {
		c.blockedOnce.Do(func() { close(c.blocked) })
	})
	mux.HandleFunc("POST /barrier/cancelled", func(http.ResponseWriter, *http.Request) {
		c.cancelledOnce.Do(func() { close(c.executionCancelled) })
	})
	mux.HandleFunc("GET /barrier/release", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-c.release:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	})
	return mux
}

func (c *sideEffectProcessControl) handleApply(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var request struct {
		ToolCallID string `json:"tool_call_id"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid side-effect request", http.StatusBadRequest)
		return
	}
	if err := c.sink.Apply(request.ToolCallID); err != nil {
		http.Error(w, "invalid side-effect identity", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *sideEffectProcessControl) Release() {
	c.releaseOnce.Do(func() { close(c.release) })
}

type sideEffectHostProcessConfig struct {
	runtimeHome string
	address     string
	sinkURL     string
	toolCallID  string
	workflowID  string
	agentRunID  string
	mode        string
	redactions  []string
}

func startSideEffectHostProcess(t *testing.T, config sideEffectHostProcessConfig) *hostProcess {
	t.Helper()
	address := freeLoopbackAddress(t)
	command := exec.Command(
		os.Args[0],
		"-test.run=^TestHostSideEffectProcessHelper$",
		"-test.count=1",
	)
	command.Env = overrideEnvironment(map[string]string{
		sideEffectHelperEnvironment:      "1",
		sideEffectRuntimeHomeEnvironment: config.runtimeHome,
		sideEffectAddressEnvironment:     address,
		sideEffectSinkURLEnvironment:     config.sinkURL,
		sideEffectToolCallIDEnvironment:  config.toolCallID,
		sideEffectWorkflowIDEnvironment:  config.workflowID,
		sideEffectAgentRunIDEnvironment:  config.agentRunID,
		sideEffectProcessModeEnvironment: config.mode,
		hostAPISkillRootEnv:              "",
		queuedLeaseDurationEnv:           time.Minute.String(),
		agentApprovalSweepIntervalEnv:    time.Hour.String(),
	})
	redactions := append([]string{config.sinkURL, config.toolCallID}, config.redactions...)
	process := startCapturedHostCommand(t, command, redactions...)
	process.baseURL = "http://" + address
	process.client = &http.Client{Timeout: time.Second}
	// Register the leak guard before readiness or any other process barrier.
	cleanupKilledHostProcess(t, process)
	if err := waitForHostReady(process); err != nil {
		t.Fatalf("side-effect host did not become ready: %v\n%s", err, process.output.String())
	}
	return process
}

func TestHostSideEffectProcessHelper(t *testing.T) {
	if os.Getenv(sideEffectHelperEnvironment) != "1" {
		return
	}
	os.Exit(runSideEffectHostProcessHelper())
}

func runSideEffectHostProcessHelper() int {
	config, err := loadSideEffectHostProcessConfig(os.Getenv)
	if err != nil {
		_, _ = io.WriteString(os.Stderr, "side-effect test helper configuration failed\n")
		return 2
	}
	server, err := NewServer(Config{
		RuntimeHome:         config.runtimeHome,
		AgentApprovalCipher: &testApprovalCipher{},
	})
	if err != nil {
		_, _ = io.WriteString(os.Stderr, "side-effect test helper initialization failed\n")
		return 2
	}
	step := testSideEffectStep{
		sinkURL: config.sinkURL,
		mode:    config.mode,
		runs:    server.runs,
	}
	server.executor = workflowkit.NewExecutor(
		server.workflows,
		[]workflowkit.Step{step},
	)
	if config.mode == sideEffectProcessModeBlocked {
		if err := seedSideEffectWorkflow(context.Background(), server, config); err != nil {
			closeSideEffectServer(server)
			_, _ = io.WriteString(os.Stderr, "side-effect test helper seed failed\n")
			return 2
		}
	}

	interrupts, stopSignals := osSignalInterrupts()
	service := newHostAPIService(server, config.address, os.Stdout)
	result := hostkit.Run(context.Background(), service, interrupts, hostkit.Options{
		DrainTimeout:   5 * time.Second,
		CleanupTimeout: hostCleanupTimeout,
	})
	stopSignals()
	if result.ExitCode() != 0 {
		_ = hostkit.WriteError(os.Stderr, result)
	}
	return result.ExitCode()
}

func closeSideEffectServer(server *Server) {
	ctx, cancel := context.WithTimeout(context.Background(), hostCleanupTimeout)
	defer cancel()
	_ = server.Close(ctx)
}

func loadSideEffectHostProcessConfig(getenv func(string) string) (sideEffectHostProcessConfig, error) {
	config := sideEffectHostProcessConfig{
		runtimeHome: strings.TrimSpace(getenv(sideEffectRuntimeHomeEnvironment)),
		sinkURL:     strings.TrimSpace(getenv(sideEffectSinkURLEnvironment)),
		toolCallID:  strings.TrimSpace(getenv(sideEffectToolCallIDEnvironment)),
		workflowID:  strings.TrimSpace(getenv(sideEffectWorkflowIDEnvironment)),
		agentRunID:  strings.TrimSpace(getenv(sideEffectAgentRunIDEnvironment)),
		mode:        strings.TrimSpace(getenv(sideEffectProcessModeEnvironment)),
	}
	configAddress := strings.TrimSpace(getenv(sideEffectAddressEnvironment))
	if config.runtimeHome == "" || config.sinkURL == "" || configAddress == "" {
		return sideEffectHostProcessConfig{}, errors.New("required test helper field is empty")
	}
	if config.mode != sideEffectProcessModeBlocked &&
		config.mode != sideEffectProcessModeComplete {
		return sideEffectHostProcessConfig{}, errors.New("invalid test helper mode")
	}
	if config.mode == sideEffectProcessModeBlocked &&
		(config.toolCallID == "" || config.workflowID == "" || config.agentRunID == "") {
		return sideEffectHostProcessConfig{}, errors.New("blocked test helper seed field is empty")
	}
	config.address = configAddress
	return config, nil
}

func seedSideEffectWorkflow(
	ctx context.Context,
	server *Server,
	config sideEffectHostProcessConfig,
) error {
	seedCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	metadata := sideEffectWorkflowMetadata(config.toolCallID)
	if err := server.workflows.Save(seedCtx, workflowkit.WorkflowRun{
		ID:         config.workflowID,
		Status:     workflowkit.StatusPending,
		AgentRunID: config.agentRunID,
		Metadata:   metadata,
	}); err != nil {
		return err
	}
	return server.runs.Create(seedCtx, runkit.RunRecord{
		RunID:      config.agentRunID,
		WorkflowID: config.workflowID,
		TaskID:     config.workflowID,
		Status:     runkit.StatusRunning,
		Metadata:   sideEffectAgentRunMetadata(config.toolCallID),
	})
}

func sideEffectWorkflowMetadata(toolCallID string) map[string]any {
	return map[string]any{
		"run_mode":                      string(RunModeQueued),
		sideEffectToolCallIDMetadataKey: toolCallID,
	}
}

func sideEffectAgentRunMetadata(toolCallID string) map[string]any {
	return map[string]any{
		sideEffectToolCallIDMetadataKey: toolCallID,
		sideEffectRequestMetadataKey: map[string]any{
			sideEffectToolCallIDMetadataKey: toolCallID,
		},
	}
}

type testSideEffectStep struct {
	sinkURL string
	mode    string
	runs    runkit.Store
}

func (testSideEffectStep) Name() string {
	return sideEffectTestStepName
}

func (s testSideEffectStep) Run(
	ctx context.Context,
	run workflowkit.WorkflowRun,
) (workflowkit.StepResult, error) {
	toolCallID, ok := metadataToolCallID(run.Metadata)
	if !ok {
		return workflowkit.StepResult{}, errors.New("workflow is missing stable external identity")
	}
	persisted, err := s.runs.Get(ctx, run.AgentRunID)
	if err != nil {
		return workflowkit.StepResult{}, err
	}
	if !metadataHasToolCallID(persisted.Metadata, toolCallID) ||
		!requestMetadataHasToolCallID(persisted.Metadata, toolCallID) {
		return workflowkit.StepResult{}, errors.New("agent run is missing stable request identity")
	}
	persisted.Status = runkit.StatusRunning
	persisted.Summary = runkit.TerminalSummary{}
	if err := s.runs.Create(ctx, persisted); err != nil {
		return workflowkit.StepResult{}, err
	}
	if err := postSideEffect(ctx, s.sinkURL+"/apply", toolCallID); err != nil {
		return workflowkit.StepResult{}, err
	}

	switch s.mode {
	case sideEffectProcessModeComplete:
		if err := s.runs.Complete(ctx, persisted.RunID, runkit.TerminalSummary{
			Status:    runkit.StatusSucceeded,
			ToolCalls: 1,
		}); err != nil {
			return workflowkit.StepResult{}, err
		}
		return workflowkit.StepResult{
			Status:     workflowkit.StatusSucceeded,
			AgentRunID: persisted.RunID,
			Metadata:   sideEffectWorkflowMetadata(toolCallID),
		}, nil
	case sideEffectProcessModeBlocked:
		if err := postSideEffectControl(s.sinkURL + "/barrier/blocked"); err != nil {
			return workflowkit.StepResult{}, err
		}
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelWait()
		select {
		case <-ctx.Done():
		case <-waitCtx.Done():
			return workflowkit.StepResult{}, errors.New("execution cancellation barrier timed out")
		}
		if err := postSideEffectControl(s.sinkURL + "/barrier/cancelled"); err != nil {
			return workflowkit.StepResult{}, err
		}
		if err := waitForSideEffectRelease(s.sinkURL + "/barrier/release"); err != nil {
			return workflowkit.StepResult{}, err
		}
		return workflowkit.StepResult{
			Status:     workflowkit.StatusFailed,
			AgentRunID: persisted.RunID,
			Error:      hostShutdownTimeoutCode,
		}, ctx.Err()
	default:
		return workflowkit.StepResult{}, errors.New("unsupported test helper mode")
	}
}

func postSideEffect(ctx context.Context, url, toolCallID string) error {
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]string{"tool_call_id": toolCallID})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		url,
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<10))
	if response.StatusCode != http.StatusNoContent {
		return errors.New("side-effect sink rejected request")
	}
	return nil
}

func postSideEffectControl(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("side-effect control rejected barrier report")
	}
	return nil
}

func waitForSideEffectRelease(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("side-effect release barrier rejected")
	}
	return nil
}

func metadataHasToolCallID(metadata map[string]any, want string) bool {
	got, ok := metadataToolCallID(metadata)
	return ok && got == want
}

func metadataToolCallID(metadata map[string]any) (string, bool) {
	got, ok := metadata[sideEffectToolCallIDMetadataKey].(string)
	return got, ok && got != ""
}

func requestMetadataHasToolCallID(metadata map[string]any, want string) bool {
	request, ok := metadata[sideEffectRequestMetadataKey].(map[string]any)
	return ok && metadataHasToolCallID(request, want)
}

func waitForSideEffectSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("wait for %s: %v", name, ctx.Err())
	}
}

func waitForSideEffectRequestCount(
	t *testing.T,
	sink *sideEffectSink,
	toolCallID string,
	want int,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := sink.Snapshot(toolCallID)
		if snapshot.requestCount >= want {
			if snapshot.requestCount != want {
				t.Fatalf("side-effect requests = %d, want exact %d", snapshot.requestCount, want)
			}
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf(
				"side-effect requests = %d, want %d before deadline",
				snapshot.requestCount,
				want,
			)
		}
	}
}

func observeNoAdditionalSideEffectRequests(
	t *testing.T,
	sink *sideEffectSink,
	toolCallID string,
	expected int,
	window time.Duration,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), window)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := sink.Snapshot(toolCallID)
		if snapshot.requestCount != expected {
			t.Fatalf(
				"side-effect requests changed during no-replay observation: got %d want %d",
				snapshot.requestCount,
				expected,
			)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("no-replay observation ended with %v, want bounded deadline", ctx.Err())
			}
			return
		}
	}
}

type sideEffectPersistedState struct {
	workflow       workflowkit.WorkflowRun
	agentRun       runkit.RunRecord
	checkpointRows int
}

func readSideEffectPersistedState(
	t *testing.T,
	runtimeHome string,
	workflowID string,
	agentRunID string,
) sideEffectPersistedState {
	t.Helper()
	workflowStore, err := workflowsql.Open(filepath.Join(runtimeHome, "workflow.db"))
	if err != nil {
		t.Fatalf("open side-effect workflow store: %v", err)
	}
	workflow, err := workflowStore.Get(t.Context(), workflowID)
	if err != nil {
		_ = workflowStore.Close()
		t.Fatalf("read side-effect workflow: %v", err)
	}
	if err := workflowStore.Close(); err != nil {
		t.Fatalf("close side-effect workflow store: %v", err)
	}

	runStore, err := runsql.Open(filepath.Join(runtimeHome, "agent-runs.db"))
	if err != nil {
		t.Fatalf("open side-effect agent run store: %v", err)
	}
	agentRun, err := runStore.Get(t.Context(), agentRunID)
	if err != nil {
		_ = runStore.Close()
		t.Fatalf("read side-effect agent run: %v", err)
	}
	probeID := sideEffectCheckpointProbeIDPrefix + workflowID
	if _, err := runStore.GetCheckpoint(t.Context(), probeID, localApprovalTenant); !errors.Is(err, runkit.ErrCheckpointNotFound) {
		_ = runStore.Close()
		t.Fatalf("checkpoint probe error = %v, want not found", err)
	}
	if err := runStore.Close(); err != nil {
		t.Fatalf("close side-effect agent run store: %v", err)
	}

	checkpointRows := countSideEffectCheckpointRows(t, filepath.Join(runtimeHome, "agent-runs.db"))
	return sideEffectPersistedState{
		workflow:       workflow,
		agentRun:       agentRun,
		checkpointRows: checkpointRows,
	}
}

func countSideEffectCheckpointRows(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open checkpoint evidence database: %v", err)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM approval_checkpoints`).Scan(&count); err != nil {
		_ = db.Close()
		t.Fatalf("count approval checkpoints: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close checkpoint evidence database: %v", err)
	}
	return count
}

func assertFailedSideEffectState(
	t *testing.T,
	state sideEffectPersistedState,
	toolCallID string,
) {
	t.Helper()
	if state.workflow.Status != workflowkit.StatusFailed ||
		state.workflow.Error != hostShutdownTimeoutCode ||
		state.workflow.AgentRunID != state.agentRun.RunID ||
		state.workflow.StepAttempts[sideEffectTestStepName] != 1 ||
		state.workflow.LeaseOwner != "" ||
		!state.workflow.LeaseUntil.IsZero() {
		t.Fatalf(
			"workflow after forced shutdown = status %q error %q linked run %t attempts %d lease cleared %t",
			state.workflow.Status,
			state.workflow.Error,
			state.workflow.AgentRunID == state.agentRun.RunID,
			state.workflow.StepAttempts[sideEffectTestStepName],
			state.workflow.LeaseOwner == "" && state.workflow.LeaseUntil.IsZero(),
		)
	}
	if state.agentRun.Status != runkit.StatusFailed ||
		state.agentRun.Summary.Status != runkit.StatusFailed ||
		state.agentRun.Summary.AbortReason != hostShutdownTimeoutCode {
		t.Fatalf(
			"agent run after forced shutdown = status %q summary %q abort %q",
			state.agentRun.Status,
			state.agentRun.Summary.Status,
			state.agentRun.Summary.AbortReason,
		)
	}
	assertStableSideEffectMetadata(t, state, toolCallID)
	if state.checkpointRows != 0 {
		t.Fatalf("approval checkpoints after cleanup = %d, want 0", state.checkpointRows)
	}
}

func assertSucceededSideEffectState(
	t *testing.T,
	state sideEffectPersistedState,
	toolCallID string,
) {
	t.Helper()
	if state.workflow.Status != workflowkit.StatusSucceeded ||
		state.workflow.Error != "" ||
		state.workflow.AgentRunID != state.agentRun.RunID ||
		state.workflow.StepAttempts[sideEffectTestStepName] != 2 ||
		state.workflow.LeaseOwner != "" ||
		!state.workflow.LeaseUntil.IsZero() {
		t.Fatalf(
			"workflow after explicit retry = status %q error empty %t linked run %t attempts %d lease cleared %t",
			state.workflow.Status,
			state.workflow.Error == "",
			state.workflow.AgentRunID == state.agentRun.RunID,
			state.workflow.StepAttempts[sideEffectTestStepName],
			state.workflow.LeaseOwner == "" && state.workflow.LeaseUntil.IsZero(),
		)
	}
	if state.agentRun.Status != runkit.StatusSucceeded ||
		state.agentRun.Summary.Status != runkit.StatusSucceeded {
		t.Fatalf(
			"agent run after explicit retry = status %q summary %q",
			state.agentRun.Status,
			state.agentRun.Summary.Status,
		)
	}
	assertStableSideEffectMetadata(t, state, toolCallID)
	if state.checkpointRows != 0 {
		t.Fatalf("approval checkpoints after explicit retry = %d, want 0", state.checkpointRows)
	}
}

func assertStableSideEffectMetadata(
	t *testing.T,
	state sideEffectPersistedState,
	toolCallID string,
) {
	t.Helper()
	if !metadataHasToolCallID(state.workflow.Metadata, toolCallID) {
		t.Fatal("workflow metadata lost the stable external identity")
	}
	if !metadataHasToolCallID(state.agentRun.Metadata, toolCallID) ||
		!requestMetadataHasToolCallID(state.agentRun.Metadata, toolCallID) {
		t.Fatal("agent run or request metadata lost the stable external identity")
	}
}

func assertSideEffectShutdownExit(
	t *testing.T,
	process *hostProcess,
	sinkURL string,
	toolCallID string,
) {
	t.Helper()
	stdout := "host_api_addr=" + strings.TrimPrefix(process.baseURL, "http://") + "\n"
	if got := process.stdout.String(); got != stdout {
		t.Fatalf("side-effect host stdout = %q, want only runtime address", got)
	}
	stderr := process.stderr.String()
	assertExactHostExitJSON(t, stderr, "shutdown_timeout", "shutdown timed out")
	combined := process.output.String()
	if combined != stdout+stderr && combined != stderr+stdout {
		t.Fatalf("side-effect host combined output has unexpected content: %q", combined)
	}
	assertSideEffectOutputSafe(t, process, sinkURL, toolCallID)
}

func assertSideEffectCleanExit(
	t *testing.T,
	process *hostProcess,
	sinkURL string,
	toolCallID string,
) {
	t.Helper()
	stdout := "host_api_addr=" + strings.TrimPrefix(process.baseURL, "http://") + "\n"
	if got := process.stdout.String(); got != stdout {
		t.Fatalf("restarted side-effect host stdout = %q, want only runtime address", got)
	}
	if got := process.stderr.String(); got != "" {
		t.Fatalf("restarted side-effect host stderr = %q, want empty", got)
	}
	if got := process.output.String(); got != stdout {
		t.Fatalf("restarted side-effect host output = %q, want only runtime address", got)
	}
	assertSideEffectOutputSafe(t, process, sinkURL, toolCallID)
}

func assertSideEffectOutputSafe(
	t *testing.T,
	process *hostProcess,
	sinkURL string,
	toolCallID string,
) {
	t.Helper()
	for name, buffer := range map[string]*lockedBuffer{
		"stdout": process.stdout,
		"stderr": process.stderr,
		"output": process.output,
	} {
		if buffer.ContainsSensitive(sinkURL) || buffer.ContainsSensitive(toolCallID) {
			t.Fatalf("%s retained a sensitive side-effect sentinel", name)
		}
	}
	for _, forbidden := range []string{"panic", "goroutine", sideEffectTestStepName} {
		if strings.Contains(strings.ToLower(process.output.String()), strings.ToLower(forbidden)) {
			t.Fatalf("host output contains forbidden diagnostic %q", forbidden)
		}
	}
}
