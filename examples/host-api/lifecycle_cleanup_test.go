package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
	workflowsqlite "github.com/eruca/goagents/workflowkit/sqlitestore"
)

func TestSideEffectSmokeControlsStayOutOfProductionHost(t *testing.T) {
	assertHostSideEffectTestIsolation(t)
}

func assertHostSideEffectTestIsolation(t *testing.T) {
	t.Helper()
	for _, path := range []string{"main.go", "server.go"} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read production host source %s: %v", path, err)
		}
		for _, forbidden := range []string{
			"GOAGENTS_TEST_SIDE_EFFECT",
			"sideEffectSink",
			"testSideEffectStep",
			"test_external_side_effect",
		} {
			if strings.Contains(string(source), forbidden) {
				t.Fatalf("production host source %s contains test-only control %q", path, forbidden)
			}
		}
	}

	cleanup, err := os.ReadFile("lifecycle_cleanup.go")
	if err != nil {
		t.Fatalf("read production lifecycle cleanup: %v", err)
	}
	lowerCleanup := strings.ToLower(string(cleanup))
	for _, forbidden := range []string{
		"createcheckpoint",
		"sideeffect",
		"sink",
		"toolcall",
		"requeue",
	} {
		if strings.Contains(lowerCleanup, forbidden) {
			t.Fatalf("production lifecycle cleanup contains forbidden %q behavior", forbidden)
		}
	}
}

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

func TestFinalizeWorkflowShutdownFailsRunningAgentRun(t *testing.T) {
	server, workflows, runs := newWorkflowAgentRunLifecycleFixture()
	saveLifecycleTestAgentRun(t, runs, runkit.RunRecord{
		RunID:      "agent-running",
		WorkflowID: "wf-agent-running",
		Status:     runkit.StatusRunning,
		Summary: runkit.TerminalSummary{
			ContentRef:   "artifact:partial-output",
			InputTokens:  11,
			OutputTokens: 7,
			LLMCalls:     2,
			ToolCalls:    1,
			UsedTools:    []string{"record_review"},
		},
		Metadata: map[string]any{"external_tool_call_id": "tool-call-stable"},
	})
	saveLifecycleTestRun(t, workflows, workflowkit.WorkflowRun{
		ID:         "wf-agent-running",
		Status:     workflowkit.StatusRunning,
		AgentRunID: "agent-running",
	})

	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-agent-running"); err != nil {
		t.Fatalf("finalizeWorkflowShutdown() error = %v", err)
	}

	workflow := getLifecycleTestRun(t, workflows, "wf-agent-running")
	if workflow.Status != workflowkit.StatusFailed || workflow.Error != hostShutdownTimeoutCode {
		t.Fatalf("workflow = status %q error %q, want shutdown failed", workflow.Status, workflow.Error)
	}
	agentRun := getLifecycleAgentRun(t, runs, "agent-running")
	if agentRun.Status != runkit.StatusFailed ||
		agentRun.Summary.Status != runkit.StatusFailed ||
		agentRun.Summary.AbortReason != hostShutdownTimeoutCode {
		t.Fatalf("agent run = status %q summary %q abort %q, want shutdown failed",
			agentRun.Status, agentRun.Summary.Status, agentRun.Summary.AbortReason)
	}
	if agentRun.Summary.ContentRef != "artifact:partial-output" ||
		agentRun.Summary.InputTokens != 11 ||
		agentRun.Summary.OutputTokens != 7 ||
		agentRun.Summary.LLMCalls != 2 ||
		agentRun.Summary.ToolCalls != 1 ||
		!reflect.DeepEqual(agentRun.Summary.UsedTools, []string{"record_review"}) ||
		!reflect.DeepEqual(agentRun.Metadata, map[string]any{"external_tool_call_id": "tool-call-stable"}) {
		t.Fatalf("agent run summary or metadata was not preserved: %+v", agentRun)
	}
}

func TestFinalizeWorkflowShutdownPreservesTerminalAgentRun(t *testing.T) {
	for _, status := range []runkit.Status{runkit.StatusSucceeded, runkit.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			server, workflows, runs := newWorkflowAgentRunLifecycleFixture()
			saveLifecycleTestAgentRun(t, runs, runkit.RunRecord{
				RunID:      "agent-terminal-" + string(status),
				WorkflowID: "wf-terminal-agent-" + string(status),
				Status:     status,
				Summary: runkit.TerminalSummary{
					Status:      status,
					ContentRef:  "artifact:terminal-output",
					AbortReason: "existing-terminal-reason",
					ToolCalls:   3,
				},
				Metadata: map[string]any{"marker": "terminal"},
			})
			before := getLifecycleAgentRun(t, runs, "agent-terminal-"+string(status))
			saveLifecycleTestRun(t, workflows, workflowkit.WorkflowRun{
				ID:         "wf-terminal-agent-" + string(status),
				Status:     workflowkit.StatusRunning,
				AgentRunID: before.RunID,
			})

			if err := server.finalizeWorkflowShutdown(t.Context(), "wf-terminal-agent-"+string(status)); err != nil {
				t.Fatalf("finalizeWorkflowShutdown() error = %v", err)
			}

			workflow := getLifecycleTestRun(t, workflows, "wf-terminal-agent-"+string(status))
			if workflow.Status != workflowkit.StatusFailed || workflow.Error != hostShutdownTimeoutCode {
				t.Fatalf("workflow = status %q error %q, want shutdown failed", workflow.Status, workflow.Error)
			}
			after := getLifecycleAgentRun(t, runs, before.RunID)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("terminal AgentRun changed:\nbefore=%+v\nafter=%+v", before, after)
			}
		})
	}
}

