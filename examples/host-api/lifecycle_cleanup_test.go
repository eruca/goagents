package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/eruca/goagents/workflowkit"
	workflowsqlite "github.com/eruca/goagents/workflowkit/sqlitestore"
)

func TestFinalizeWorkflowShutdownFailsActiveWorkflow(t *testing.T) {
	for _, status := range []workflowkit.Status{workflowkit.StatusPending, workflowkit.StatusRunning} {
		t.Run(string(status), func(t *testing.T) {
			server, store := newSQLiteLifecycleTestServer(t)
			saveLifecycleTestRun(t, store, workflowkit.WorkflowRun{
				ID:     "wf-active-" + string(status),
				Status: status,
			})

			if err := server.finalizeWorkflowShutdown(t.Context(), "wf-active-"+string(status)); err != nil {
				t.Fatalf("finalizeWorkflowShutdown() error = %v", err)
			}

			run := getLifecycleTestRun(t, store, "wf-active-"+string(status))
			if run.Status != workflowkit.StatusFailed {
				t.Fatalf("status = %q, want %q", run.Status, workflowkit.StatusFailed)
			}
			if run.Error != hostShutdownTimeoutCode {
				t.Fatalf("error = %q, want %q", run.Error, hostShutdownTimeoutCode)
			}
		})
	}
}

func TestFinalizeWorkflowShutdownClearsQueuedLease(t *testing.T) {
	server, store := newSQLiteLifecycleTestServer(t)
	saveLifecycleTestRun(t, store, workflowkit.WorkflowRun{
		ID:         "wf-queued",
		Status:     workflowkit.StatusPending,
		LeaseOwner: "worker-1",
		LeaseUntil: time.Now().Add(time.Minute),
	})

	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-queued"); err != nil {
		t.Fatalf("finalizeWorkflowShutdown() error = %v", err)
	}

	run := getLifecycleTestRun(t, store, "wf-queued")
	if run.LeaseOwner != "" {
		t.Fatalf("lease owner = %q, want empty", run.LeaseOwner)
	}
	if !run.LeaseUntil.IsZero() {
		t.Fatalf("lease until = %v, want zero", run.LeaseUntil)
	}
}

func TestFinalizeWorkflowShutdownPreservesTerminalWorkflow(t *testing.T) {
	for _, status := range []workflowkit.Status{
		workflowkit.StatusSucceeded,
		workflowkit.StatusFailed,
		workflowkit.StatusCancelled,
	} {
		t.Run(string(status), func(t *testing.T) {
			server, store := newSQLiteLifecycleTestServer(t)
			saveLifecycleTestRun(t, store, workflowkit.WorkflowRun{
				ID:         "wf-terminal-" + string(status),
				Status:     status,
				Error:      "existing terminal result",
				LeaseOwner: "existing-owner",
				LeaseUntil: time.Now().Add(time.Minute),
			})
			before := getLifecycleTestRun(t, store, "wf-terminal-"+string(status))

			if err := server.finalizeWorkflowShutdown(t.Context(), before.ID); err != nil {
				t.Fatalf("finalizeWorkflowShutdown() error = %v", err)
			}

			after := getLifecycleTestRun(t, store, before.ID)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("terminal workflow changed:\nbefore=%+v\nafter=%+v", before, after)
			}
		})
	}
}

func TestFinalizeWorkflowShutdownPreservesStableWaitingApproval(t *testing.T) {
	server, store := newSQLiteLifecycleTestServer(t)
	saveLifecycleTestRun(t, store, workflowkit.WorkflowRun{
		ID:            "wf-waiting",
		Status:        workflowkit.StatusWaitingApproval,
		ApprovalRef:   "approval:wf-waiting",
		WaitingReason: "operator approval required",
		OutputRef:     "artifact:waiting-output",
	})
	before := getLifecycleTestRun(t, store, "wf-waiting")

	if err := server.finalizeWorkflowShutdown(t.Context(), before.ID); err != nil {
		t.Fatalf("finalizeWorkflowShutdown() error = %v", err)
	}

	after := getLifecycleTestRun(t, store, before.ID)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("waiting workflow changed:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestFinalizeWorkflowShutdownPreservesHistoryAndReferences(t *testing.T) {
	server, store := newLifecycleTestServer()
	records := []workflowkit.StepRecord{{
		Name:       "agent_review",
		Status:     workflowkit.StatusSucceeded,
		Attempt:    1,
		OutputRef:  "artifact:step-output",
		AgentRunID: "agent-step-1",
		AuditRef:   "audit:step-1",
	}}
	saveLifecycleTestRun(t, store, workflowkit.WorkflowRun{
		ID:             "wf-history",
		Status:         workflowkit.StatusRunning,
		InputRef:       "artifact:workflow-input",
		OutputRef:      "artifact:workflow-output",
		AgentRunID:     "agent-workflow-1",
		AuditRef:       "audit:workflow-1",
		ApprovalRef:    "approval:wf-history",
		WaitingReason:  "existing waiting reason",
		CurrentStep:    "finalize",
		CompletedSteps: []string{"ingest", "agent_review"},
		StepAttempts:   map[string]int{"ingest": 1, "agent_review": 2},
		StepRecords:    records,
		Metadata:       map[string]any{"run_mode": "queued", "tenant": "demo"},
	})

	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-history"); err != nil {
		t.Fatalf("finalizeWorkflowShutdown() error = %v", err)
	}

	run := getLifecycleTestRun(t, store, "wf-history")
	if run.InputRef != "artifact:workflow-input" || run.OutputRef != "artifact:workflow-output" ||
		run.AgentRunID != "agent-workflow-1" || run.AuditRef != "audit:workflow-1" ||
		run.ApprovalRef != "approval:wf-history" || run.WaitingReason != "existing waiting reason" ||
		run.CurrentStep != "finalize" {
		t.Fatalf("workflow references changed: %+v", run)
	}
	if !reflect.DeepEqual(run.CompletedSteps, []string{"ingest", "agent_review"}) {
		t.Fatalf("completed steps = %v, want preserved history", run.CompletedSteps)
	}
	if !reflect.DeepEqual(run.StepRecords, records) {
		t.Fatalf("step records = %+v, want %+v", run.StepRecords, records)
	}
	if !reflect.DeepEqual(run.StepAttempts, map[string]int{"ingest": 1, "agent_review": 2}) {
		t.Fatalf("step attempts = %+v, want preserved attempts", run.StepAttempts)
	}
	if !reflect.DeepEqual(run.Metadata, map[string]any{"run_mode": "queued", "tenant": "demo"}) {
		t.Fatalf("metadata = %+v, want preserved metadata", run.Metadata)
	}
}

