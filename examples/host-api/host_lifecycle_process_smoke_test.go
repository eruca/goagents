//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eruca/goagents/workflowkit"
	workflowsql "github.com/eruca/goagents/workflowkit/sqlitestore"
)

func TestHostAPILifecycleProcessGracefulDrainAndRestart(t *testing.T) {
	binary := buildHostBinary(t)
	provider := newMVPProviderStub(t, mvpProviderReady)
	barrier := newProviderBarrier()
	provider.SetBarrier(barrier)
	oidc := newOIDCTestProvider(t)
	runtimeHome := t.TempDir()
	writeMVPLLMKitConfig(t, runtimeHome, provider.URL())

	firstProcess := startLifecycleMVPHostProcess(
		t,
		binary,
		runtimeHome,
		oidc.issuer,
		provider,
		"graceful-a",
		5*time.Second,
	)
	first := submitQueuedLifecycleWorkflow(t, firstProcess, "wf-lifecycle-graceful-first")
	second := submitQueuedLifecycleWorkflow(t, firstProcess, "wf-lifecycle-graceful-second")
	waitForLifecycleProviderRequests(t, provider, barrier, 1)

	if err := signalHostProcess(firstProcess, syscall.SIGTERM); err != nil {
		t.Fatalf("signal first host process: %v", err)
	}
	if err := waitForHostListenerClosed(firstProcess, 2*time.Second); err != nil {
		t.Fatalf("wait for first host listener to close: %v", err)
	}
	close(barrier.release)
	exitCode, err := waitHostProcess(firstProcess, 8*time.Second)
	if err != nil || exitCode != 0 {
		t.Fatalf("wait first host process = (%d, %v), want (0, nil)\n%s", exitCode, err, firstProcess.output.String())
	}
	assertLifecycleProcessCleanExit(t, firstProcess, provider)
	if got := barrier.Cancellations(); got != 0 {
		t.Fatalf("provider cancellations = %d, want 0 after released graceful request", got)
	}

	afterFirstExit := readLifecycleWorkflows(t, runtimeHome, first.ID, second.ID)
	assertWaitingApprovalLifecycleWorkflow(t, afterFirstExit[first.ID])
	assertPendingLifecycleWorkflow(t, afterFirstExit[second.ID])
	firstStable := afterFirstExit[first.ID]

	provider.SetBarrier(nil)
	secondProcess := startLifecycleMVPHostProcess(
		t,
		binary,
		runtimeHome,
		oidc.issuer,
		provider,
		"graceful-b",
		5*time.Second,
	)
	waitForLifecycleWorkflowStatus(t, secondProcess, second.ID, workflowkit.StatusWaitingApproval)
	waitForLifecycleProviderRequestCount(t, provider, 2)
	if err := signalHostProcess(secondProcess, os.Interrupt); err != nil {
		t.Fatalf("signal second host process: %v", err)
	}
	exitCode, err = waitHostProcess(secondProcess, 8*time.Second)
	if err != nil || exitCode != 0 {
		t.Fatalf("wait second host process = (%d, %v), want (0, nil)\n%s", exitCode, err, secondProcess.output.String())
	}
	assertLifecycleProcessCleanExit(t, secondProcess, provider)

	afterRestart := readLifecycleWorkflows(t, runtimeHome, first.ID, second.ID)
	if !reflect.DeepEqual(afterRestart[first.ID], firstStable) {
		t.Fatalf(
			"first workflow changed across restart:\nbefore=%+v\nafter=%+v",
			firstStable,
			afterRestart[first.ID],
		)
	}
	assertWaitingApprovalLifecycleWorkflow(t, afterRestart[second.ID])
	if got := len(provider.Requests()); got != 2 {
		t.Fatalf("provider requests = %d, want exact 2", got)
	}
}

