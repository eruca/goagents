package agentcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/prompt"
	"github.com/eruca/goagent/tools"
)

const runCheckpointVersion = 1

var (
	// ErrApprovalPending reports that a host must resolve pending tool calls before execution can continue.
	ErrApprovalPending = errors.New("approval pending")
	// ErrInvalidRunCheckpoint reports a malformed, unsupported, or incomplete checkpoint.
	ErrInvalidRunCheckpoint = errors.New("invalid run checkpoint")
	// ErrInvalidApprovalResolution reports resolutions that do not exactly match the pending call batch.
	ErrInvalidApprovalResolution = errors.New("invalid approval resolution")
)

// CheckpointRequest is the JSON-safe request context retained for a paused run.
// It intentionally keeps the Run ID in RunCheckpoint so that its JSON form is a string.
type CheckpointRequest struct {
	Input              string              `json:"input"`
	UserID             string              `json:"user_id,omitempty"`
	SessionID          string              `json:"session_id,omitempty"`
	Metadata           map[string]any      `json:"metadata,omitempty"`
	AllowedPermissions []policy.Permission `json:"allowed_permissions,omitempty"`
	PolicyContext      ports.PolicyContext `json:"policy_context"`
}

// RunCheckpoint is a versioned, JSON-serializable snapshot of a run paused before tool execution.
// It includes raw user content and tool inputs, so hosts must store it as sensitive data.
type RunCheckpoint struct {
	Version            int                 `json:"version"`
	RunID              string              `json:"run_id"`
	Request            CheckpointRequest   `json:"request"`
	Iteration          int                 `json:"iteration"`
	Messages           []Message           `json:"messages"`
	Usage              Usage               `json:"usage"`
	LLMCalls           int                 `json:"llm_calls"`
	ToolCalls          int                 `json:"tool_calls"`
	UsedTools          []string            `json:"used_tools,omitempty"`
	StartedAt          time.Time           `json:"started_at"`
	Metadata           map[string]any      `json:"metadata"`
	AllowedPermissions []policy.Permission `json:"allowed_permissions"`
	PendingCalls       []ports.ToolCall    `json:"pending_calls"`
	PromptBlocks       []prompt.Block      `json:"prompt_blocks"`
}

// ToolApprovalResolution is a host decision for one checkpointed pending call.
// Index, ToolCallID, and Tool must exactly match the original call.
type ToolApprovalResolution struct {
	Index      int    `json:"index"`
	ToolCallID string `json:"tool_call_id"`
	Tool       string `json:"tool"`
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason,omitempty"`
}

// ToolApprovalInterruption exposes the checkpoint required to continue a pending approval.
type ToolApprovalInterruption struct {
	Checkpoint RunCheckpoint `json:"checkpoint"`
}

func (c RunCheckpoint) validate() (RunID, error) {
	if c.Version != runCheckpointVersion {
		return RunID{}, fmt.Errorf("%w: version %d", ErrInvalidRunCheckpoint, c.Version)
	}
	runID, err := RunIDFromString(c.RunID)
	if err != nil || runID.IsZero() {
		return RunID{}, fmt.Errorf("%w: run ID", ErrInvalidRunCheckpoint)
	}
	if c.StartedAt.IsZero() {
		return RunID{}, fmt.Errorf("%w: started at", ErrInvalidRunCheckpoint)
	}
	if len(c.PendingCalls) == 0 {
		return RunID{}, fmt.Errorf("%w: pending calls", ErrInvalidRunCheckpoint)
	}
	for i, call := range c.PendingCalls {
		if call.Name == "" {
			return RunID{}, fmt.Errorf("%w: pending call %d has no tool", ErrInvalidRunCheckpoint, i)
		}
	}
	if _, err := json.Marshal(c); err != nil {
		return RunID{}, fmt.Errorf("%w: JSON serialization: %v", ErrInvalidRunCheckpoint, err)
	}
	return runID, nil
}

