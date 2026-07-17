package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
)

const agentApprovalExpiredReason = "agent tool approval expired"

// StartAgentApprovalJanitor starts the host-local expiry loop. It remains
// separate from queued workflow execution because it only reconciles terminal
// tool-approval state.
func (s *Server) StartAgentApprovalJanitor(ctx context.Context) {
	s.janitorStart.Do(func() {
		interval := s.agentApprovalJanitorCfg.interval
		if interval <= 0 {
			interval = defaultAgentApprovalSweepInterval
		}
		go func() {
			defer close(s.janitorDone)
			s.runAgentApprovalJanitor(ctx, interval)
		}()
	})
}

func (s *Server) WaitAgentApprovalJanitor(ctx context.Context) error {
	select {
	case <-s.janitorDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) runAgentApprovalJanitor(ctx context.Context, interval time.Duration) {
	if ctx.Err() != nil {
		return
	}
	s.reconcileExpiredAgentApprovalsNow(ctx)
	if ctx.Err() != nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			s.reconcileExpiredAgentApprovalsNow(ctx)
		}
	}
}

func (s *Server) reconcileExpiredAgentApprovalsNow(ctx context.Context) {
	if _, err := s.ReconcileExpiredAgentApprovals(ctx, time.Now().UTC()); err != nil && ctx.Err() == nil {
		log.Printf("agent approval janitor: %v", err)
	}
}

// ReconcileExpiredAgentApprovals turns local expired tool approvals into
// terminal agent-run and workflow state. It intentionally reads only the safe
// checkpoint identity stored in workflow metadata; encrypted checkpoint content
// is never decrypted during expiry handling.
func (s *Server) ReconcileExpiredAgentApprovals(ctx context.Context, now time.Time) (int, error) {
	if s.agentApprovals == nil || s.queries == nil {
		return 0, nil
	}
	if _, err := s.agentApprovals.checkpoints.ExpireCheckpoints(ctx, now); err != nil {
		return 0, err
	}
	waiting, err := s.queries.ListWorkflows(ctx, workflowkit.WorkflowQuery{
		Status: workflowkit.StatusWaitingApproval,
		Limit:  0,
	})
	if err != nil {
		return 0, err
	}
	reconciled := 0
	for _, workflow := range waiting {
		approval := agentApprovalFromMetadata(workflow.Metadata)
		if approval == nil {
			continue
		}
		checkpoint, err := s.agentApprovals.checkpoints.GetCheckpoint(ctx, approval.CheckpointID, localApprovalTenant)
		if errors.Is(err, runkit.ErrCheckpointNotFound) {
			continue
		}
		if err != nil {
			return reconciled, err
		}
		if checkpoint.Status != runkit.CheckpointExpired {
			continue
		}
		if err := s.runs.Complete(ctx, workflow.AgentRunID, runkit.TerminalSummary{
			Status:      runkit.StatusFailed,
			AbortReason: agentApprovalExpiredReason,
		}); err != nil {
			return reconciled, err
		}
		_, err = s.workflows.Update(ctx, workflow.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
			if !workflowHasPendingAgentApproval(current, approval.CheckpointID) {
				return current, errAgentApprovalNotPending
			}
			clearAgentApprovalMetadata(current.Metadata)
			current.Status = workflowkit.StatusFailed
			current.Error = agentApprovalExpiredReason
			current.ApprovalRef = ""
			current.WaitingReason = ""
			return current, nil
		})
		if errors.Is(err, errAgentApprovalNotPending) {
			continue
		}
		if err != nil {
			return reconciled, err
		}
		reconciled++
	}
	return reconciled, nil
}