func TestFinalizeWorkflowShutdownDoesNotRequireAgentRunID(t *testing.T) {
	server, workflows, _ := newWorkflowAgentRunLifecycleFixture()
	saveLifecycleTestRun(t, workflows, workflowkit.WorkflowRun{
		ID:     "wf-without-agent-run",
		Status: workflowkit.StatusRunning,
	})

	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-without-agent-run"); err != nil {
		t.Fatalf("finalizeWorkflowShutdown() error = %v", err)
	}

	workflow := getLifecycleTestRun(t, workflows, "wf-without-agent-run")
	if workflow.Status != workflowkit.StatusFailed || workflow.Error != hostShutdownTimeoutCode {
		t.Fatalf("workflow = status %q error %q, want shutdown failed", workflow.Status, workflow.Error)
	}
}

func TestFinalizeWorkflowShutdownConvergesAfterAgentRunWriteFailure(t *testing.T) {
	server, workflows, runs := newWorkflowAgentRunLifecycleFixture()
	saveLifecycleTestAgentRun(t, runs, runkit.RunRecord{
		RunID:      "agent-fail-once",
		WorkflowID: "wf-agent-fail-once",
		Status:     runkit.StatusRunning,
	})
	saveLifecycleTestRun(t, workflows, workflowkit.WorkflowRun{
		ID:         "wf-agent-fail-once",
		Status:     workflowkit.StatusRunning,
		AgentRunID: "agent-fail-once",
	})
	writeErr := errors.New("agent run write failed once")
	server.runs = &failOnceLifecycleRunStore{Store: runs, err: writeErr}

	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-agent-fail-once"); !errors.Is(err, writeErr) {
		t.Fatalf("first finalizeWorkflowShutdown() error = %v, want AgentRun write error", err)
	}
	partialWorkflow := getLifecycleTestRun(t, workflows, "wf-agent-fail-once")
	partialAgentRun := getLifecycleAgentRun(t, runs, "agent-fail-once")
	if partialWorkflow.Status != workflowkit.StatusFailed ||
		partialWorkflow.Error != hostShutdownTimeoutCode ||
		partialAgentRun.Status != runkit.StatusRunning {
		t.Fatalf("partial state = workflow %q/%q AgentRun %q, want shutdown failed/running",
			partialWorkflow.Status, partialWorkflow.Error, partialAgentRun.Status)
	}

	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-agent-fail-once"); err != nil {
		t.Fatalf("retry finalizeWorkflowShutdown() error = %v", err)
	}
	converged := getLifecycleAgentRun(t, runs, "agent-fail-once")
	if converged.Status != runkit.StatusFailed ||
		converged.Summary.Status != runkit.StatusFailed ||
		converged.Summary.AbortReason != hostShutdownTimeoutCode {
		t.Fatalf("converged AgentRun = status %q summary %q abort %q, want shutdown failed",
			converged.Status, converged.Summary.Status, converged.Summary.AbortReason)
	}
}

func TestFinalizeWorkflowShutdownConvergesAfterWorkflowWriteFailure(t *testing.T) {
	server, workflows, runs := newWorkflowAgentRunLifecycleFixture()
	saveLifecycleTestAgentRun(t, runs, runkit.RunRecord{
		RunID:      "agent-workflow-fail-once",
		WorkflowID: "wf-write-fail-once",
		Status:     runkit.StatusRunning,
	})
	saveLifecycleTestRun(t, workflows, workflowkit.WorkflowRun{
		ID:         "wf-write-fail-once",
		Status:     workflowkit.StatusRunning,
		AgentRunID: "agent-workflow-fail-once",
	})
	writeErr := errors.New("workflow write failed once")
	server.workflows = &failOnceLifecycleWorkflowStore{Store: workflows, err: writeErr}

	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-write-fail-once"); !errors.Is(err, writeErr) {
		t.Fatalf("first finalizeWorkflowShutdown() error = %v, want workflow write error", err)
	}
	partialWorkflow := getLifecycleTestRun(t, workflows, "wf-write-fail-once")
	partialAgentRun := getLifecycleAgentRun(t, runs, "agent-workflow-fail-once")
	if partialWorkflow.Status != workflowkit.StatusRunning ||
		partialAgentRun.Status != runkit.StatusFailed ||
		partialAgentRun.Summary.AbortReason != hostShutdownTimeoutCode {
		t.Fatalf("partial state = workflow %q AgentRun %q/%q, want running/shutdown failed",
			partialWorkflow.Status, partialAgentRun.Status, partialAgentRun.Summary.AbortReason)
	}

	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-write-fail-once"); err != nil {
		t.Fatalf("retry finalizeWorkflowShutdown() error = %v", err)
	}
	converged := getLifecycleTestRun(t, workflows, "wf-write-fail-once")
	if converged.Status != workflowkit.StatusFailed || converged.Error != hostShutdownTimeoutCode {
		t.Fatalf("converged workflow = status %q error %q, want shutdown failed",
			converged.Status, converged.Error)
	}
}

