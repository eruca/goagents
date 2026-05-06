package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eruca/llmkit/llmkit"
	"github.com/eruca/runkit"
	"github.com/eruca/workflowkit"
)

func TestRuntimeRunsAgentWorkflowWithArtifactsAuditAndLLMRouting(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	runtime, err := NewRuntime(Config{LLMKitHome: home})
	if err != nil {
		t.Fatalf("NewRuntime returned error: %v", err)
	}

	run, err := runtime.Start(ctx, Task{
		ID:    "wf-host-1",
		Input: "Review the draft and prepare an approval summary.",
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if run.Status != workflowkit.StatusWaitingApproval {
		t.Fatalf("run status = %q, want waiting_approval; run=%+v", run.Status, run)
	}
	if run.InputRef == "" || run.OutputRef == "" || run.AgentRunID == "" || run.ApprovalRef == "" {
		t.Fatalf("run refs should be populated: %+v", run)
	}
	if !containsStep(run.CompletedSteps, "ingest") || !containsStep(run.CompletedSteps, "agent_review") {
		t.Fatalf("completed steps = %v, want ingest and agent_review", run.CompletedSteps)
	}

	agentOutput, err := runtime.Artifacts.Get(ctx, run.OutputRef)
	if err != nil {
		t.Fatalf("Artifacts.Get(agent output) returned error: %v", err)
	}
	if !strings.Contains(string(agentOutput.Content), "host runtime draft") {
		t.Fatalf("agent output content = %q, want host runtime draft", string(agentOutput.Content))
	}

	agentRun, err := runtime.AgentRuns.Get(ctx, run.AgentRunID)
	if err != nil {
		t.Fatalf("AgentRuns.Get returned error: %v", err)
	}
	if agentRun.WorkflowID != run.ID || agentRun.Status != runkit.StatusSucceeded {
		t.Fatalf("agent run record = %+v, want succeeded correlated run", agentRun)
	}
	if agentRun.Summary.ContentRef != run.OutputRef || agentRun.Summary.OutputTokens == 0 {
		t.Fatalf("agent run summary = %+v, want content ref and token usage", agentRun.Summary)
	}
	events, err := runtime.AgentRuns.Events(ctx, run.AgentRunID)
	if err != nil {
		t.Fatalf("AgentRuns.Events returned error: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("agent run events should be recorded")
	}
	if !hasAgentEvent(events, "finalized") {
		t.Fatalf("agent events should include finalized: %+v", events)
	}

	route := readSingleJSONL[llmkit.RouteTrace](t, filepath.Join(home, "route-events.jsonl"))
	if route.RouteID == "" || route.TaskID != "wf-host-1" || route.ModelAlias != "local-free" || route.Provider != "local" {
		t.Fatalf("route trace = %+v, want local-free route for task", route)
	}
	outcome := readSingleJSONL[llmkit.TaskOutcome](t, filepath.Join(home, "outcomes.jsonl"))
	if !outcome.Success || outcome.RouteID != route.RouteID || outcome.TaskID != route.TaskID || outcome.ModelAlias != route.ModelAlias {
		t.Fatalf("outcome = %+v, want successful correlated outcome for %+v", outcome, route)
	}

	approved, err := runtime.Approve(ctx, run.ID, Approval{
		ApprovedBy: "operator-1",
		Note:       "accepted for release",
	})
	if err != nil {
		t.Fatalf("Approve returned error: %v", err)
	}
	if approved.Status != workflowkit.StatusSucceeded {
		t.Fatalf("approved status = %q, want succeeded; run=%+v", approved.Status, approved)
	}
	if !containsStep(approved.CompletedSteps, "finalize") {
		t.Fatalf("completed steps = %v, want finalize", approved.CompletedSteps)
	}
	finalArtifact, err := runtime.Artifacts.Get(ctx, approved.OutputRef)
	if err != nil {
		t.Fatalf("Artifacts.Get(final output) returned error: %v", err)
	}
	if !strings.Contains(string(finalArtifact.Content), "approved by operator-1") {
		t.Fatalf("final artifact content = %q, want approval marker", string(finalArtifact.Content))
	}
}

func containsStep(steps []string, want string) bool {
	for _, step := range steps {
		if step == want {
			return true
		}
	}
	return false
}

func hasAgentEvent(events []runkit.RunEvent, want string) bool {
	for _, event := range events {
		if event.Type == want {
			return true
		}
	}
	return false
}

func readSingleJSONL[T any](t *testing.T, path string) T {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) returned error: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("%s lines = %d, want 1; raw=%s", path, len(lines), string(raw))
	}
	var out T
	if err := json.Unmarshal([]byte(lines[0]), &out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return out
}