func TestFinalizeWorkflowShutdownIsIdempotent(t *testing.T) {
	server, store := newLifecycleTestServer()
	saveLifecycleTestRun(t, store, workflowkit.WorkflowRun{
		ID:         "wf-idempotent",
		Status:     workflowkit.StatusRunning,
		LeaseOwner: "worker-1",
		LeaseUntil: time.Now().Add(time.Minute),
	})

	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-idempotent"); err != nil {
		t.Fatalf("first finalizeWorkflowShutdown() error = %v", err)
	}
	afterFirst := getLifecycleTestRun(t, store, "wf-idempotent")
	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-idempotent"); err != nil {
		t.Fatalf("second finalizeWorkflowShutdown() error = %v", err)
	}
	afterSecond := getLifecycleTestRun(t, store, "wf-idempotent")

	if !reflect.DeepEqual(afterSecond, afterFirst) {
		t.Fatalf("second cleanup changed workflow:\nfirst=%+v\nsecond=%+v", afterFirst, afterSecond)
	}
}

func TestWaitAndCleanupExecutionsWaitsBeforeDoneAndPropagatesCleanupError(t *testing.T) {
	operationDone := make(chan struct{})
	cleanupStarted := make(chan struct{})
	cleanupContext := make(chan context.Context, 1)
	cleanupErr := errors.New("cleanup failed")
	ctx := newObservedDoneContext(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- waitAndCleanupExecutions(ctx, []executionSnapshot{{
			workflowID: "wf-wait",
			kind:       executionSyncWorkflow,
			done:       operationDone,
			cleanup: func(got context.Context) error {
				close(cleanupStarted)
				cleanupContext <- got
				return cleanupErr
			},
		}})
	}()
	select {
	case <-ctx.observed:
	case <-cleanupStarted:
		t.Fatal("cleanup ran before waitAndCleanupExecutions observed operation completion")
	}

	close(operationDone)
	if err := <-result; err != cleanupErr {
		t.Fatalf("waitAndCleanupExecutions() error = %v, want cleanup sentinel", err)
	}
	if got := <-cleanupContext; got != ctx {
		t.Fatalf("cleanup context = %p, want original %p", got, ctx)
	}
	<-cleanupStarted
}

func TestWaitAndCleanupExecutionsStopsCleanupWhenContextExpires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleanupCalled := false

	err := waitAndCleanupExecutions(ctx, []executionSnapshot{{
		workflowID: "wf-cancelled",
		kind:       executionQueuedWorkflow,
		done:       make(chan struct{}),
		cleanup: func(context.Context) error {
			cleanupCalled = true
			return nil
		},
	}})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitAndCleanupExecutions() error = %v, want context.Canceled", err)
	}
	if cleanupCalled {
		t.Fatal("cleanup ran after the context expired")
	}
}

func newLifecycleTestServer() (*Server, *workflowkit.MemoryStore) {
	store := workflowkit.NewMemoryStore()
	return &Server{workflows: store}, store
}

func newSQLiteLifecycleTestServer(t *testing.T) (*Server, workflowkit.Store) {
	t.Helper()
	store, err := workflowsqlite.Open(filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatalf("Open SQLite workflow store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close SQLite workflow store: %v", err)
		}
	})
	return &Server{workflows: store}, store
}

func saveLifecycleTestRun(t *testing.T, store workflowkit.Store, run workflowkit.WorkflowRun) {
	t.Helper()
	if err := store.Save(t.Context(), run); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

func getLifecycleTestRun(t *testing.T, store workflowkit.Store, id string) workflowkit.WorkflowRun {
	t.Helper()
	run, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%q) error = %v", id, err)
	}
	return run
}
