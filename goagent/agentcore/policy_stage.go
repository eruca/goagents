package agentcore

import (
	"context"
	"errors"
	"fmt"

	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/ports"
)

var ErrPolicyDenied = errors.New("policy denied")

type PolicyStage struct {
	Engine       ports.PolicyEngine
	ToolRegistry ports.ToolRegistry
}

func (s PolicyStage) Name() string {
	return "policy"
}

func (s PolicyStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Engine == nil || len(state.PendingCalls) == 0 {
		return StageContinue, nil
	}
	for _, call := range state.PendingCalls {
		tool, err := s.ToolRegistry.MustGet(call.Name)
		if err != nil {
			return StageAbort, err
		}
		permission := policy.Permission(tool.Spec().Permission)
		decision := s.Engine.Decide(policy.Request{
			RunID:      state.RunID.String(),
			UserID:     state.Input.UserID,
			SessionID:  state.Input.SessionID,
			Tool:       call.Name,
			Permission: permission,
			Input:      call.Input,
			Allowed:    state.AllowedPermissions,
			Context:    state.Input.PolicyContext,
			Metadata:   state.Metadata,
		})
		if !decision.Allowed {
			return StageAbort, fmt.Errorf("%w: tool %q denied: %s", ErrPolicyDenied, call.Name, decision.Reason)
		}
	}
	return StageContinue, nil
}