func TestHostAPILifecycleProcessDrainTimeoutFailsActiveWorkflow(t *testing.T) {
	binary := buildHostBinary(t)
	provider := newMVPProviderStub(t, mvpProviderReady)
	barrier := newProviderBarrier()
	provider.SetBarrier(barrier)
	oidc := newOIDCTestProvider(t)
	runtimeHome := t.TempDir()
	writeMVPLLMKitConfig(t, runtimeHome, provider.URL())

	firstProcess := startLifecycleMVPHostProcess(
		t,
		binary,
		runtimeHome,
		oidc.issuer,
		provider,
		"timeout-a",
		time.Second,
	)
	created := submitQueuedLifecycleWorkflow(t, firstProcess, "wf-lifecycle-timeout")
	waitForLifecycleProviderRequests(t, provider, barrier, 1)

	if err := signalHostProcess(firstProcess, syscall.SIGTERM); err != nil {
		t.Fatalf("signal timeout host process: %v", err)
	}
	if err := waitForHostListenerClosed(firstProcess, 2*time.Second); err != nil {
		t.Fatalf("wait for timeout host listener to close: %v", err)
	}
	exitCode, err := waitHostProcess(firstProcess, 8*time.Second)
	if err != nil || exitCode != 5 {
		t.Fatalf("wait timeout host process = (%d, %v), want (5, nil)\n%s", exitCode, err, firstProcess.output.String())
	}
	waitForLifecycleProviderCancellations(t, barrier, 1)
	assertLifecycleProcessShutdownTimeoutExit(t, firstProcess, provider)

	afterTimeout := readLifecycleWorkflows(t, runtimeHome, created.ID)
	assertFailedLifecycleWorkflowAfterShutdown(t, afterTimeout[created.ID])
	failedStable := afterTimeout[created.ID]

	provider.SetBarrier(nil)
	secondProcess := startLifecycleMVPHostProcess(
		t,
		binary,
		runtimeHome,
		oidc.issuer,
		provider,
		"timeout-b",
		5*time.Second,
	)
	waitForLifecycleWorkerClaimAttempt(t, secondProcess)
	observeNoAdditionalLifecycleProviderRequests(t, provider, 1, 500*time.Millisecond)
	if err := signalHostProcess(secondProcess, os.Interrupt); err != nil {
		t.Fatalf("signal restarted timeout host process: %v", err)
	}
	exitCode, err = waitHostProcess(secondProcess, 8*time.Second)
	if err != nil || exitCode != 0 {
		t.Fatalf("wait restarted timeout host process = (%d, %v), want (0, nil)\n%s", exitCode, err, secondProcess.output.String())
	}
	assertLifecycleProcessCleanExit(t, secondProcess, provider)

	afterRestart := readLifecycleWorkflows(t, runtimeHome, created.ID)
	if !reflect.DeepEqual(afterRestart[created.ID], failedStable) {
		t.Fatalf(
			"failed workflow changed without requeue:\nbefore=%+v\nafter=%+v",
			failedStable,
			afterRestart[created.ID],
		)
	}
	if got := len(provider.Requests()); got != 1 {
		t.Fatalf("provider requests = %d, want exact 1", got)
	}
}

func TestHostAPILifecycleProcessSecondSignalForcesImmediately(t *testing.T) {
	const drainTimeout = 5 * time.Second

	binary := buildHostBinary(t)
	provider := newMVPProviderStub(t, mvpProviderReady)
	barrier := newProviderBarrier()
	provider.SetBarrier(barrier)
	oidc := newOIDCTestProvider(t)
	runtimeHome := t.TempDir()
	writeMVPLLMKitConfig(t, runtimeHome, provider.URL())

	process := startLifecycleMVPHostProcess(
		t,
		binary,
		runtimeHome,
		oidc.issuer,
		provider,
		"second-signal",
		drainTimeout,
	)
	created := submitQueuedLifecycleWorkflow(t, process, "wf-lifecycle-second-signal")
	waitForLifecycleProviderRequests(t, provider, barrier, 1)

	fullDrainTimer := time.NewTimer(drainTimeout)
	defer fullDrainTimer.Stop()
	if err := signalHostProcess(process, syscall.SIGTERM); err != nil {
		t.Fatalf("signal second-signal host process for drain: %v", err)
	}
	if err := waitForHostListenerClosed(process, 2*time.Second); err != nil {
		t.Fatalf("wait for second-signal host listener to close: %v", err)
	}
	if err := signalHostProcess(process, os.Interrupt); err != nil {
		t.Fatalf("send second signal to host process: %v", err)
	}
	exitCode, err := waitForLifecycleProcessBeforeDrainTimeout(
		t,
		process,
		10*time.Second,
		fullDrainTimer.C,
	)
	if err != nil || exitCode != 5 {
		t.Fatalf("wait second-signal host process = (%d, %v), want (5, nil)\n%s", exitCode, err, process.output.String())
	}
	waitForLifecycleProviderCancellations(t, barrier, 1)
	assertLifecycleProcessShutdownTimeoutExit(t, process, provider)

	afterExit := readLifecycleWorkflows(t, runtimeHome, created.ID)
	assertFailedLifecycleWorkflowAfterShutdown(t, afterExit[created.ID])
	if got := len(provider.Requests()); got != 1 {
		t.Fatalf("provider requests = %d, want exact 1", got)
	}
}