func checkpointFromState(state *RunState) (RunCheckpoint, error) {
	checkpoint := RunCheckpoint{
		Version:            runCheckpointVersion,
		RunID:              state.RunID.String(),
		Request:            checkpointRequestFromRunRequest(state.Input),
		Iteration:          state.Iteration,
		Messages:           cloneMessages(state.Messages),
		Usage:              state.Usage,
		LLMCalls:           state.summary.LLMCalls,
		ToolCalls:          state.summary.ToolCalls,
		UsedTools:          append([]string(nil), state.summary.UsedTools...),
		StartedAt:          state.startedAt,
		Metadata:           cloneMetadata(state.Metadata),
		AllowedPermissions: append([]policy.Permission(nil), state.AllowedPermissions...),
		PendingCalls:       checkpointToolCalls(state.PendingCalls),
		PromptBlocks:       append([]prompt.Block(nil), state.PromptBlocks...),
	}
	if _, err := checkpoint.validate(); err != nil {
		return RunCheckpoint{}, err
	}
	return checkpoint, nil
}

func checkpointRequestFromRunRequest(req RunRequest) CheckpointRequest {
	return CheckpointRequest{
		Input:              req.Input,
		UserID:             req.UserID,
		SessionID:          req.SessionID,
		Metadata:           cloneMetadata(req.Metadata),
		AllowedPermissions: append([]policy.Permission(nil), req.AllowedPermissions...),
		PolicyContext: ports.PolicyContext{
			TenantID:  req.PolicyContext.TenantID,
			RequestID: req.PolicyContext.RequestID,
			TraceID:   req.PolicyContext.TraceID,
			Labels:    cloneStringMap(req.PolicyContext.Labels),
		},
	}
}

func checkpointToolCalls(calls []tools.Call) []ports.ToolCall {
	cloned := make([]ports.ToolCall, 0, len(calls))
	for _, call := range calls {
		cloned = append(cloned, ports.ToolCall{
			ID:    call.ID,
			Name:  call.Name,
			Input: append(json.RawMessage(nil), call.Input...),
		})
	}
	return cloned
}

func cloneMessages(messages []Message) []Message {
	cloned := make([]Message, 0, len(messages))
	for _, message := range messages {
		copied := Message{
			Role:       message.Role,
			Content:    message.Content,
			ToolCallID: message.ToolCallID,
			ToolCalls:  make([]ports.ToolCall, 0, len(message.ToolCalls)),
		}
		for _, call := range message.ToolCalls {
			copied.ToolCalls = append(copied.ToolCalls, ports.ToolCall{
				ID:    call.ID,
				Name:  call.Name,
				Input: append(json.RawMessage(nil), call.Input...),
			})
		}
		cloned = append(cloned, copied)
	}
	return cloned
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func (c RunCheckpoint) restore(eventSink EventSink) (*RunState, error) {
	runID, err := c.validate()
	if err != nil {
		return nil, err
	}
	state := &RunState{
		RunID:              runID,
		Input:              c.Request.runRequest(runID),
		Iteration:          c.Iteration,
		Messages:           cloneMessages(c.Messages),
		Usage:              c.Usage,
		Metadata:           cloneMetadata(c.Metadata),
		EventSink:          eventSink,
		PromptBlocks:       append([]prompt.Block(nil), c.PromptBlocks...),
		PendingCalls:       restoreToolCalls(c.PendingCalls),
		AllowedPermissions: append([]policy.Permission(nil), c.AllowedPermissions...),
		summary: ExecutionSummary{
			LLMCalls:  c.LLMCalls,
			ToolCalls: c.ToolCalls,
			UsedTools: append([]string(nil), c.UsedTools...),
		},
		startedAt: c.StartedAt,
	}
	state.usedToolLookup = make(map[string]struct{}, len(state.summary.UsedTools))
	for _, name := range state.summary.UsedTools {
		state.usedToolLookup[name] = struct{}{}
	}
	return state, nil
}

func (r CheckpointRequest) runRequest(runID RunID) RunRequest {
	return RunRequest{
		RunID:              runID,
		Input:              r.Input,
		UserID:             r.UserID,
		SessionID:          r.SessionID,
		Metadata:           cloneMetadata(r.Metadata),
		AllowedPermissions: append([]policy.Permission(nil), r.AllowedPermissions...),
		PolicyContext: ports.PolicyContext{
			TenantID:  r.PolicyContext.TenantID,
			RequestID: r.PolicyContext.RequestID,
			TraceID:   r.PolicyContext.TraceID,
			Labels:    cloneStringMap(r.PolicyContext.Labels),
		},
	}
}

func restoreToolCalls(calls []ports.ToolCall) []tools.Call {
	restored := make([]tools.Call, 0, len(calls))
	for _, call := range calls {
		restored = append(restored, tools.Call{
			ID:    call.ID,
			Name:  call.Name,
			Input: append(json.RawMessage(nil), call.Input...),
		})
	}
	return restored
}
