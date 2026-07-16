package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/workflowkit"
	"github.com/eruca/goagents/workflowkit/agentstep"
)

type mockAgent struct {
	runID agentcore.RunID
}

func (a mockAgent) RunDetailed(ctx context.Context, req agentcore.RunRequest) (*agentcore.RunResult, error) {
	runID := a.runID
	if runID.IsZero() {
		runID = agentcore.NewRunID()
	}
	return &agentcore.RunResult{
		RunID:   runID,
		Content: "agent requested approval to update the draft",
	}, nil
}

type prepareStep struct{}

func (prepareStep) Name() string {
	return "prepare"
}

func (prepareStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	return workflowkit.StepResult{
		Status: workflowkit.StatusRunning,
		Metadata: map[string]any{
			"prepared_input": run.InputRef,
		},
	}, nil
}

type finalizeStep struct{}

func (finalizeStep) Name() string {
	return "finalize"
}

func (finalizeStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	return workflowkit.StepResult{
		Status:    workflowkit.StatusSucceeded,
		OutputRef: "artifact:agent-final",
	}, nil
}

func run(ctx context.Context, out io.Writer) error {
	store := workflowkit.NewMemoryStore()
	agent := mockAgent{runID: agentcore.NewRunID()}
	agentStep := agentstep.New("agent", agent, func(run workflowkit.WorkflowRun) agentcore.RunRequest {
		return agentcore.RunRequest{
			Input:     "review " + run.InputRef,
			SessionID: run.ID,
		}
	}, agentstep.WithResultMapper(func(run workflowkit.WorkflowRun, result *agentcore.RunResult) workflowkit.StepResult {
		return workflowkit.StepResult{
			Status:        workflowkit.StatusWaitingApproval,
			AgentRunID:    result.RunID.String(),
			ApprovalRef:   "approval:" + run.ID,
			WaitingReason: result.Content,
			Metadata: map[string]any{
				"agent_content_preview": result.Content,
			},
		}
	}))

	executor := workflowkit.NewExecutor(store, []workflowkit.Step{
		prepareStep{},
		agentStep,
		finalizeStep{},
	})

	workflow, err := executor.Run(ctx, workflowkit.WorkflowRun{
		ID:       "wf-agent",
		Status:   workflowkit.StatusPending,
		InputRef: "artifact:agent-input",
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "workflow=%s status=%s approval=%s agent_run=%s reason=%q\n", workflow.ID, workflow.Status, workflow.ApprovalRef, workflow.AgentRunID, workflow.WaitingReason)

	workflow, err = executor.Approve(ctx, workflow.ID, workflowkit.Approval{
		AuditRef: "audit:agent-approval-recorded",
		Metadata: map[string]any{
			"approved": true,
		},
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "workflow=%s status=%s output=%s audit=%s completed=%v records=%d\n", workflow.ID, workflow.Status, workflow.OutputRef, workflow.AuditRef, workflow.CompletedSteps, len(workflow.StepRecords))
	return nil
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		panic(err)
	}
}