func startLifecycleMVPHostProcess(
	t *testing.T,
	binary string,
	runtimeHome string,
	issuer string,
	provider *mvpProviderStub,
	identity string,
	shutdownTimeout time.Duration,
) *hostProcess {
	t.Helper()
	keychainService := localApprovalKeychainService + ".smoke.lifecycle." + identity
	keyID := "lifecycle-" + identity
	environment := mvpHostEnvironment("")
	environment[hostShutdownTimeoutEnv] = shutdownTimeout.String()
	return startHostProcessWithEnvAndRedactions(
		t,
		binary,
		runtimeHome,
		issuer,
		keychainService,
		keyID,
		environment,
		[]string{
			mvpProviderAPIKey,
			provider.URL(),
			issuer,
			keychainService,
			keyID,
		},
	)
}

func submitQueuedLifecycleWorkflow(t *testing.T, process *hostProcess, id string) workflowResponse {
	t.Helper()
	created, status := processJSON[workflowResponse](
		t,
		process,
		http.MethodPost,
		"/workflows",
		map[string]any{
			"id":       id,
			"input":    "Lifecycle process workflow " + id,
			"run_mode": string(RunModeQueued),
			"task_profile": map[string]any{
				"complexity": "simple",
				"privacy":    "cloud_allowed",
			},
		},
		"",
	)
	if status != http.StatusAccepted ||
		created.Status != string(workflowkit.StatusPending) ||
		created.RunMode != string(RunModeQueued) {
		t.Fatalf("create queued lifecycle workflow status=%d workflow=%+v, want 202 pending", status, created)
	}
	return created
}

func waitForLifecycleProviderRequests(
	t *testing.T,
	provider *mvpProviderStub,
	barrier *providerBarrier,
	count int,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	requests, err := provider.WaitForRequests(ctx, count)
	if err != nil {
		t.Fatalf("wait for %d lifecycle provider requests: %v", count, err)
	}
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatalf("wait for lifecycle provider barrier entry: %v", ctx.Err())
	}
	if len(requests) != count {
		t.Fatalf("provider requests = %d, want exact %d", len(requests), count)
	}
}

func waitForLifecycleProviderRequestCount(t *testing.T, provider *mvpProviderStub, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	requests, err := provider.WaitForRequests(ctx, count)
	if err != nil {
		t.Fatalf("wait for %d lifecycle provider requests: %v", count, err)
	}
	if len(requests) != count {
		t.Fatalf("provider requests = %d, want exact %d", len(requests), count)
	}
}

func waitForLifecycleProviderCancellations(
	t *testing.T,
	barrier *providerBarrier,
	count int,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := barrier.WaitForCancellations(ctx, count); err != nil {
		t.Fatalf("wait for %d lifecycle provider cancellations: %v", count, err)
	}
	if got := barrier.Cancellations(); got != count {
		t.Fatalf("provider cancellations = %d, want exact %d", got, count)
	}
}

func waitForLifecycleWorkflowStatus(
	t *testing.T,
	process *hostProcess,
	id string,
	status workflowkit.Status,
) workflowResponse {
	t.Helper()
	return waitForProcessWorkflowStatus(t, process, id, status)
}