func TestFinalizeWorkflowShutdownPreservesRunningAgentRunForStableWorkflow(t *testing.T) {
	tests := []struct {
		name string
		run  workflowkit.WorkflowRun
	}{
		{name: "waiting approval", run: workflowkit.WorkflowRun{
			Status:        workflowkit.StatusWaitingApproval,
			ApprovalRef:   "approval:stable",
			WaitingReason: "operator approval required",
		}},
		{name: "succeeded", run: workflowkit.WorkflowRun{Status: workflowkit.StatusSucceeded}},
		{name: "cancelled", run: workflowkit.WorkflowRun{Status: workflowkit.StatusCancelled}},
		{name: "other failure", run: workflowkit.WorkflowRun{
			Status: workflowkit.StatusFailed,
			Error:  "provider_failed",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, workflows, runs := newWorkflowAgentRunLifecycleFixture()
			workflowID := "wf-stable-" + strings.ReplaceAll(test.name, " ", "-")
			agentRunID := "agent-stable-" + strings.ReplaceAll(test.name, " ", "-")
			saveLifecycleTestAgentRun(t, runs, runkit.RunRecord{
				RunID:      agentRunID,
				WorkflowID: workflowID,
				Status:     runkit.StatusRunning,
			})
			test.run.ID = workflowID
			test.run.AgentRunID = agentRunID
			saveLifecycleTestRun(t, workflows, test.run)
			beforeWorkflow := getLifecycleTestRun(t, workflows, workflowID)
			beforeAgentRun := getLifecycleAgentRun(t, runs, agentRunID)

			if err := server.finalizeWorkflowShutdown(t.Context(), workflowID); err != nil {
				t.Fatalf("finalizeWorkflowShutdown() error = %v", err)
			}

			afterWorkflow := getLifecycleTestRun(t, workflows, workflowID)
			afterAgentRun := getLifecycleAgentRun(t, runs, agentRunID)
			if !reflect.DeepEqual(afterWorkflow, beforeWorkflow) ||
				!reflect.DeepEqual(afterAgentRun, beforeAgentRun) {
				t.Fatalf("stable state changed:\nworkflow before=%+v after=%+v\nAgentRun before=%+v after=%+v",
					beforeWorkflow, afterWorkflow, beforeAgentRun, afterAgentRun)
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
			if !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("terminal workflow UpdatedAt changed: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
			}
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
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("waiting workflow UpdatedAt changed: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("waiting workflow changed:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestFinalizeWorkflowShutdownRollsBackWhenWorkflowStabilizesBeforeUpdate(t *testing.T) {
	for _, status := range []workflowkit.Status{
		workflowkit.StatusWaitingApproval,
		workflowkit.StatusSucceeded,
		workflowkit.StatusFailed,
		workflowkit.StatusCancelled,
	} {
		t.Run(string(status), func(t *testing.T) {
			updatedAt := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
			agentRunID := "agent-stabilized-" + string(status)
			store := &workflowShutdownRaceStore{
				outer: workflowkit.WorkflowRun{
					ID:         "wf-stabilized",
					Status:     workflowkit.StatusRunning,
					AgentRunID: agentRunID,
					UpdatedAt:  updatedAt.Add(-time.Minute),
				},
				current: workflowkit.WorkflowRun{
					ID:         "wf-stabilized",
					Status:     status,
					OutputRef:  "artifact:stable-output",
					AgentRunID: agentRunID,
					UpdatedAt:  updatedAt,
				},
			}
			runs := runkit.NewMemoryStore()
			saveLifecycleTestAgentRun(t, runs, runkit.RunRecord{
				RunID:  agentRunID,
				Status: runkit.StatusRunning,
			})
			beforeAgentRun := getLifecycleAgentRun(t, runs, agentRunID)
			server := &Server{workflows: store, runs: runs}

			if err := server.finalizeWorkflowShutdown(t.Context(), "wf-stabilized"); err != nil {
				t.Fatalf("finalizeWorkflowShutdown() error = %v", err)
			}

			if store.wrote {
				t.Fatalf("Update wrote stable %q workflow instead of rolling back", status)
			}
			if !store.current.UpdatedAt.Equal(updatedAt) {
				t.Fatalf("stable %q UpdatedAt changed: got %v want %v", status, store.current.UpdatedAt, updatedAt)
			}
			if store.updateCalls != 1 {
				t.Fatalf("Update calls = %d, want 1", store.updateCalls)
			}
			afterAgentRun := getLifecycleAgentRun(t, runs, agentRunID)
			if !reflect.DeepEqual(afterAgentRun, beforeAgentRun) {
				t.Fatalf("stable %q AgentRun changed:\nbefore=%+v\nafter=%+v",
					status, beforeAgentRun, afterAgentRun)
			}
		})
	}
}

func TestFinalizeWorkflowShutdownPreservesHistoryAndReferences(t *testing.T) {
	server, store := newSQLiteLifecycleTestServer(t)
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
	server, workflows, runs := newWorkflowAgentRunLifecycleFixture()
	saveLifecycleTestAgentRun(t, runs, runkit.RunRecord{
		RunID:      "agent-idempotent",
		WorkflowID: "wf-idempotent",
		Status:     runkit.StatusRunning,
	})
	saveLifecycleTestRun(t, workflows, workflowkit.WorkflowRun{
		ID:         "wf-idempotent",
		Status:     workflowkit.StatusRunning,
		AgentRunID: "agent-idempotent",
		LeaseOwner: "worker-1",
		LeaseUntil: time.Now().Add(time.Minute),
	})

	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-idempotent"); err != nil {
		t.Fatalf("first finalizeWorkflowShutdown() error = %v", err)
	}
	afterFirstWorkflow := getLifecycleTestRun(t, workflows, "wf-idempotent")
	afterFirstAgentRun := getLifecycleAgentRun(t, runs, "agent-idempotent")
	if err := server.finalizeWorkflowShutdown(t.Context(), "wf-idempotent"); err != nil {
		t.Fatalf("second finalizeWorkflowShutdown() error = %v", err)
	}
	afterSecondWorkflow := getLifecycleTestRun(t, workflows, "wf-idempotent")
	afterSecondAgentRun := getLifecycleAgentRun(t, runs, "agent-idempotent")

	if !reflect.DeepEqual(afterSecondWorkflow, afterFirstWorkflow) ||
		!reflect.DeepEqual(afterSecondAgentRun, afterFirstAgentRun) {
		t.Fatalf(
			"second cleanup changed state:\nworkflow first=%+v second=%+v\nAgentRun first=%+v second=%+v",
			afterFirstWorkflow,
			afterSecondWorkflow,
			afterFirstAgentRun,
			afterSecondAgentRun,
		)
	}
}

func TestFinalizeAgentApprovalShutdownBeforeLeaseIsNoOp(t *testing.T) {
	server, created, checkpoints := newAgentApprovalLifecycleFixture(t, "wf-agent-cleanup-pending")
	beforeCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	beforeRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	beforeWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)

	if err := server.finalizeAgentApprovalShutdown(t.Context(), created.ID, *created.AgentApproval, "host-api:not-acquired"); err != nil {
		t.Fatalf("finalizeAgentApprovalShutdown() error = %v", err)
	}

	afterCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	afterRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	afterWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)
	if !reflect.DeepEqual(afterCheckpoint, beforeCheckpoint) || !reflect.DeepEqual(afterRun, beforeRun) || !reflect.DeepEqual(afterWorkflow, beforeWorkflow) {
		t.Fatalf("cleanup before lease changed state:\ncheckpoint before=%+v after=%+v\nrun before=%+v after=%+v\nworkflow before=%+v after=%+v",
			beforeCheckpoint, afterCheckpoint, beforeRun, afterRun, beforeWorkflow, afterWorkflow)
	}
}

func TestFinalizeAgentApprovalShutdownFailsOwnedLeaseAndRunningAgentRun(t *testing.T) {
	server, created, checkpoints := newAgentApprovalLifecycleFixture(t, "wf-agent-cleanup-owned")
	const leaseOwner = "host-api:owned-cleanup"
	leaseLifecycleCheckpoint(t, checkpoints, *created.AgentApproval, leaseOwner)

	if err := server.finalizeAgentApprovalShutdown(t.Context(), created.ID, *created.AgentApproval, leaseOwner); err != nil {
		t.Fatalf("finalizeAgentApprovalShutdown() error = %v", err)
	}

	checkpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	if checkpoint.Status != runkit.CheckpointFailed || checkpoint.FailureCode != hostShutdownTimeoutCode ||
		checkpoint.LeaseOwner != "" || !checkpoint.LeaseUntil.IsZero() {
		t.Fatalf("checkpoint after cleanup = %+v, want failed shutdown checkpoint without lease", checkpoint)
	}
	run := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	if run.Status != runkit.StatusFailed || run.Summary.Status != runkit.StatusFailed || run.Summary.AbortReason != hostShutdownTimeoutCode {
		t.Fatalf("agent run after cleanup = %+v, want failed shutdown summary", run)
	}
	workflow := getLifecycleTestRun(t, server.workflows, created.ID)
	if workflow.Status != workflowkit.StatusFailed || workflow.Error != hostShutdownTimeoutCode ||
		workflow.ApprovalRef != "" || workflow.WaitingReason != "" || agentApprovalFromMetadata(workflow.Metadata) != nil {
		t.Fatalf("workflow after cleanup = %+v, want failed without pending approval", workflow)
	}
}

func TestFinalizeAgentApprovalShutdownLeavesCompetingLeaseUntouched(t *testing.T) {
	server, created, checkpoints := newAgentApprovalLifecycleFixture(t, "wf-agent-cleanup-competing")
	leaseLifecycleCheckpoint(t, checkpoints, *created.AgentApproval, "host-api:competitor")
	beforeCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	beforeRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	beforeWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)

	if err := server.finalizeAgentApprovalShutdown(t.Context(), created.ID, *created.AgentApproval, "host-api:loser"); err != nil {
		t.Fatalf("finalizeAgentApprovalShutdown() error = %v", err)
	}

	afterCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	afterRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	afterWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)
	if !reflect.DeepEqual(afterCheckpoint, beforeCheckpoint) || !reflect.DeepEqual(afterRun, beforeRun) || !reflect.DeepEqual(afterWorkflow, beforeWorkflow) {
		t.Fatalf("competing cleanup changed state:\ncheckpoint before=%+v after=%+v\nrun before=%+v after=%+v\nworkflow before=%+v after=%+v",
			beforeCheckpoint, afterCheckpoint, beforeRun, afterRun, beforeWorkflow, afterWorkflow)
	}
}

func TestFinalizeAgentApprovalShutdownKeepsConsumedCheckpointConsumed(t *testing.T) {
	server, created, checkpoints := newAgentApprovalLifecycleFixture(t, "wf-agent-cleanup-consumed")
	const leaseOwner = "host-api:consumed-cleanup"
	leaseLifecycleCheckpoint(t, checkpoints, *created.AgentApproval, leaseOwner)
	if err := checkpoints.CompleteLease(t.Context(), runkit.CheckpointLeaseCompletion{
		CheckpointID: created.AgentApproval.CheckpointID,
		TenantID:     localApprovalTenant,
		LeaseOwner:   leaseOwner,
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CompleteLease: %v", err)
	}
	beforeCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)

	if err := server.finalizeAgentApprovalShutdown(t.Context(), created.ID, *created.AgentApproval, leaseOwner); err != nil {
		t.Fatalf("finalizeAgentApprovalShutdown() error = %v", err)
	}

	afterCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	if !reflect.DeepEqual(afterCheckpoint, beforeCheckpoint) || afterCheckpoint.Status != runkit.CheckpointConsumed {
		t.Fatalf("consumed checkpoint changed:\nbefore=%+v\nafter=%+v", beforeCheckpoint, afterCheckpoint)
	}
	run := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	if run.Status != runkit.StatusFailed || run.Summary.AbortReason != hostShutdownTimeoutCode {
		t.Fatalf("incomplete consumed agent run = %+v, want failed shutdown summary", run)
	}
	workflow := getLifecycleTestRun(t, server.workflows, created.ID)
	if workflow.Status != workflowkit.StatusFailed || workflow.Error != hostShutdownTimeoutCode || agentApprovalFromMetadata(workflow.Metadata) != nil {
		t.Fatalf("incomplete consumed workflow = %+v, want failed without pending approval", workflow)
	}
}

func TestFinalizeAgentApprovalShutdownPreservesCompletedResume(t *testing.T) {
	server, created, checkpoints := newAgentApprovalLifecycleFixture(t, "wf-agent-cleanup-complete")
	pending := created.AgentApproval.Tools[0]
	response := agentApprovalRequestForTest(t, server.Handler(), created.ID, map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": pending.ToolCallID,
			"tool":         pending.Tool,
			"allowed":      true,
		}},
	}, "Bearer test-operator")
	if response.Code != 200 {
		t.Fatalf("agent approval status = %d; body=%s", response.Code, response.Body.String())
	}
	beforeCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	beforeRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	beforeWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)
	if beforeCheckpoint.Status != runkit.CheckpointConsumed || beforeRun.Status != runkit.StatusSucceeded ||
		beforeWorkflow.Status != workflowkit.StatusWaitingApproval || agentApprovalFromMetadata(beforeWorkflow.Metadata) != nil {
		t.Fatalf("completed resume fixture is not stable: checkpoint=%+v run=%+v workflow=%+v", beforeCheckpoint, beforeRun, beforeWorkflow)
	}

	if err := server.finalizeAgentApprovalShutdown(t.Context(), created.ID, *created.AgentApproval, "host-api:completed-cleanup"); err != nil {
		t.Fatalf("finalizeAgentApprovalShutdown() error = %v", err)
	}

	afterCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	afterRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	afterWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)
	if !reflect.DeepEqual(afterCheckpoint, beforeCheckpoint) || !reflect.DeepEqual(afterRun, beforeRun) || !reflect.DeepEqual(afterWorkflow, beforeWorkflow) {
		t.Fatalf("completed resume changed:\ncheckpoint before=%+v after=%+v\nrun before=%+v after=%+v\nworkflow before=%+v after=%+v",
			beforeCheckpoint, afterCheckpoint, beforeRun, afterRun, beforeWorkflow, afterWorkflow)
	}
}

