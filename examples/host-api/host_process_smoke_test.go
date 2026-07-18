//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/runkit"
	runsql "github.com/eruca/goagents/runkit/sqlitestore"
	"github.com/eruca/goagents/workflowkit"
	workflowsql "github.com/eruca/goagents/workflowkit/sqlitestore"
)

func TestHostAPIProcessToolApprovalSurvivesRestart(t *testing.T) {
	requireInteractiveLoginKeychain(t)
	t.Setenv(hostAPISkillRootEnv, "relative/ambient-skills")
	provider := newOIDCTestProvider(t)
	binary := buildHostBinary(t)
	runtimeHome := t.TempDir()
	token := provider.mintToken(t, "operator-process", "host-api", time.Now().Add(time.Hour))
	smokeKeychainService := fmt.Sprintf(
		"%s.smoke.%d",
		localApprovalKeychainService,
		time.Now().UnixNano(),
	)
	cleanupKeychain := smokeKeychainCleanup(t, smokeKeychainService, localApprovalKeyID)
	t.Cleanup(cleanupKeychain)

	first := startHostProcess(t, binary, runtimeHome, provider.issuer, smokeKeychainService, localApprovalKeyID)
	created, status := processJSON[workflowResponse](t, first, http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-process-tool-approval",
		"input": "process-only approval checkpoint plaintext",
		"task_profile": map[string]any{
			"needs_tools": true,
		},
	}, "")
	if status != http.StatusAccepted || created.AgentApproval == nil || len(created.AgentApproval.Tools) != 1 {
		t.Fatalf("create status=%d response_error=%q agent_approval=%#v, want 202 and one pending tool", status, first.lastResponseError(), created.AgentApproval)
	}
	pending := created.AgentApproval.Tools[0]

	_, invalidStatus := processJSON[map[string]any](t, first, http.MethodPost, "/workflows/"+created.ID+"/agent-approve", map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": pending.ToolCallID,
			"tool":         pending.Tool,
			"allowed":      true,
		}},
	}, "Bearer invalid")
	if invalidStatus != http.StatusUnauthorized {
		t.Fatalf("invalid agent approval status=%d, want 401", invalidStatus)
	}
	stopHostProcess(t, first)
	assertPersistedPendingProcessCheckpoint(t, runtimeHome, created)

	second := startHostProcess(t, binary, runtimeHome, provider.issuer, smokeKeychainService, localApprovalKeyID)
	resumed, resumedStatus := processJSON[workflowResponse](t, second, http.MethodPost, "/workflows/"+created.ID+"/agent-approve", map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": pending.ToolCallID,
			"tool":         pending.Tool,
			"allowed":      true,
		}},
	}, "Bearer "+token)
	if resumedStatus != http.StatusOK || resumed.Status != string(workflowkit.StatusWaitingApproval) || resumed.AgentApproval != nil {
		t.Fatalf("resumed status=%d workflow=%#v, want final workflow approval pause", resumedStatus, resumed)
	}
	completed, completedStatus := processJSON[workflowResponse](t, second, http.MethodPost, "/workflows/"+created.ID+"/approve", map[string]any{
		"note": "process smoke accepted",
	}, "Bearer "+token)
	if completedStatus != http.StatusOK || completed.Status != string(workflowkit.StatusSucceeded) {
		t.Fatalf("final approval status=%d workflow=%#v, want succeeded", completedStatus, completed)
	}
	stopHostProcess(t, second)
	assertCompletedProcessWorkflow(t, runtimeHome, created)
	cleanupKeychain()
}

type hostProcess struct {
	baseURL  string
	client   *http.Client
	cmd      *exec.Cmd
	stdout   *lockedBuffer
	stderr   *lockedBuffer
	output   *lockedBuffer
	response lockedString

	stopOnce sync.Once
	stopErr  error

	waitOnce     sync.Once
	waitDone     chan struct{}
	waitExitCode int
	waitErr      error
}

type lockedBuffer struct {
	mu         sync.Mutex
	b          bytes.Buffer
	redactions []string
}

type lockedString struct {
	mu    sync.Mutex
	value string
}

