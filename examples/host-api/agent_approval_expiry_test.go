package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
)

func TestHostAPIAgentApprovalExpiryFailsWorkflowAndRun(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-expiry"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-agent-tool-expiry")

	reconciled, err := server.ReconcileExpiredAgentApprovals(t.Context(), time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("ReconcileExpiredAgentApprovals: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", reconciled)
	}

	workflow, err := server.workflows.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get workflow: %v", err)
	}
	if workflow.Status != workflowkit.StatusFailed || workflow.Error != agentApprovalExpiredReason || agentApprovalFromMetadata(workflow.Metadata) != nil {
		t.Fatalf("expired workflow = %#v", workflow)
	}
	run, err := server.runs.Get(t.Context(), created.AgentRunID)
	if err != nil {
		t.Fatalf("Get agent run: %v", err)
	}
	if run.Summary.Status != runkit.StatusFailed || run.Summary.AbortReason != agentApprovalExpiredReason {
		t.Fatalf("expired agent run summary = %#v", run.Summary)
	}
	checkpoint, err := server.runs.(runkit.CheckpointStore).GetCheckpoint(t.Context(), created.AgentApproval.CheckpointID, localApprovalTenant)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if checkpoint.Status != runkit.CheckpointExpired {
		t.Fatalf("checkpoint status = %q, want expired", checkpoint.Status)
	}
	if _, err := server.artifacts.Get(t.Context(), "artifact:"+created.ID+":review-action"); !errors.Is(err, artifactkit.ErrArtifactNotFound) {
		t.Fatalf("review action exists after expiry: %v", err)
	}
}

func TestHostAPIAgentApprovalExpiryRetriesWaitingWorkflowAfterRunPersistenceFailure(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-expiry-retry"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-agent-tool-expiry-retry")
	originalRuns := server.runs
	server.runs = failingRunStore{err: errors.New("run summary unavailable")}
	if _, err := server.ReconcileExpiredAgentApprovals(t.Context(), time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("first expiry sweep error = nil, want run persistence failure")
	}
	waiting, err := server.workflows.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get workflow after failed sweep: %v", err)
	}
	if waiting.Status != workflowkit.StatusWaitingApproval || agentApprovalFromMetadata(waiting.Metadata) == nil {
		t.Fatalf("workflow after failed sweep = %#v, want still waiting for retry", waiting)
	}

	server.runs = originalRuns
	reconciled, err := server.ReconcileExpiredAgentApprovals(t.Context(), time.Now().Add(3*time.Hour))
	if err != nil {
		t.Fatalf("retry expiry sweep: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("retry reconciled = %d, want 1", reconciled)
	}
	workflow, err := server.workflows.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get workflow after retry: %v", err)
	}
	if workflow.Status != workflowkit.StatusFailed || agentApprovalFromMetadata(workflow.Metadata) != nil {
		t.Fatalf("workflow after retry = %#v, want failed without pending approval", workflow)
	}
}

func TestLoadAgentApprovalJanitorConfigRejectsInvalidIntervals(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: time.Minute},
		{name: "configured", value: "25ms", want: 25 * time.Millisecond},
		{name: "zero", value: "0s", wantErr: true},
		{name: "invalid", value: "not-a-duration", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config, err := loadAgentApprovalJanitorConfig(func(string) string { return tc.value })
			if tc.wantErr {
				if err == nil {
					t.Fatal("loadAgentApprovalJanitorConfig error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadAgentApprovalJanitorConfig: %v", err)
			}
			if config.interval != tc.want {
				t.Fatalf("interval = %s, want %s", config.interval, tc.want)
			}
		})
	}
}

func TestHostAPIAgentApprovalJanitorReconcilesExpiredWorkflow(t *testing.T) {
	t.Setenv(agentApprovalSweepIntervalEnv, "5ms")
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-expiry-janitor"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-agent-tool-expiry-janitor")
	if _, err := server.runs.(runkit.CheckpointStore).ExpireCheckpoints(t.Context(), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("ExpireCheckpoints: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	server.StartAgentApprovalJanitor(ctx)
	_ = waitForWorkflowStatus(t, server.Handler(), created.ID, workflowkit.StatusFailed)
	workflow, err := server.workflows.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get workflow after janitor: %v", err)
	}
	if workflow.Error != agentApprovalExpiredReason || agentApprovalFromMetadata(workflow.Metadata) != nil {
		t.Fatalf("janitor workflow = %#v", workflow)
	}
}

