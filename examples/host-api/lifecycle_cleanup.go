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
const cleanupReconciliationInterval = 10 * time.Millisecond

var errWorkflowShutdownUnchanged = errors.New("workflow shutdown state unchanged")

type pendingExecutionCleanup struct {
	cleanup executionCleanup
	lastErr error
}

func (s *Server) finalizeWorkflowShutdown(ctx context.Context, workflowID string) error {
	return s.finalizeWorkflowShutdownTracked(ctx, workflowID, nil)
}

func (s *Server) finalizeWorkflowShutdownTracked(
	ctx context.Context,
	workflowID string,
	tracker *pendingShutdownTracker,
) error {
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

	// Attempt every durable side even when another write failed. Cleanup runs
	// after the execution operation is done, and Fix B re-enters this exact
	// shutdown state within the same cleanup budget.
	identity, hasPendingIdentity := tracker.Snapshot()
	pendingErr := s.finalizePendingCheckpointShutdown(ctx, identity, hasPendingIdentity)
	agentRunID := run.AgentRunID
	if hasPendingIdentity {
		agentRunID = identity.RunID
	}
	agentRunErr := s.finalizeRunningAgentRunShutdown(ctx, agentRunID)
	return errors.Join(workflowErr, pendingErr, agentRunErr)
}

func (s *Server) finalizePendingCheckpointShutdown(
	ctx context.Context,
	identity pendingShutdownIdentity,
	present bool,
) error {
	if !present || s.agentApprovals == nil || s.agentApprovals.pendingFailures == nil {
		return nil
	}
	err := s.agentApprovals.pendingFailures.FailPendingCheckpoint(ctx, runkit.PendingCheckpointFailure{
		CheckpointID:   identity.CheckpointID,
		RunID:          identity.RunID,
		TenantID:       identity.TenantID,
		DefinitionHash: identity.DefinitionHash,
		FailureCode:    hostShutdownTimeoutCode,
		Now:            time.Now().UTC(),
	})
	// A terminal checkpoint is owned by the state transition that reached it.
	// Cleanup preserves it instead of retrying or rewriting that transition.
	if errors.Is(err, runkit.ErrCheckpointNotClaimable) {
		return nil
	}
	return err
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
	return s.finalizeAgentApprovalShutdownTracked(ctx, workflowID, approval, leaseOwner, nil)
}

func (s *Server) finalizeAgentApprovalShutdownTracked(
	ctx context.Context,
	workflowID string,
	approval agentApprovalResponse,
	leaseOwner string,
	tracker *pendingShutdownTracker,
) error {
	identity, hasPendingIdentity := tracker.Snapshot()
	workflowStillPending := false
	workflowAlreadyFailed := false
	stableNextPause := false
	_, err := s.workflows.Update(ctx, workflowID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		workflowStillPending = workflowHasPendingAgentApproval(current, approval.CheckpointID)
		workflowAlreadyFailed = current.Status == workflowkit.StatusFailed && current.Error == hostShutdownTimeoutCode
		stableNextPause = hasPendingIdentity && workflowHasPendingAgentApproval(current, identity.CheckpointID)
		// This update is only an atomic state probe. The sentinel rolls the
		// transaction back so stable final or replacement waits keep UpdatedAt.
		return current, errWorkflowShutdownUnchanged
	})
	if !errors.Is(err, errWorkflowShutdownUnchanged) {
		return err
	}
	if stableNextPause || (!workflowStillPending && !workflowAlreadyFailed) {
		return nil
	}

	checkpoint, checkpointErr := s.agentApprovals.checkpoints.GetCheckpoint(ctx, approval.CheckpointID, localApprovalTenant)
	checkpointRelevant := checkpointErr == nil
	if checkpointErr == nil {
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
	} else if !hasPendingIdentity {
		return checkpointErr
	}

	var leaseErr error
	if checkpointRelevant && checkpoint.Status == runkit.CheckpointLeased {
		leaseErr = s.agentApprovals.checkpoints.FailLease(ctx, runkit.CheckpointLeaseCompletion{
			CheckpointID: approval.CheckpointID,
			TenantID:     checkpoint.TenantID,
			LeaseOwner:   leaseOwner,
			FailureCode:  hostShutdownTimeoutCode,
			Now:          time.Now().UTC(),
		})
	}

	pendingErr := s.finalizePendingCheckpointShutdown(ctx, identity, hasPendingIdentity)
	agentRunID := checkpoint.RunID
	if hasPendingIdentity {
		agentRunID = identity.RunID
	}
	agentRunErr := s.finalizeRunningAgentRunShutdown(ctx, agentRunID)

	var workflowErr error
	if workflowStillPending && !workflowAlreadyFailed {
		_, workflowErr = s.workflows.Update(ctx, workflowID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
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
		if errors.Is(workflowErr, errWorkflowShutdownUnchanged) {
			workflowErr = nil
		}
	}
	return errors.Join(checkpointErr, leaseErr, pendingErr, agentRunErr, workflowErr)
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
	}

	pending := make([]pendingExecutionCleanup, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.cleanup != nil {
			pending = append(pending, pendingExecutionCleanup{cleanup: snapshot.cleanup})
		}
	}

	for len(pending) > 0 {
		failed := make([]pendingExecutionCleanup, 0, len(pending))
		for index, item := range pending {
			if err := ctx.Err(); err != nil {
				failed = append(failed, pending[index:]...)
				return pendingCleanupErrors(failed, err)
			}
			item.lastErr = item.cleanup(ctx)
			if item.lastErr != nil {
				failed = append(failed, item)
			}
			if err := ctx.Err(); err != nil {
				failed = append(failed, pending[index+1:]...)
				return pendingCleanupErrors(failed, err)
			}
		}
		if len(failed) == 0 {
			return nil
		}

		timer := time.NewTimer(cleanupReconciliationInterval)
		select {
		case <-timer.C:
			pending = failed
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return pendingCleanupErrors(failed, ctx.Err())
		}
	}
	return nil
}

func pendingCleanupErrors(pending []pendingExecutionCleanup, additional ...error) error {
	cleanupErrors := make([]error, 0, len(pending)+len(additional))
	for _, item := range pending {
		if item.lastErr != nil {
			cleanupErrors = append(cleanupErrors, item.lastErr)
		}
	}
	return joinCleanupErrors(cleanupErrors, additional...)
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
