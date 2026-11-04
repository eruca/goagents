package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
)

func (s *Server) handleApproveAgentTool(w http.ResponseWriter, r *http.Request) {
	identity, err := s.approvalAuthenticator.AuthenticateApproval(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		writeApprovalUnauthorized(w)
		return
	}
	var req agentApprovalRequest
	if !decodeJSONStrict(w, r, &req) {
		return
	}
	if len(req.Resolutions) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "at least one tool resolution is required")
		return
	}
	resolutions := req.coreResolutions()
	workflowID := r.PathValue("id")
	run, err := s.workflows.Get(r.Context(), workflowID)
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	approval := agentApprovalFromMetadata(run.Metadata)
	if run.Status == workflowkit.StatusWaitingApproval && approval == nil && run.ApprovalRef == "approval:"+run.ID && run.OutputRef != "" {
		if completed := completedAgentApprovalFromMetadata(run.Metadata); completed != nil && resolutionsMatchCompletedApproval(resolutions, completed.Tools) {
			writeJSON(w, http.StatusOK, workflowToResponse(run, RunModeSync))
			return
		}
	}
	if run.Status != workflowkit.StatusWaitingApproval || approval == nil || s.agentApprovals == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", errAgentApprovalNotPending.Error())
		return
	}
	if hasDeniedResolution(resolutions) {
		if err := s.agentApprovals.Reject(r.Context(), workflowID, *approval, identity.Subject); err != nil {
			writeError(w, http.StatusConflict, "workflow_error", "agent tool approval could not be rejected")
			return
		}
		updated, err := s.rejectAgentApprovalWorkflow(r.Context(), run, "agent tool approval rejected")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "workflow_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, workflowToResponse(updated, RunModeSync))
		return
	}

	approvalToResume := *approval
	leaseOwner := "host-api:" + agentcore.NewRunID().String()
	pendingShutdown := newPendingShutdownTracker()
	handle, accepted := s.executions.Begin(workflowID, executionAgentApproval, func(ctx context.Context) error {
		return s.finalizeAgentApprovalShutdownTracked(ctx, workflowID, approvalToResume, leaseOwner, pendingShutdown)
	})
	if !accepted {
		writeHostDraining(w)
		return
	}
	defer handle.Done()
	operationCtx := withPendingShutdownTracker(r.Context(), pendingShutdown)
	next, result, resumeErr := s.agentApprovals.ApproveAndResume(operationCtx, workflowID, approvalToResume, identity.Subject, resolutions, leaseOwner)
	if errors.Is(resumeErr, runkit.ErrCheckpointNotClaimable) {
		writeError(w, http.StatusConflict, "approval_conflict", "agent tool approval is already being processed")
		return
	}
	if errors.Is(resumeErr, agentcore.ErrApprovalPending) && result != nil && result.Interruption != nil && len(next.Tools) > 0 {
		updated, err := s.replacePendingAgentApproval(operationCtx, run, approval.CheckpointID, next)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "workflow_error", err.Error())
			return
		}
		pendingShutdown.Clear(next.CheckpointID)
		writeJSON(w, http.StatusAccepted, workflowToResponse(updated, RunModeSync))
		return
	}
	if resumeErr != nil || result == nil {
		_ = s.failAgentApprovalWorkflow(operationCtx, run, approval.CheckpointID, result, errAgentApprovalResumeFailed)
		if errors.Is(resumeErr, agentcore.ErrInvalidApprovalResolution) {
			writeError(w, http.StatusBadRequest, "invalid_request", "tool resolutions do not match the pending approval")
			return
		}
		writeError(w, http.StatusInternalServerError, "workflow_error", errAgentApprovalResumeFailed.Error())
		return
	}

	updated, err := s.persistResumedAgentResult(operationCtx, run, approval.CheckpointID, result)
	if err != nil {
		_ = s.failAgentApprovalWorkflow(operationCtx, run, approval.CheckpointID, result, err)
		writeError(w, http.StatusInternalServerError, "workflow_error", "agent approval result persistence failed")
		return
	}
	writeJSON(w, http.StatusOK, workflowToResponse(updated, RunModeSync))
}

func hasDeniedResolution(resolutions []agentcore.ToolApprovalResolution) bool {
	for _, resolution := range resolutions {
		if !resolution.Allowed {
			return true
		}
	}
	return false
}