func TestAgentApprovalJanitorWaitsForCurrentScan(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	server.agentApprovalJanitorCfg.interval = time.Hour
	checkpoints := newControlledExpiryCheckpointStore(server.agentApprovals.checkpoints)
	server.agentApprovals.checkpoints = checkpoints

	ctx, cancel := context.WithCancel(t.Context())
	server.StartAgentApprovalJanitor(ctx)
	waitForJanitorSignal(t, checkpoints.started, "current scan start")
	cancel()
	waitForJanitorSignal(t, checkpoints.canceled, "current scan cancellation")

	waitParent, cancelWait := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelWait()
	waitCtx := newObservedDoneContext(waitParent)
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- server.WaitAgentApprovalJanitor(waitCtx)
	}()
	waitForJanitorSignal(t, waitCtx.observed, "janitor wait start")
	select {
	case err := <-waitResult:
		t.Fatalf("WaitAgentApprovalJanitor returned before the current scan stopped: %v", err)
	default:
	}

	close(checkpoints.release)
	if err := <-waitResult; err != nil {
		t.Fatalf("WaitAgentApprovalJanitor returned error: %v", err)
	}
}

func TestAgentApprovalJanitorDoesNotStartScanAfterIntakeCancellation(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	checkpoints := newControlledExpiryCheckpointStore(server.agentApprovals.checkpoints)
	server.agentApprovals.checkpoints = checkpoints

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	server.StartAgentApprovalJanitor(ctx)

	waitCtx, cancelWait := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelWait()
	if err := server.WaitAgentApprovalJanitor(waitCtx); err != nil {
		t.Fatalf("WaitAgentApprovalJanitor returned error: %v", err)
	}
	if calls := checkpoints.callCount(); calls != 0 {
		t.Fatalf("expiry scan calls = %d, want 0 after intake cancellation", calls)
	}
}

func TestAgentApprovalJanitorStartsOnlyOnce(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	server.agentApprovalJanitorCfg.interval = time.Hour
	checkpoints := newControlledExpiryCheckpointStore(server.agentApprovals.checkpoints)
	server.agentApprovals.checkpoints = checkpoints

	ctx, cancel := context.WithCancel(t.Context())
	server.StartAgentApprovalJanitor(ctx)
	server.StartAgentApprovalJanitor(ctx)
	waitForJanitorSignal(t, checkpoints.started, "janitor scan start")
	cancel()
	waitForJanitorSignal(t, checkpoints.canceled, "janitor scan cancellation")
	close(checkpoints.release)

	waitCtx, cancelWait := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelWait()
	if err := server.WaitAgentApprovalJanitor(waitCtx); err != nil {
		t.Fatalf("WaitAgentApprovalJanitor returned error: %v", err)
	}
	if calls := checkpoints.callCount(); calls != 1 {
		t.Fatalf("expiry scan calls = %d, want 1 after repeated Start", calls)
	}
}

func TestHostAPIAgentApprovalExpiryIgnoresUnrelatedCheckpoint(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-expiry-unrelated"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	createWaitingWorkflow(t, server, "wf-agent-tool-expiry-unrelated")
	checkpoints := server.runs.(runkit.CheckpointStore)
	if err := checkpoints.CreateCheckpoint(t.Context(), runkit.ApprovalCheckpoint{
		ID:             "unrelated-expired-checkpoint",
		RunID:          "unrelated-run",
		TenantID:       localApprovalTenant,
		DefinitionHash: hostAgentDefinitionHash,
		Ciphertext:     []byte("opaque"),
		ExpiresAt:      time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	reconciled, err := server.ReconcileExpiredAgentApprovals(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("ReconcileExpiredAgentApprovals: %v", err)
	}
	if reconciled != 0 {
		t.Fatalf("reconciled = %d, want 0", reconciled)
	}
	workflow, err := server.workflows.Get(t.Context(), "wf-agent-tool-expiry-unrelated")
	if err != nil {
		t.Fatalf("Get workflow: %v", err)
	}
	if workflow.Status != workflowkit.StatusWaitingApproval {
		t.Fatalf("unrelated workflow status = %q, want waiting_approval", workflow.Status)
	}
}

type controlledExpiryCheckpointStore struct {
	runkit.CheckpointStore

	mu           sync.Mutex
	calls        int
	started      chan struct{}
	canceled     chan struct{}
	release      chan struct{}
	cancelSignal sync.Once
}

func newControlledExpiryCheckpointStore(store runkit.CheckpointStore) *controlledExpiryCheckpointStore {
	return &controlledExpiryCheckpointStore{
		CheckpointStore: store,
		started:         make(chan struct{}, 2),
		canceled:        make(chan struct{}),
		release:         make(chan struct{}),
	}
}

func (s *controlledExpiryCheckpointStore) ExpireCheckpoints(ctx context.Context, _ time.Time) (int, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	s.started <- struct{}{}

	select {
	case <-ctx.Done():
		s.cancelSignal.Do(func() {
			close(s.canceled)
		})
		<-s.release
		return 0, ctx.Err()
	case <-s.release:
		return 0, nil
	}
}

func (s *controlledExpiryCheckpointStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func waitForJanitorSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