func waitForLifecycleWorkerClaimAttempt(t *testing.T, process *hostProcess) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()

	var last queuedWorkerResponse
	for {
		last, _ = processJSON[queuedWorkerResponse](
			t,
			process,
			http.MethodGet,
			"/workers/queued",
			nil,
			"",
		)
		if last.Started && last.ClaimAttempts > 0 {
			return
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatalf("queued worker status = %+v, want started with a claim attempt", last)
		}
	}
}

func observeNoAdditionalLifecycleProviderRequests(
	t *testing.T,
	provider *mvpProviderStub,
	expected int,
	window time.Duration,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), window)
	defer cancel()
	requests, err := provider.WaitForRequests(ctx, expected+1)
	if err == nil {
		t.Fatalf(
			"provider received an unexpected request during %s observation: %+v",
			window,
			requests,
		)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("observe provider requests: %v, want bounded deadline", err)
	}
	if got := len(provider.Requests()); got != expected {
		t.Fatalf("provider requests = %d, want exact %d after observation", got, expected)
	}
}

type lifecycleProcessWaitResult struct {
	exitCode int
	err      error
}

func waitForLifecycleProcessBeforeDrainTimeout(
	t *testing.T,
	process *hostProcess,
	waitTimeout time.Duration,
	fullDrainTimeout <-chan time.Time,
) (int, error) {
	t.Helper()
	result := make(chan lifecycleProcessWaitResult, 1)
	go func() {
		exitCode, err := waitHostProcess(process, waitTimeout)
		result <- lifecycleProcessWaitResult{exitCode: exitCode, err: err}
	}()

	select {
	case completed := <-result:
		return completed.exitCode, completed.err
	case <-fullDrainTimeout:
		t.Fatal("full drain timeout elapsed before the second signal stopped the host process")
		return -1, errors.New("full drain timeout elapsed first")
	}
}

func assertLifecycleProcessCleanExit(
	t *testing.T,
	process *hostProcess,
	provider *mvpProviderStub,
) {
	t.Helper()
	stdout := "host_api_addr=" + strings.TrimPrefix(process.baseURL, "http://") + "\n"
	if got := process.stdout.String(); got != stdout {
		t.Fatalf("host stdout = %q, want runtime address %q", got, stdout)
	}
	if got := process.stderr.String(); got != "" {
		t.Fatalf("host stderr = %q, want empty", got)
	}
	if got := process.output.String(); got != stdout {
		t.Fatalf("host combined output = %q, want only runtime address %q", got, stdout)
	}
	assertLifecycleProcessOutputSafe(t, process, provider)
}

func assertLifecycleProcessShutdownTimeoutExit(
	t *testing.T,
	process *hostProcess,
	provider *mvpProviderStub,
) {
	t.Helper()
	stdout := "host_api_addr=" + strings.TrimPrefix(process.baseURL, "http://") + "\n"
	if got := process.stdout.String(); got != stdout {
		t.Fatalf("host stdout = %q, want runtime address %q", got, stdout)
	}
	stderr := process.stderr.String()
	assertExactHostExitJSON(t, stderr, "shutdown_timeout", "shutdown timed out")
	combined := process.output.String()
	if combined != stdout+stderr && combined != stderr+stdout {
		t.Fatalf(
			"host combined output = %q, want runtime address and exact shutdown error",
			combined,
		)
	}
	assertLifecycleProcessOutputSafe(t, process, provider)
}

func assertLifecycleProcessOutputSafe(
	t *testing.T,
	process *hostProcess,
	provider *mvpProviderStub,
) {
	t.Helper()
	for name, buffer := range map[string]*lockedBuffer{
		"stdout": process.stdout,
		"stderr": process.stderr,
		"output": process.output,
	} {
		for sensitiveName, sensitive := range map[string]string{
			"API key":      mvpProviderAPIKey,
			"provider URL": provider.URL(),
		} {
			if buffer.ContainsSensitive(sensitive) {
				t.Fatalf("%s retained leaked %s", name, sensitiveName)
			}
		}
	}
	lower := strings.ToLower(process.output.String())
	for _, forbidden := range []string{"panic", "goroutine", "checkpoint", "token"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("host output contains forbidden %q diagnostic: %q", forbidden, process.output.String())
		}
	}
}

