package agentstep

import (
	"context"
	"errors"
	"testing"

	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/workflowkit"
)

type fakeRunner struct {
	result *agentcore.RunResult
	err    error
	req    agentcore.RunRequest
}

func (r *fakeRunner) RunDetailed(ctx context.Context, req agentcore.RunRequest) (*agentcore.RunResult, error) {
	r.req = req
	return r.result, r.err
}

func TestAgentStepMapsSuccessfulRun(t *testing.T) {
	runID := agentcore.NewRunID()
	runner := &fakeRunner{result: &agentcore.RunResult{RunID: runID, Content: "done"}}
	step := New("agent", runner, func(run workflowkit.WorkflowRun) agentcore.RunRequest {
		return agentcore.RunRequest{Input: "input:" + run.InputRef}
	})

	result, err := step.Run(context.Background(), workflowkit.WorkflowRun{ID: "wf-1", InputRef: "artifact:in"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if runner.req.Input != "input:artifact:in" {
		t.Fatalf("request = %#v", runner.req)
	}
	if result.Status != workflowkit.StatusSucceeded || result.AgentRunID != runID.String() || result.OutputRef != "agent:"+runID.String() {
		t.Fatalf("result = %#v", result)
	}
	if result.Metadata["content_preview"] != "done" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
}

func TestAgentStepUsesCustomResultMapper(t *testing.T) {
	runID := agentcore.NewRunID()
	runner := &fakeRunner{result: &agentcore.RunResult{RunID: runID, Content: "draft update requested"}}
	step := New("agent", runner, func(run workflowkit.WorkflowRun) agentcore.RunRequest {
		return agentcore.RunRequest{Input: run.InputRef}
	}, WithResultMapper(func(run workflowkit.WorkflowRun, result *agentcore.RunResult) workflowkit.StepResult {
		return workflowkit.StepResult{
			Status:        workflowkit.StatusWaitingApproval,
			AgentRunID:    result.RunID.String(),
			ApprovalRef:   "approval:" + run.ID,
			WaitingReason: result.Content,
		}
	}))

	mapped, err := step.Run(context.Background(), workflowkit.WorkflowRun{ID: "wf-approval", InputRef: "artifact:in"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if mapped.Status != workflowkit.StatusWaitingApproval || mapped.AgentRunID != runID.String() {
		t.Fatalf("mapped = %#v", mapped)
	}
	if mapped.ApprovalRef != "approval:wf-approval" || mapped.WaitingReason != "draft update requested" {
		t.Fatalf("mapped approval = %#v", mapped)
	}
}

func TestAgentStepMapsFailedRunWithPartialResult(t *testing.T) {
	runID := agentcore.NewRunID()
	abort := errors.New("approval denied")
	runner := &fakeRunner{
		result: &agentcore.RunResult{
			RunID: runID,
			ExecutionSummary: agentcore.ExecutionSummary{
				AbortReason: "approval denied: operator rejected",
			},
		},
		err: abort,
	}
	step := New("agent", runner, func(run workflowkit.WorkflowRun) agentcore.RunRequest {
		return agentcore.RunRequest{Input: run.InputRef}
	})

	result, err := step.Run(context.Background(), workflowkit.WorkflowRun{ID: "wf-1", InputRef: "artifact:in"})
	if !errors.Is(err, abort) {
		t.Fatalf("err = %v, want abort", err)
	}
	if result.Status != workflowkit.StatusFailed || result.AgentRunID != runID.String() {
		t.Fatalf("result = %#v", result)
	}
	if result.Error != "approval denied: operator rejected" {
		t.Fatalf("error = %q", result.Error)
	}
}