func TestLockedBufferRedactsSensitiveValues(t *testing.T) {
	var buffer lockedBuffer
	buffer.SetRedactions("provider-key", "https://provider.example/v1")
	if _, err := buffer.Write([]byte("key=provider-key endpoint=https://provider.example/v1")); err != nil {
		t.Fatalf("write locked buffer: %v", err)
	}
	if !buffer.ContainsSensitive("provider-key") || !buffer.ContainsSensitive("https://provider.example/v1") {
		t.Fatal("locked buffer did not retain sensitive values for leak detection")
	}
	got := buffer.String()
	if strings.Contains(got, "provider-key") || strings.Contains(got, "https://provider.example/v1") {
		t.Fatalf("locked buffer returned unredacted sensitive output: %q", got)
	}
	if got != "key=[REDACTED] endpoint=[REDACTED]" {
		t.Fatalf("locked buffer output = %q, want redacted placeholders", got)
	}
}

func (s *lockedString) Set(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = value
}

func (s *lockedString) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value
}

func (p *hostProcess) lastResponseError() string {
	return p.response.String()
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}

func (b *lockedBuffer) SetRedactions(values ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, value := range values {
		if value != "" {
			b.redactions = append(b.redactions, value)
		}
	}
}

func (b *lockedBuffer) ContainsSensitive(value string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return value != "" && strings.Contains(b.b.String(), value)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := b.b.String()
	for _, sensitive := range b.redactions {
		value = strings.ReplaceAll(value, sensitive, "[REDACTED]")
	}
	return value
}

func buildHostBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "host-api")
	command := exec.Command("go", "build", "-o", binary, ".")
	command.Dir = "."
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build host binary: %v\n%s", err, output)
	}
	return binary
}

func requireInteractiveLoginKeychain(t *testing.T) {
	t.Helper()
	defaultKeychain := exec.Command("security", "default-keychain", "-d", "user")
	rawPath, err := defaultKeychain.Output()
	if err != nil {
		t.Skip("host process smoke requires an accessible macOS login Keychain")
	}
	keychainPath := strings.Trim(strings.TrimSpace(string(rawPath)), "\"")
	if keychainPath == "" {
		t.Skip("host process smoke requires an accessible macOS login Keychain")
	}
	probe := exec.Command("security", "show-keychain-info", keychainPath)
	if err := probe.Run(); err != nil {
		t.Skip("host process smoke requires an unlocked macOS login Keychain")
	}
}

func startHostProcess(t *testing.T, binary, runtimeHome, issuer, keychainService, keyID string) *hostProcess {
	t.Helper()
	return startHostProcessWithEnv(t, binary, runtimeHome, issuer, keychainService, keyID, map[string]string{hostAPISkillRootEnv: ""})
}

func startHostProcessWithEnv(t *testing.T, binary, runtimeHome, issuer, keychainService, keyID string, extraEnvironment map[string]string) *hostProcess {
	t.Helper()
	return startHostProcessWithEnvAndRedactions(t, binary, runtimeHome, issuer, keychainService, keyID, extraEnvironment, nil)
}

func startHostProcessWithEnvAndRedactions(t *testing.T, binary, runtimeHome, issuer, keychainService, keyID string, extraEnvironment map[string]string, redactions []string) *hostProcess {
	t.Helper()
	address := freeLoopbackAddress(t)
	process := &hostProcess{
		baseURL: "http://" + address,
		client:  &http.Client{Timeout: time.Second},
		cmd:     exec.Command(binary),
		stdout:  &lockedBuffer{},
		stderr:  &lockedBuffer{},
		output:  &lockedBuffer{},
	}
	process.stdout.SetRedactions(redactions...)
	process.stderr.SetRedactions(redactions...)
	process.output.SetRedactions(redactions...)
	environment := make(map[string]string, len(extraEnvironment)+9)
	for name, value := range extraEnvironment {
		environment[name] = value
	}
	for name, value := range map[string]string{
		"HOST_API_ADDR":                          address,
		"HOST_RUNTIME_HOME":                      runtimeHome,
		"HOST_API_OIDC_ISSUER":                   issuer,
		"HOST_API_OIDC_AUDIENCE":                 "host-api",
		"HOST_API_AGENT_APPROVAL_SWEEP_INTERVAL": time.Hour.String(),
		"HOST_API_QUEUED_LEASE_DURATION":         time.Minute.String(),
		agentApprovalKeychainServiceEnv:          keychainService,
		agentApprovalKeyIDEnv:                    keyID,
		"LLMKIT_HOME":                            filepath.Join(runtimeHome, ".llmkit"),
	} {
		environment[name] = value
	}
	process.cmd.Env = overrideEnvironment(environment)
	process.cmd.Stdout = io.MultiWriter(process.stdout, process.output)
	process.cmd.Stderr = io.MultiWriter(process.stderr, process.output)
	if err := process.cmd.Start(); err != nil {
		t.Fatalf("start host process: %v", err)
	}
	cleanupKilledHostProcess(t, process)
	if err := waitForHostReady(process); err != nil {
		stopHostProcess(t, process)
		t.Fatalf("host process did not become ready: %v\n%s", err, process.output.String())
	}
	return process
}