func assertFailedLifecycleWorkflowAfterShutdown(t *testing.T, run workflowkit.WorkflowRun) {
	t.Helper()
	if run.Status != workflowkit.StatusFailed ||
		run.Error != hostShutdownTimeoutCode ||
		run.LeaseOwner != "" ||
		!run.LeaseUntil.IsZero() {
		t.Fatalf("workflow = %+v, want failed host shutdown timeout without lease", run)
	}
}

func readLifecycleWorkflows(
	t *testing.T,
	runtimeHome string,
	ids ...string,
) map[string]workflowkit.WorkflowRun {
	t.Helper()
	store, err := workflowsql.Open(filepath.Join(runtimeHome, "workflow.db"))
	if err != nil {
		t.Fatalf("open lifecycle workflow store: %v", err)
	}
	runs := make(map[string]workflowkit.WorkflowRun, len(ids))
	for _, id := range ids {
		run, getErr := store.Get(t.Context(), id)
		if getErr != nil {
			_ = store.Close()
			t.Fatalf("read lifecycle workflow %s: %v", id, getErr)
		}
		runs[id] = run
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close lifecycle workflow store: %v", err)
	}
	return runs
}

func assertWaitingApprovalLifecycleWorkflow(t *testing.T, run workflowkit.WorkflowRun) {
	t.Helper()
	if run.Status != workflowkit.StatusWaitingApproval ||
		run.Error != "" ||
		run.OutputRef == "" ||
		run.AgentRunID == "" ||
		run.ApprovalRef == "" ||
		run.WaitingReason == "" ||
		run.LeaseOwner != "" ||
		!run.LeaseUntil.IsZero() {
		t.Fatalf("workflow = %+v, want stable waiting approval without lease", run)
	}
}

func assertPendingLifecycleWorkflow(t *testing.T, run workflowkit.WorkflowRun) {
	t.Helper()
	if run.Status != workflowkit.StatusPending ||
		run.Error != "" ||
		run.OutputRef != "" ||
		run.AgentRunID != "" ||
		run.ApprovalRef != "" ||
		run.WaitingReason != "" ||
		run.CurrentStep != "" ||
		len(run.CompletedSteps) != 0 ||
		len(run.StepAttempts) != 0 ||
		len(run.StepRecords) != 0 ||
		run.LeaseOwner != "" ||
		!run.LeaseUntil.IsZero() {
		t.Fatalf("workflow = %+v, want untouched pending workflow without lease", run)
	}
}

func TestHostAPILifecycleProcessConfigFailure(t *testing.T) {
	const sentinel = "host-config-sensitive-invalid-duration"
	binary := buildHostBinary(t)
	command := exec.Command(binary)
	command.Env = overrideEnvironment(map[string]string{
		hostShutdownTimeoutEnv: sentinel,
	})
	process := startCapturedHostCommand(t, command, sentinel)
	cleanupKilledHostProcess(t, process)

	exitCode, err := waitHostProcess(process, 5*time.Second)
	if err != nil || exitCode != 2 {
		t.Fatalf("wait host config failure = (%d, %v), want (2, nil)", exitCode, err)
	}
	assertLifecycleProcessFailure(
		t,
		process,
		sentinel,
		"config_failed",
		"host configuration failed",
	)
}