func TestFinalizeAgentApprovalShutdownPreservesCompletedPauseReplacement(t *testing.T) {
	server, created, checkpoints := newAgentApprovalLifecycleFixture(t, "wf-agent-cleanup-next-pause")
	const leaseOwner = "host-api:next-pause-cleanup"
	leaseLifecycleCheckpoint(t, checkpoints, *created.AgentApproval, leaseOwner)
	if err := checkpoints.CompleteLease(t.Context(), runkit.CheckpointLeaseCompletion{
		CheckpointID: created.AgentApproval.CheckpointID,
		TenantID:     localApprovalTenant,
		LeaseOwner:   leaseOwner,
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CompleteLease: %v", err)
	}
	nextApproval := agentApprovalResponse{
		CheckpointID: "checkpoint-next-pause",
		Tools:        append([]agentApprovalPendingTool(nil), created.AgentApproval.Tools...),
	}
	if err := checkpoints.CreateCheckpoint(t.Context(), runkit.ApprovalCheckpoint{
		ID:             nextApproval.CheckpointID,
		RunID:          created.AgentRunID,
		TenantID:       localApprovalTenant,
		DefinitionHash: hostAgentDefinitionHash,
		Ciphertext:     []byte("opaque-next-pause"),
		ExpiresAt:      time.Now().UTC().Add(agentApprovalLifetime),
	}); err != nil {
		t.Fatalf("CreateCheckpoint(next): %v", err)
	}
	current := getLifecycleTestRun(t, server.workflows, created.ID)
	if _, err := server.replacePendingAgentApproval(t.Context(), current, created.AgentApproval.CheckpointID, nextApproval); err != nil {
		t.Fatalf("replacePendingAgentApproval: %v", err)
	}
	beforeOldCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	beforeNextCheckpoint := getLifecycleCheckpoint(t, checkpoints, nextApproval.CheckpointID)
	beforeRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	beforeWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)

	if err := server.finalizeAgentApprovalShutdown(t.Context(), created.ID, *created.AgentApproval, leaseOwner); err != nil {
		t.Fatalf("finalizeAgentApprovalShutdown() error = %v", err)
	}

	afterOldCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	afterNextCheckpoint := getLifecycleCheckpoint(t, checkpoints, nextApproval.CheckpointID)
	afterRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	afterWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)
	if !reflect.DeepEqual(afterOldCheckpoint, beforeOldCheckpoint) ||
		!reflect.DeepEqual(afterNextCheckpoint, beforeNextCheckpoint) ||
		!reflect.DeepEqual(afterRun, beforeRun) ||
		!reflect.DeepEqual(afterWorkflow, beforeWorkflow) {
		t.Fatalf("stable replacement pause changed:\nold checkpoint before=%+v after=%+v\nnext checkpoint before=%+v after=%+v\nrun before=%+v after=%+v\nworkflow before=%+v after=%+v",
			beforeOldCheckpoint, afterOldCheckpoint, beforeNextCheckpoint, afterNextCheckpoint, beforeRun, afterRun, beforeWorkflow, afterWorkflow)
	}
}

