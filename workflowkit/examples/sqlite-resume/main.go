package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/eruca/workflowkit"
	"github.com/eruca/workflowkit/sqlitestore"
)

type approvalStep struct{}

func (approvalStep) Name() string {
	return "approval"
}

func (approvalStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	return workflowkit.StepResult{
		Status:        workflowkit.StatusWaitingApproval,
		ApprovalRef:   "approval:sqlite",
		WaitingReason: "operator approval required",
	}, nil
}

type finalizeStep struct{}

func (finalizeStep) Name() string {
	return "finalize"
}

func (finalizeStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	return workflowkit.StepResult{
		Status:    workflowkit.StatusSucceeded,
		OutputRef: "artifact:sqlite-final",
	}, nil
}

func run(ctx context.Context, dbPath string, out io.Writer) error {
	store, err := sqlitestore.Open(dbPath)
	if err != nil {
		return err
	}
	executor := workflowkit.NewExecutor(store, []workflowkit.Step{
		approvalStep{},
		finalizeStep{},
	})
	workflow, err := executor.Run(ctx, workflowkit.WorkflowRun{
		ID:       "wf-sqlite",
		Status:   workflowkit.StatusPending,
		InputRef: "artifact:sqlite-input",
	})
	if err != nil {
		_ = store.Close()
		return err
	}
	fmt.Fprintf(out, "workflow=%s status=%s approval=%s\n", workflow.ID, workflow.Status, workflow.ApprovalRef)
	if err := store.Close(); err != nil {
		return err
	}

	reopened, err := sqlitestore.Open(dbPath)
	if err != nil {
		return err
	}
	defer reopened.Close()
	resumed := workflowkit.NewExecutor(reopened, []workflowkit.Step{
		approvalStep{},
		finalizeStep{},
	})
	workflow, err = resumed.Approve(ctx, workflow.ID, workflowkit.Approval{AuditRef: "audit:sqlite-approval-recorded"})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "workflow=%s status=%s output=%s audit=%s records=%d\n", workflow.ID, workflow.Status, workflow.OutputRef, workflow.AuditRef, len(workflow.StepRecords))
	return nil
}

func main() {
	dbPath := "workflowkit-example.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}
	if err := run(context.Background(), dbPath, os.Stdout); err != nil {
		panic(err)
	}
}
