package main

import (
	"context"
	"errors"
	"time"

	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
)

const hostShutdownTimeoutCode = "host_shutdown_timeout"
const hostCleanupTimeout = 5 * time.Second

var errWorkflowShutdownUnchanged = errors.New("workflow shutdown state unchanged")

func (s *Server) finalizeWorkflowShutdown(ctx context.Context, workflowID string) error {
	run, err := s.workflows.Get(ctx, workflowID)
	if err != nil {
		return err
	}
	if run.Status != workflowkit.StatusPending && run.Status != workflowkit.StatusRunning {
		return nil
	}

	_, err = s.workflows.Update(ctx, workflowID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if current.Status != workflowkit.StatusPending && current.Status != workflowkit.StatusRunning {
			return current, errWorkflowShutdownUnchanged
		}
		current.Status = workflowkit.StatusFailed
		current.Error = hostShutdownTimeoutCode
		current.LeaseOwner = ""
		current.LeaseUntil = time.Time{}
		return current, nil
	})
	if errors.Is(err, errWorkflowShutdownUnchanged) {
		return nil
	}
	return err
}

func (s *Server) finalizeAgentApprovalShutdown(
	ctx context.Context,
	workflowID string,
	approval agentApprovalResponse,
	leaseOwner string,
) error {
	checkpoint, err := s.agentApprovals.checkpoints.GetCheckpoint(ctx, approval.CheckpointID, localApprovalTenant)
	if err != nil {
		return err
	}
	switch checkpoint.Status {
	case runkit.CheckpointLeased:
		if checkpoint.LeaseOwner != leaseOwner {
			return nil
		}
	case runkit.CheckpointConsumed:
		// A consumed checkpoint cannot be replayed. Continue below so an
		// incompletely persisted resume is made terminal.
	case runkit.CheckpointFailed:
		if checkpoint.FailureCode != hostShutdownTimeoutCode {
			return nil
		}
	default:
		return nil
	}

	workflowStillPending := false
	_, err = s.workflows.Update(ctx, workflowID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		workflowStillPending = workflowHasPendingAgentApproval(current, approval.CheckpointID)
		// This update is only an atomic state probe. The sentinel rolls the
		// transaction back so stable final or replacement waits keep UpdatedAt.
		return current, errWorkflowShutdownUnchanged
	})
	if !errors.Is(err, errWorkflowShutdownUnchanged) {
		return err
	}
	if !workflowStillPending {
		return nil
	}

	if checkpoint.Status == runkit.CheckpointLeased {
		if err := s.agentApprovals.checkpoints.FailLease(ctx, runkit.CheckpointLeaseCompletion{
			CheckpointID: approval.CheckpointID,
			TenantID:     checkpoint.TenantID,
			LeaseOwner:   leaseOwner,
			FailureCode:  hostShutdownTimeoutCode,
			Now:          time.Now(),
		}); err != nil {
			return err
		}
	}

	agentRun, err := s.runs.Get(ctx, checkpoint.RunID)
	if err != nil {
		return err
	}
	if agentRun.Status == runkit.StatusRunning {
		summary := agentRun.Summary
		summary.Status = runkit.StatusFailed
		summary.AbortReason = hostShutdownTimeoutCode
		if err := s.runs.Complete(ctx, checkpoint.RunID, summary); err != nil {
			return err
		}
	}

	_, err = s.workflows.Update(ctx, workflowID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if !workflowHasPendingAgentApproval(current, approval.CheckpointID) {
			return current, errWorkflowShutdownUnchanged
		}
		clearAgentApprovalMetadata(current.Metadata)
		current.Status = workflowkit.StatusFailed
		current.Error = hostShutdownTimeoutCode
		current.ApprovalRef = ""
		current.WaitingReason = ""
		return current, nil
	})
	if errors.Is(err, errWorkflowShutdownUnchanged) {
		return nil
	}
	return err
}

func waitAndCleanupExecutions(ctx context.Context, snapshots []executionSnapshot) error {
	for _, snapshot := range snapshots {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-snapshot.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if snapshot.cleanup != nil {
			if err := snapshot.cleanup(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