func TestFinalizeAgentApprovalShutdownPreservesLeasedPauseReplacement(t *testing.T) {
	server, created, checkpoints := newAgentApprovalLifecycleFixture(t, "wf-agent-cleanup-leased-next-pause")
	const leaseOwner = "host-api:leased-next-pause-cleanup"
	leaseLifecycleCheckpoint(t, checkpoints, *created.AgentApproval, leaseOwner)
	nextApproval := agentApprovalResponse{
		CheckpointID: "checkpoint-leased-next-pause",
		Tools:        append([]agentApprovalPendingTool(nil), created.AgentApproval.Tools...),
	}
	if err := checkpoints.CreateCheckpoint(t.Context(), runkit.ApprovalCheckpoint{
		ID:             nextApproval.CheckpointID,
		RunID:          created.AgentRunID,
		TenantID:       localApprovalTenant,
		DefinitionHash: hostAgentDefinitionHash,
		Ciphertext:     []byte("opaque-leased-next-pause"),
		ExpiresAt:      time.Now().UTC().Add(agentApprovalLifetime),
	}); err != nil {
		t.Fatalf("CreateCheckpoint(next): %v", err)
	}
	current := getLifecycleTestRun(t, server.workflows, created.ID)
	if _, err := server.replacePendingAgentApproval(t.Context(), current, created.AgentApproval.CheckpointID, nextApproval); err != nil {
		t.Fatalf("replacePendingAgentApproval: %v", err)
	}
	beforeOldCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	beforeNextCheckpoint := getLifecycleCheckpoint(t, checkpoints, nextApproval.CheckpointID)
	beforeRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	beforeWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)

	if err := server.finalizeAgentApprovalShutdown(t.Context(), created.ID, *created.AgentApproval, leaseOwner); err != nil {
		t.Fatalf("finalizeAgentApprovalShutdown() error = %v", err)
	}

	afterOldCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	afterNextCheckpoint := getLifecycleCheckpoint(t, checkpoints, nextApproval.CheckpointID)
	afterRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	afterWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)
	if !reflect.DeepEqual(afterOldCheckpoint, beforeOldCheckpoint) ||
		!reflect.DeepEqual(afterNextCheckpoint, beforeNextCheckpoint) ||
		!reflect.DeepEqual(afterRun, beforeRun) ||
		!reflect.DeepEqual(afterWorkflow, beforeWorkflow) {
		t.Fatalf("leased replacement pause changed:\nold checkpoint before=%+v after=%+v\nnext checkpoint before=%+v after=%+v\nrun before=%+v after=%+v\nworkflow before=%+v after=%+v",
			beforeOldCheckpoint, afterOldCheckpoint, beforeNextCheckpoint, afterNextCheckpoint, beforeRun, afterRun, beforeWorkflow, afterWorkflow)
	}
}

