package main

import (
	"context"
	"fmt"

	"github.com/eruca/workflowkit"
)

type prepareStep struct{}

var prepareAttempts int

func (prepareStep) Name() string {
	return "prepare"
}

func (prepareStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	prepareAttempts++
	if prepareAttempts == 1 {
		return workflowkit.StepResult{}, workflowkit.TransientError{Err: fmt.Errorf("temporary prepare outage")}
	}
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
		OutputRef: "artifact:workflow-output",
	}, nil
}

type approvalStep struct{}

func (approvalStep) Name() string {
	return "approval"
}

func (approvalStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	if run.Metadata["approved"] == true {
		return workflowkit.StepResult{Status: workflowkit.StatusRunning}, nil
	}
	return workflowkit.StepResult{
		Status:        workflowkit.StatusWaitingApproval,
		ApprovalRef:   "approval:basic",
		WaitingReason: "operator approval required",
	}, nil
}

func main() {
	ctx := context.Background()
	store := workflowkit.NewMemoryStore()
	executor := workflowkit.NewExecutor(store, []workflowkit.Step{
		prepareStep{},
		approvalStep{},
		finalizeStep{},
	}, workflowkit.WithRetryPolicy(workflowkit.RetryPolicy{MaxAttempts: 2}))

	run, err := executor.Run(ctx, workflowkit.WorkflowRun{
		ID:       "wf-basic",
		Status:   workflowkit.StatusPending,
		InputRef: "artifact:workflow-input",
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("workflow=%s status=%s approval=%s reason=%q prepare_attempts=%d\n", run.ID, run.Status, run.ApprovalRef, run.WaitingReason, run.StepAttempts["prepare"])

	run, err = executor.Approve(ctx, run.ID, workflowkit.Approval{
		AuditRef: "audit:approval-recorded",
		Metadata: map[string]any{
			"approved": true,
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("workflow=%s status=%s output=%s audit=%s completed=%v records=%d\n", run.ID, run.Status, run.OutputRef, run.AuditRef, run.CompletedSteps, len(run.StepRecords))
}