func (s *Server) persistResumedAgentResult(ctx context.Context, run workflowkit.WorkflowRun, checkpointID string, result *agentcore.RunResult) (workflowkit.WorkflowRun, error) {
	outputRef := "artifact:" + result.RunID.String() + ":agent-output"
	if err := putTextArtifact(ctx, s.artifacts, outputRef, result.Content); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if err := s.runs.Complete(ctx, result.RunID.String(), runkit.TerminalSummary{
		Status:       runkit.StatusSucceeded,
		ContentRef:   outputRef,
		InputTokens:  result.Usage.InputTokens,
		OutputTokens: result.Usage.OutputTokens,
		LLMCalls:     result.ExecutionSummary.LLMCalls,
		ToolCalls:    result.ExecutionSummary.ToolCalls,
		UsedTools:    result.ExecutionSummary.UsedTools,
	}); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	if s.memory != nil {
		scope, scopeErr := resolveTrustedMemoryScope(run.Metadata)
		enqueueErr := scopeErr
		if enqueueErr == nil {
			enqueueErr = s.memory.EnqueueExtraction(ctx, scope, outputRef, result.RunID.String())
		}
		if enqueueErr != nil {
			s.memory.observer.RecordMemoryEvent(withMemoryObserverRunID(ctx, result.RunID.String()), "memory.degraded", map[string]any{
				"memory.degraded_channels": []string{"extraction_enqueue"},
			})
		}
	}
	return s.workflows.Update(ctx, run.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		pending := agentApprovalFromMetadata(current.Metadata)
		if current.Status != workflowkit.StatusWaitingApproval || pending == nil || pending.CheckpointID != checkpointID {
			return current, errAgentApprovalNotPending
		}
		rememberCompletedAgentApprovalMetadata(current.Metadata, *pending)
		clearAgentApprovalMetadata(current.Metadata)
		current.OutputRef = outputRef
		current.AgentRunID = result.RunID.String()
		current.ApprovalRef = "approval:" + current.ID
		current.WaitingReason = "operator approval required before finalizing host API output"
		current.Metadata["agent_output_ref"] = outputRef
		return current, nil
	})
}

func (s *Server) replacePendingAgentApproval(ctx context.Context, run workflowkit.WorkflowRun, previousCheckpointID string, approval agentApprovalResponse) (workflowkit.WorkflowRun, error) {
	return s.workflows.Update(ctx, run.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if !workflowHasPendingAgentApproval(current, previousCheckpointID) {
			return current, errAgentApprovalNotPending
		}
		for key, value := range approval.workflowMetadata() {
			current.Metadata[key] = value
		}
		current.ApprovalRef = "agent-approval:" + approval.CheckpointID
		current.WaitingReason = "operator tool approval required before record_review executes"
		return current, nil
	})
}

func (s *Server) rejectAgentApprovalWorkflow(ctx context.Context, run workflowkit.WorkflowRun, reason string) (workflowkit.WorkflowRun, error) {
	if err := s.runs.Complete(ctx, run.AgentRunID, runkit.TerminalSummary{
		Status:      runkit.StatusFailed,
		AbortReason: reason,
	}); err != nil {
		return workflowkit.WorkflowRun{}, err
	}
	return s.workflows.Update(ctx, run.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if current.Status != workflowkit.StatusWaitingApproval || agentApprovalFromMetadata(current.Metadata) == nil {
			return current, errAgentApprovalNotPending
		}
		clearAgentApprovalMetadata(current.Metadata)
		current.Status = workflowkit.StatusCancelled
		current.Error = reason
		current.ApprovalRef = ""
		current.WaitingReason = ""
		return current, nil
	})
}

func (s *Server) failAgentApprovalWorkflow(ctx context.Context, run workflowkit.WorkflowRun, checkpointID string, result *agentcore.RunResult, cause error) error {
	_ = completeFailedAgentRun(ctx, s.runs, result, cause)
	_, err := s.workflows.Update(ctx, run.ID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if !workflowHasPendingAgentApproval(current, checkpointID) {
			return current, errAgentApprovalNotPending
		}
		clearAgentApprovalMetadata(current.Metadata)
		current.Status = workflowkit.StatusFailed
		current.Error = "agent tool approval processing failed"
		current.ApprovalRef = ""
		current.WaitingReason = ""
		return current, nil
	})
	return err
}

func clearAgentApprovalMetadata(metadata map[string]any) {
	delete(metadata, agentApprovalMetadataID)
	delete(metadata, agentApprovalMetadataTools)
}

func workflowHasPendingAgentApproval(run workflowkit.WorkflowRun, checkpointID string) bool {
	approval := agentApprovalFromMetadata(run.Metadata)
	return run.Status == workflowkit.StatusWaitingApproval && approval != nil && approval.CheckpointID == checkpointID
}