func TestFinalizeAgentApprovalShutdownProbeErrorLeavesCheckpointAndRunUntouched(t *testing.T) {
	server, created, checkpoints := newAgentApprovalLifecycleFixture(t, "wf-agent-cleanup-probe-error")
	const leaseOwner = "host-api:probe-error-cleanup"
	leaseLifecycleCheckpoint(t, checkpoints, *created.AgentApproval, leaseOwner)
	beforeCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	beforeRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	beforeWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)
	persistedWorkflows := server.workflows
	probeErr := errors.New("workflow probe failed")
	server.workflows = &workflowUpdateErrorStore{
		Store: persistedWorkflows,
		err:   probeErr,
	}

	err := server.finalizeAgentApprovalShutdown(t.Context(), created.ID, *created.AgentApproval, leaseOwner)
	if !errors.Is(err, probeErr) {
		t.Fatalf("finalizeAgentApprovalShutdown() error = %v, want workflow probe error", err)
	}

	afterCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	afterRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	afterWorkflow := getLifecycleTestRun(t, persistedWorkflows, created.ID)
	if !reflect.DeepEqual(afterCheckpoint, beforeCheckpoint) ||
		!reflect.DeepEqual(afterRun, beforeRun) ||
		!reflect.DeepEqual(afterWorkflow, beforeWorkflow) {
		t.Fatalf("probe error changed persisted state:\ncheckpoint before=%+v after=%+v\nrun before=%+v after=%+v\nworkflow before=%+v after=%+v",
			beforeCheckpoint, afterCheckpoint, beforeRun, afterRun, beforeWorkflow, afterWorkflow)
	}
}

