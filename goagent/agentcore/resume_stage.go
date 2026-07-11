package agentcore

import (
	"context"
	"fmt"

	"github.com/eruca/goagent/ports"
)

// ResolvedApprovalStage verifies a complete host decision set before allowing a paused batch to execute.
type ResolvedApprovalStage struct {
	Resolutions []ToolApprovalResolution
}

func (s ResolvedApprovalStage) Name() string {
	return "approval_resume"
}

func (s ResolvedApprovalStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	resolutions, err := validateApprovalResolutions(state.PendingCalls, s.Resolutions)
	if err != nil {
		return StageAbort, err
	}
	for index, resolution := range resolutions {
		if resolution.Allowed {
			continue
		}
		metadata := approvalResolutionMetadata(index, state.PendingCalls[index].Name, resolution.Reason)
		state.Emit(ctx, Event{Type: EventApprovalDenied, Metadata: metadata})
		return StageAbort, approvalDeniedError(state.PendingCalls[index].Name, resolution.Reason)
	}
	for index, resolution := range resolutions {
		metadata := approvalResolutionMetadata(index, state.PendingCalls[index].Name, resolution.Reason)
		state.Emit(ctx, Event{Type: EventApprovalCompleted, Metadata: metadata})
	}
	return StageContinue, nil
}

func validateApprovalResolutions(calls []ports.ToolCall, resolutions []ToolApprovalResolution) ([]ToolApprovalResolution, error) {
	if len(calls) != len(resolutions) {
		return nil, fmt.Errorf("%w: got %d resolutions for %d calls", ErrInvalidApprovalResolution, len(resolutions), len(calls))
	}
	ordered := make([]ToolApprovalResolution, len(calls))
	seen := make([]bool, len(calls))
	for _, resolution := range resolutions {
		if resolution.Index < 0 || resolution.Index >= len(calls) {
			return nil, fmt.Errorf("%w: index %d", ErrInvalidApprovalResolution, resolution.Index)
		}
		if seen[resolution.Index] {
			return nil, fmt.Errorf("%w: duplicate index %d", ErrInvalidApprovalResolution, resolution.Index)
		}
		call := calls[resolution.Index]
		if resolution.ToolCallID != call.ID || resolution.Tool != call.Name {
			return nil, fmt.Errorf("%w: index %d does not match call", ErrInvalidApprovalResolution, resolution.Index)
		}
		seen[resolution.Index] = true
		ordered[resolution.Index] = resolution
	}
	for index, found := range seen {
		if !found {
			return nil, fmt.Errorf("%w: missing index %d", ErrInvalidApprovalResolution, index)
		}
	}
	return ordered, nil
}

func approvalResolutionMetadata(index int, tool string, reason string) map[string]any {
	metadata := map[string]any{
		"index": index,
		"tool":  tool,
	}
	if reason != "" {
		metadata["reason"] = reason
	}
	return metadata
}
