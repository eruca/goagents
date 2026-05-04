package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrApprovalDenied = errors.New("approval denied")

type ToolApprover interface {
	ApproveTool(ctx context.Context, req ToolApprovalRequest) ToolApprovalDecision
}

type ToolApprovalRequest struct {
	RunID     RunID
	UserID    string
	SessionID string
	Tool      string
	Input     json.RawMessage
	Metadata  map[string]any
}

type ToolApprovalDecision struct {
	Allowed bool
	Reason  string
}

type ApprovalStage struct {
	Approver ToolApprover
}

func (s ApprovalStage) Name() string {
	return "approval"
}

func (s ApprovalStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Approver == nil || len(state.PendingCalls) == 0 {
		return StageContinue, nil
	}
	for i, call := range state.PendingCalls {
		metadata := map[string]any{
			"index": i,
			"tool":  call.Name,
		}
		state.Emit(ctx, Event{
			Type:     EventApprovalRequested,
			Metadata: metadata,
		})
		decision := s.Approver.ApproveTool(ctx, ToolApprovalRequest{
			RunID:     state.RunID,
			UserID:    state.Input.UserID,
			SessionID: state.Input.SessionID,
			Tool:      call.Name,
			Input:     call.Input,
			Metadata:  state.Metadata,
		})
		if !decision.Allowed {
			deniedMetadata := cloneApprovalMetadata(metadata)
			if decision.Reason != "" {
				deniedMetadata["reason"] = decision.Reason
			}
			state.Emit(ctx, Event{
				Type:     EventApprovalDenied,
				Metadata: deniedMetadata,
			})
			return StageAbort, approvalDeniedError(call.Name, decision.Reason)
		}
		completedMetadata := cloneApprovalMetadata(metadata)
		if decision.Reason != "" {
			completedMetadata["reason"] = decision.Reason
		}
		state.Emit(ctx, Event{
			Type:     EventApprovalCompleted,
			Metadata: completedMetadata,
		})
	}
	return StageContinue, nil
}

func approvalDeniedError(tool string, reason string) error {
	if reason == "" {
		reason = "approval denied"
	}
	return fmt.Errorf("%w: tool %q denied: %s", ErrApprovalDenied, tool, reason)
}

func cloneApprovalMetadata(metadata map[string]any) map[string]any {
	copied := make(map[string]any, len(metadata))
	for key, value := range metadata {
		copied[key] = value
	}
	return copied
}