func TestFinalizeAgentApprovalShutdownIsIdempotent(t *testing.T) {
	server, created, checkpoints := newAgentApprovalLifecycleFixture(t, "wf-agent-cleanup-idempotent")
	const leaseOwner = "host-api:idempotent-cleanup"
	leaseLifecycleCheckpoint(t, checkpoints, *created.AgentApproval, leaseOwner)

	if err := server.finalizeAgentApprovalShutdown(t.Context(), created.ID, *created.AgentApproval, leaseOwner); err != nil {
		t.Fatalf("first finalizeAgentApprovalShutdown() error = %v", err)
	}
	afterFirstCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	afterFirstRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	afterFirstWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)

	if err := server.finalizeAgentApprovalShutdown(t.Context(), created.ID, *created.AgentApproval, leaseOwner); err != nil {
		t.Fatalf("second finalizeAgentApprovalShutdown() error = %v", err)
	}
	afterSecondCheckpoint := getLifecycleCheckpoint(t, checkpoints, created.AgentApproval.CheckpointID)
	afterSecondRun := getLifecycleAgentRun(t, server.runs, created.AgentRunID)
	afterSecondWorkflow := getLifecycleTestRun(t, server.workflows, created.ID)
	if !reflect.DeepEqual(afterSecondCheckpoint, afterFirstCheckpoint) ||
		!reflect.DeepEqual(afterSecondRun, afterFirstRun) ||
		!reflect.DeepEqual(afterSecondWorkflow, afterFirstWorkflow) {
		t.Fatalf("second cleanup changed state:\ncheckpoint first=%+v second=%+v\nrun first=%+v second=%+v\nworkflow first=%+v second=%+v",
			afterFirstCheckpoint, afterSecondCheckpoint, afterFirstRun, afterSecondRun, afterFirstWorkflow, afterSecondWorkflow)
	}
}