type smokeKeychainCleanupReporter interface {
	Helper()
	Errorf(string, ...any)
}

type smokeKeychainDelete func(service, account string) ([]byte, error)

func smokeKeychainCleanup(t *testing.T, service, keyID string) func() {
	return smokeKeychainCleanupWithDelete(t, service, keyID, func(service, account string) ([]byte, error) {
		command := exec.Command(
			"security", "delete-generic-password",
			"-s", service,
			"-a", account,
		)
		return command.CombinedOutput()
	})
}

func smokeKeychainCleanupWithDelete(
	t smokeKeychainCleanupReporter,
	service string,
	keyID string,
	deleteItem smokeKeychainDelete,
) func() {
	t.Helper()
	var once sync.Once
	return func() {
		once.Do(func() {
			if !strings.HasPrefix(service, localApprovalKeychainService+".smoke.") {
				t.Errorf("refusing to delete non-smoke Keychain service %q", service)
				return
			}
			output, err := deleteItem(service, "approval-data-key:"+keyID)
			if err != nil && !bytes.Contains(output, []byte("could not be found")) {
				t.Errorf("delete smoke Keychain item: %v: %s", err, strings.TrimSpace(string(output)))
			}
		})
	}
}

func stopHostProcess(t *testing.T, process *hostProcess) {
	t.Helper()
	process.stopOnce.Do(func() {
		if err := signalHostProcess(process, os.Interrupt); err != nil {
			process.stopErr = fmt.Errorf("interrupt host process: %w", err)
			return
		}
		exitCode, err := waitHostProcess(process, 5*time.Second)
		switch {
		case err != nil:
			process.stopErr = err
		case exitCode != 0:
			process.stopErr = fmt.Errorf("host process exit code = %d, want 0", exitCode)
		}
	})
	if process.stopErr != nil {
		t.Fatalf("stop host process: %v\n%s", process.stopErr, process.output.String())
	}
}

func signalHostProcess(process *hostProcess, signal os.Signal) error {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return errors.New("host process is not started")
	}
	if err := process.cmd.Process.Signal(signal); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

var errHostProcessWaitTimeout = errors.New("timed out waiting for host process")

func waitHostProcess(process *hostProcess, timeout time.Duration) (int, error) {
	if process == nil || process.cmd == nil {
		return -1, errors.New("host process command is nil")
	}
	process.waitOnce.Do(func() {
		process.waitDone = make(chan struct{})
		go func() {
			err := process.cmd.Wait()
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				err = nil
			}
			process.waitExitCode = -1
			if process.cmd.ProcessState != nil {
				process.waitExitCode = process.cmd.ProcessState.ExitCode()
			}
			process.waitErr = err
			close(process.waitDone)
		}()
	})

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-process.waitDone:
		return process.waitExitCode, process.waitErr
	case <-timer.C:
		return -1, fmt.Errorf("%w after %s", errHostProcessWaitTimeout, timeout)
	}
}

type hostProcessCleanupReporter interface {
	Helper()
	Cleanup(func())
	Errorf(string, ...any)
}

func cleanupKilledHostProcess(t *testing.T, process *hostProcess) {
	t.Helper()
	cleanupKilledHostProcessWithTimeout(t, process, 5*time.Second)
}

func cleanupKilledHostProcessWithTimeout(
	t hostProcessCleanupReporter,
	process *hostProcess,
	timeout time.Duration,
) {
	t.Helper()
	t.Cleanup(func() {
		if err := signalHostProcess(process, os.Interrupt); err != nil {
			t.Errorf("signal host process during cleanup: %v", err)
		}
		if _, err := waitHostProcess(process, timeout); err == nil {
			return
		} else if !errors.Is(err, errHostProcessWaitTimeout) {
			t.Errorf("wait host process during cleanup: %v", err)
			return
		}

		killErr := process.cmd.Process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
		_, waitErr := waitHostProcess(process, 5*time.Second)
		t.Errorf(
			"host process cleanup required kill after %s (kill error: %v; reap error: %v)",
			timeout,
			killErr,
			waitErr,
		)
	})
}

