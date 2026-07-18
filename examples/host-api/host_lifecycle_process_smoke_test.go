//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/workflowkit"
	workflowsql "github.com/eruca/goagents/workflowkit/sqlitestore"
)

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
