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
	active := run.Status == workflowkit.StatusPending || run.Status == workflowkit.StatusRunning
	// A previous attempt may have committed the workflow failure before the
	// AgentRun store failed. Re-enter that exact shutdown state so a later
	// cleanup call can converge without reopening any other terminal workflow.
	shutdownFailed := run.Status == workflowkit.StatusFailed && run.Error == hostShutdownTimeoutCode
	if !active && !shutdownFailed {
		return nil
	}

	var workflowErr error
	if active {
		updated, updateErr := s.workflows.Update(ctx, workflowID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
			if current.Status != workflowkit.StatusPending && current.Status != workflowkit.StatusRunning {
				return current, errWorkflowShutdownUnchanged
			}
			current.Status = workflowkit.StatusFailed
			current.Error = hostShutdownTimeoutCode
			current.LeaseOwner = ""
			current.LeaseUntil = time.Time{}
			return current, nil
		})
		if errors.Is(updateErr, errWorkflowShutdownUnchanged) {
			// The workflow stabilized after the outer read. Its referenced
			// AgentRun belongs to that stable result and must remain untouched.
			return nil
		}
		if updateErr == nil {
			run = updated
		} else {
			workflowErr = updateErr
		}
	}

	// Attempt both stores even when the workflow write failed. Cleanup runs
	// after the execution operation is done, and a later idempotent cleanup
	// call will finish whichever side did not commit.
	agentRunErr := s.finalizeRunningAgentRunShutdown(ctx, run.AgentRunID)
	return errors.Join(workflowErr, agentRunErr)
}

func (s *Server) finalizeRunningAgentRunShutdown(ctx context.Context, agentRunID string) error {
	if agentRunID == "" || s.runs == nil {
		return nil
	}
	agentRun, err := s.runs.Get(ctx, agentRunID)
	if err != nil {
		return err
	}
	if agentRun.Status != runkit.StatusRunning {
		return nil
	}
	summary := agentRun.Summary
	summary.Status = runkit.StatusFailed
	summary.AbortReason = hostShutdownTimeoutCode
	return s.runs.Complete(ctx, agentRunID, summary)
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
	var cleanupErrors []error
	for _, snapshot := range snapshots {
		if err := ctx.Err(); err != nil {
			return joinCleanupErrors(cleanupErrors, err)
		}
		select {
		case <-snapshot.done:
		case <-ctx.Done():
			return joinCleanupErrors(cleanupErrors, ctx.Err())
		}
		if err := ctx.Err(); err != nil {
			return joinCleanupErrors(cleanupErrors, err)
		}
		if snapshot.cleanup != nil {
			if err := snapshot.cleanup(ctx); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
			if err := ctx.Err(); err != nil {
				return joinCleanupErrors(cleanupErrors, err)
			}
		}
	}
	return joinCleanupErrors(cleanupErrors)
}

func joinCleanupErrors(existing []error, additional ...error) error {
	all := append(append([]error(nil), existing...), additional...)
	switch len(all) {
	case 0:
		return nil
	case 1:
		return all[0]
	default:
		return errors.Join(all...)
	}
}