func waitForHostReady(process *hostProcess) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := process.client.Get(process.baseURL + "/workers/queued")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("readiness endpoint did not return 200")
}

func waitForHostListenerClosed(process *hostProcess, timeout time.Duration) error {
	address := strings.TrimPrefix(process.baseURL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	dialer := net.Dialer{Timeout: 100 * time.Millisecond}

	for {
		connection, err := dialer.DialContext(ctx, "tcp4", address)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("host listener %s still accepts new connections after %s", address, timeout)
			}
			return nil
		}
		_ = connection.Close()

		select {
		case <-poll.C:
		case <-ctx.Done():
			return fmt.Errorf("host listener %s still accepts new connections after %s", address, timeout)
		}
	}
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback address: %v", err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func processJSON[T any](t *testing.T, process *hostProcess, method, path string, body any, authorization string) (T, int) {
	t.Helper()
	var result T
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s %s request: %v", method, path, err)
	}
	request, err := http.NewRequest(method, process.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build %s %s request: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := process.client.Do(request)
	if err != nil {
		t.Fatalf("request %s %s: %v\n%s", method, path, err, process.output.String())
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s %s response status=%d: %v", method, path, response.StatusCode, err)
	}
	if response.StatusCode >= http.StatusBadRequest {
		var failure errorResponse
		if json.Unmarshal(responseBody, &failure) == nil {
			process.response.Set(failure.Error + ": " + failure.Message)
		}
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		t.Fatalf("decode %s %s response status=%d: %v", method, path, response.StatusCode, err)
	}
	return result, response.StatusCode
}

func overrideEnvironment(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, overridden := overrides[name]; !overridden {
			environment = append(environment, entry)
		}
	}
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}
	return environment
}

func assertPersistedPendingProcessCheckpoint(t *testing.T, runtimeHome string, created workflowResponse) {
	t.Helper()
	runs, err := runsql.Open(filepath.Join(runtimeHome, "agent-runs.db"))
	if err != nil {
		t.Fatalf("open persisted run store: %v", err)
	}
	defer runs.Close()
	checkpoint, err := runs.GetCheckpoint(context.Background(), created.AgentApproval.CheckpointID, localApprovalTenant)
	if err != nil {
		t.Fatalf("get persisted checkpoint: %v", err)
	}
	if checkpoint.Status != runkit.CheckpointPending || len(checkpoint.Ciphertext) == 0 || bytes.Contains(checkpoint.Ciphertext, []byte("process-only approval checkpoint plaintext")) {
		t.Fatalf("checkpoint did not remain opaque and pending")
	}
}

func assertCompletedProcessWorkflow(t *testing.T, runtimeHome string, created workflowResponse) {
	t.Helper()
	runs, err := runsql.Open(filepath.Join(runtimeHome, "agent-runs.db"))
	if err != nil {
		t.Fatalf("open completed run store: %v", err)
	}
	defer runs.Close()
	run, err := runs.Get(context.Background(), created.AgentRunID)
	if err != nil {
		t.Fatalf("get completed agent run: %v", err)
	}
	checkpoint, err := runs.GetCheckpoint(context.Background(), created.AgentApproval.CheckpointID, localApprovalTenant)
	if err != nil {
		t.Fatalf("get consumed checkpoint: %v", err)
	}
	workflows, err := workflowsql.Open(filepath.Join(runtimeHome, "workflow.db"))
	if err != nil {
		t.Fatalf("open completed workflow store: %v", err)
	}
	defer workflows.Close()
	workflow, err := workflows.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get completed workflow: %v", err)
	}
	if run.Summary.Status != runkit.StatusSucceeded || run.Summary.ToolCalls != 1 {
		t.Fatalf("agent run summary=%#v, want one succeeded tool call", run.Summary)
	}
	if checkpoint.Status != runkit.CheckpointConsumed || checkpoint.Approval == nil || checkpoint.Approval.ApproverID != "operator-process" {
		t.Fatalf("checkpoint=%#v, want consumed checkpoint approved by verified subject", checkpoint)
	}
	if workflow.Status != workflowkit.StatusSucceeded || workflow.Metadata["approved_by"] != "operator-process" {
		t.Fatalf("workflow=%#v, want succeeded workflow approved by verified subject", workflow)
	}
}