func TestHostAPILifecycleProcessListenFailure(t *testing.T) {
	const sentinel = "host-listen-sensitive-sentinel"
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy loopback address: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	binary := buildHostBinary(t)
	provider := newOIDCTestProvider(t)
	runtimeHome := t.TempDir()
	before := seedPendingLifecycleWorkflow(t, runtimeHome)
	keychainService := localApprovalKeychainService + ".smoke.lifecycle." + sentinel
	keyID := "lifecycle-" + sentinel
	command := exec.Command(binary)
	command.Env = overrideEnvironment(map[string]string{
		"HOST_API_ADDR":                          listener.Addr().String(),
		"HOST_RUNTIME_HOME":                      runtimeHome,
		"HOST_API_OIDC_ISSUER":                   provider.issuer,
		"HOST_API_OIDC_AUDIENCE":                 "host-api",
		hostShutdownTimeoutEnv:                   time.Second.String(),
		"HOST_API_AGENT_APPROVAL_SWEEP_INTERVAL": time.Hour.String(),
		"HOST_API_QUEUED_LEASE_DURATION":         time.Minute.String(),
		agentApprovalKeychainServiceEnv:          keychainService,
		agentApprovalKeyIDEnv:                    keyID,
		"LLMKIT_HOME":                            filepath.Join(runtimeHome, ".llmkit"),
		hostAPISkillRootEnv:                      "",
	})
	process := startCapturedHostCommand(t, command, sentinel, keychainService, keyID)
	cleanupKilledHostProcess(t, process)

	exitCode, err := waitHostProcess(process, 5*time.Second)
	if err != nil || exitCode != 3 {
		t.Fatalf("wait host listen failure = (%d, %v), want (3, nil)", exitCode, err)
	}
	assertLifecycleProcessFailure(
		t,
		process,
		sentinel,
		"listen_failed",
		"host API listen failed",
	)
	assertPendingLifecycleWorkflowUnchanged(t, runtimeHome, before)
}

func seedPendingLifecycleWorkflow(t *testing.T, runtimeHome string) workflowkit.WorkflowRun {
	t.Helper()
	store, err := workflowsql.Open(filepath.Join(runtimeHome, "workflow.db"))
	if err != nil {
		t.Fatalf("open lifecycle workflow store: %v", err)
	}
	run := workflowkit.WorkflowRun{
		ID:       "wf-listen-failure-must-remain-pending",
		Status:   workflowkit.StatusPending,
		InputRef: "input-before-listen",
		Metadata: map[string]any{
			"run_mode": "queued",
			"marker":   "before-listen",
		},
	}
	if err := store.Save(context.Background(), run); err != nil {
		_ = store.Close()
		t.Fatalf("seed pending lifecycle workflow: %v", err)
	}
	persisted, err := store.Get(context.Background(), run.ID)
	if err != nil {
		_ = store.Close()
		t.Fatalf("read seeded lifecycle workflow: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seeded lifecycle workflow store: %v", err)
	}
	return persisted
}

func assertPendingLifecycleWorkflowUnchanged(
	t *testing.T,
	runtimeHome string,
	before workflowkit.WorkflowRun,
) {
	t.Helper()
	store, err := workflowsql.Open(filepath.Join(runtimeHome, "workflow.db"))
	if err != nil {
		t.Fatalf("reopen lifecycle workflow store: %v", err)
	}
	defer store.Close()
	after, err := store.Get(context.Background(), before.ID)
	if err != nil {
		t.Fatalf("read lifecycle workflow after listen failure: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("workflow changed after listen failure:\nbefore=%+v\nafter=%+v", before, after)
	}
	if after.Status != workflowkit.StatusPending ||
		after.LeaseOwner != "" ||
		!after.LeaseUntil.IsZero() ||
		after.CurrentStep != "" ||
		len(after.CompletedSteps) != 0 ||
		len(after.StepAttempts) != 0 ||
		len(after.StepRecords) != 0 {
		t.Fatalf("workflow advanced after listen failure: %+v", after)
	}
}

func assertLifecycleProcessFailure(
	t *testing.T,
	process *hostProcess,
	sentinel string,
	code string,
	message string,
) {
	t.Helper()
	if got := process.stdout.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	stderr := process.stderr.String()
	assertExactHostExitJSON(t, stderr, code, message)
	if strings.Contains(stderr, "panic") || strings.Contains(stderr, "goroutine") {
		t.Fatalf("stderr contains crash diagnostics: %q", stderr)
	}
	if got := process.output.String(); got != stderr {
		t.Fatalf("combined output = %q, want exact stderr %q", got, stderr)
	}
	for name, buffer := range map[string]*lockedBuffer{
		"stdout": process.stdout,
		"stderr": process.stderr,
		"output": process.output,
	} {
		if buffer.ContainsSensitive(sentinel) {
			t.Fatalf("%s retained leaked sentinel", name)
		}
		if strings.Contains(buffer.String(), sentinel) {
			t.Fatalf("%s exposed sentinel through redacted String", name)
		}
	}
}