func TestWaitAndCleanupExecutionsWaitsBeforeDoneAndReconcilesCleanupError(t *testing.T) {
	operationDone := make(chan struct{})
	cleanupStarted := make(chan struct{})
	cleanupContext := make(chan context.Context, 2)
	cleanupErr := errors.New("cleanup failed")
	var cleanupCalls int
	ctx := newObservedDoneContext(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- waitAndCleanupExecutions(ctx, []executionSnapshot{{
			workflowID: "wf-wait",
			kind:       executionSyncWorkflow,
			done:       operationDone,
			cleanup: func(got context.Context) error {
				cleanupCalls++
				if cleanupCalls == 1 {
					close(cleanupStarted)
				}
				cleanupContext <- got
				if cleanupCalls == 1 {
					return cleanupErr
				}
				return nil
			},
		}})
	}()
	select {
	case <-ctx.observed:
	case <-cleanupStarted:
		t.Fatal("cleanup ran before waitAndCleanupExecutions observed operation completion")
	}

	close(operationDone)
	if err := <-result; err != nil {
		t.Fatalf("waitAndCleanupExecutions() error = %v, want fail-once cleanup to converge", err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if got := <-cleanupContext; got != ctx {
			t.Fatalf("cleanup context on attempt %d = %p, want original %p", attempt, got, ctx)
		}
	}
	if cleanupCalls != 2 {
		t.Fatalf("cleanup calls = %d, want 2 after one failure", cleanupCalls)
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

func TestWaitAndCleanupExecutionsFairlyReconcilesCleanupErrors(t *testing.T) {
	registry := newExecutionRegistry()
	firstErr := errors.New("first cleanup failed")
	secondErr := errors.New("second cleanup failed")
	var firstCalls int
	var secondCalls int
	first, accepted := registry.Begin("wf-first", executionSyncWorkflow, func(context.Context) error {
		firstCalls++
		if firstCalls == 1 {
			return firstErr
		}
		return nil
	})
	if !accepted {
		t.Fatal("first execution was rejected")
	}
	second, accepted := registry.Begin("wf-second", executionQueuedWorkflow, func(context.Context) error {
		secondCalls++
		if secondCalls == 1 {
			return secondErr
		}
		return nil
	})
	if !accepted {
		t.Fatal("second execution was rejected")
	}
	snapshots := registry.Snapshot()
	first.Done()
	second.Done()

	err := waitAndCleanupExecutions(t.Context(), snapshots)
	if err != nil {
		t.Fatalf("waitAndCleanupExecutions() error = %v, want both fail-once cleanups to converge", err)
	}
	if firstCalls != 2 {
		t.Fatalf("first snapshot cleanup calls = %d, want 2", firstCalls)
	}
	if secondCalls != 2 {
		t.Fatalf("second snapshot cleanup calls = %d, want 2", secondCalls)
	}
}

func TestWaitAndCleanupExecutionsStopsAfterCleanupExpiresContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cleanupErr := errors.New("cleanup failed before context expiry")
	secondCalled := false

	err := waitAndCleanupExecutions(ctx, []executionSnapshot{
		{
			workflowID: "wf-expire",
			kind:       executionSyncWorkflow,
			done:       closedLifecycleChannel(),
			cleanup: func(context.Context) error {
				cancel()
				return cleanupErr
			},
		},
		{
			workflowID: "wf-must-not-run",
			kind:       executionQueuedWorkflow,
			done:       closedLifecycleChannel(),
			cleanup: func(context.Context) error {
				secondCalled = true
				return nil
			},
		},
	})

	if !errors.Is(err, cleanupErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("waitAndCleanupExecutions() error = %v, want cleanup and context errors", err)
	}
	if secondCalled {
		t.Fatal("cleanup continued after the context expired")
	}
}

func closedLifecycleChannel() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
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

func newAgentApprovalLifecycleFixture(t *testing.T, workflowID string) (*Server, workflowResponse, runkit.CheckpointStore) {
	t.Helper()
	server, err := NewServer(Config{
		RuntimeHome:           t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-cleanup"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, workflowID)
	return server, created, server.runs.(runkit.CheckpointStore)
}

func leaseLifecycleCheckpoint(t *testing.T, checkpoints runkit.CheckpointStore, approval agentApprovalResponse, leaseOwner string) {
	t.Helper()
	if _, err := checkpoints.ApproveAndLease(t.Context(), runkit.ApprovalLeaseRequest{
		CheckpointID:   approval.CheckpointID,
		TenantID:       localApprovalTenant,
		DefinitionHash: hostAgentDefinitionHash,
		ApproverID:     "operator-cleanup",
		AuditRef:       "audit:lifecycle-cleanup:" + approval.CheckpointID,
		ReasonCode:     "operator_approved",
		LeaseOwner:     leaseOwner,
		LeaseDuration:  agentApprovalLifetime,
		Now:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("ApproveAndLease: %v", err)
	}
}

func getLifecycleCheckpoint(t *testing.T, checkpoints runkit.CheckpointStore, checkpointID string) runkit.ApprovalCheckpoint {
	t.Helper()
	checkpoint, err := checkpoints.GetCheckpoint(t.Context(), checkpointID, localApprovalTenant)
	if err != nil {
		t.Fatalf("GetCheckpoint(%q): %v", checkpointID, err)
	}
	return checkpoint
}

func getLifecycleAgentRun(t *testing.T, store runkit.Store, runID string) runkit.RunRecord {
	t.Helper()
	run, err := store.Get(t.Context(), runID)
	if err != nil {
		t.Fatalf("Get agent run %q: %v", runID, err)
	}
	return run
}

func newWorkflowAgentRunLifecycleFixture() (*Server, *workflowkit.MemoryStore, *runkit.MemoryStore) {
	workflows := workflowkit.NewMemoryStore()
	runs := runkit.NewMemoryStore()
	return &Server{workflows: workflows, runs: runs}, workflows, runs
}

func saveLifecycleTestAgentRun(t *testing.T, store runkit.Store, run runkit.RunRecord) {
	t.Helper()
	if err := store.Create(t.Context(), run); err != nil {
		t.Fatalf("Create AgentRun: %v", err)
	}
}

type failOnceLifecycleRunStore struct {
	runkit.Store
	err    error
	failed bool
}

func (s *failOnceLifecycleRunStore) Complete(
	ctx context.Context,
	runID string,
	summary runkit.TerminalSummary,
) error {
	if !s.failed {
		s.failed = true
		return s.err
	}
	return s.Store.Complete(ctx, runID, summary)
}

type failOnceLifecycleWorkflowStore struct {
	workflowkit.Store
	err    error
	failed bool
}

func (s *failOnceLifecycleWorkflowStore) Update(
	ctx context.Context,
	id string,
	mutate func(workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error),
) (workflowkit.WorkflowRun, error) {
	if !s.failed {
		s.failed = true
		return workflowkit.WorkflowRun{}, s.err
	}
	return s.Store.Update(ctx, id, mutate)
}

type workflowUpdateErrorStore struct {
	workflowkit.Store
	err error
}

func (s *workflowUpdateErrorStore) Update(
	context.Context,
	string,
	func(workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error),
) (workflowkit.WorkflowRun, error) {
	return workflowkit.WorkflowRun{}, s.err
}

type workflowShutdownRaceStore struct {
	outer       workflowkit.WorkflowRun
	current     workflowkit.WorkflowRun
	updateCalls int
	wrote       bool
}

func (s *workflowShutdownRaceStore) Save(ctx context.Context, run workflowkit.WorkflowRun) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.current = run
	return nil
}

func (s *workflowShutdownRaceStore) Get(ctx context.Context, _ string) (workflowkit.WorkflowRun, error) {
	if err := ctx.Err(); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return s.outer, nil
}

func (s *workflowShutdownRaceStore) Update(
	ctx context.Context,
	_ string,
	mutate func(workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error),
) (workflowkit.WorkflowRun, error) {
	if err := ctx.Err(); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	s.updateCalls++
	updated, err := mutate(s.current)
	if err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	updated.UpdatedAt = s.current.UpdatedAt.Add(time.Second)
	s.current = updated
	s.wrote = true
	return updated, nil
}
