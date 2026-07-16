package agentcore

import (
	"context"

	"github.com/eruca/goagents/goagent/tools"
)

type ActStage struct {
	Executor *tools.Executor
}

func (s ActStage) Name() string {
	return "act"
}

func (s ActStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if len(state.PendingCalls) == 0 {
		return StageContinue, nil
	}
	for _, call := range state.PendingCalls {
		state.Emit(ctx, Event{
			Type: EventToolStarted,
			Metadata: map[string]any{
				"tool": call.Name,
			},
		})
	}
	results, err := s.Executor.Execute(ctx, state.PendingCalls, tools.Env{
		UserID:    state.Input.UserID,
		SessionID: state.Input.SessionID,
		Metadata:  state.Metadata,
	})
	if err != nil {
		for _, call := range state.PendingCalls {
			state.Emit(ctx, Event{
				Type: EventToolFailed,
				Metadata: map[string]any{
					"tool": call.Name,
				},
			})
		}
		return StageAbort, err
	}
	for _, result := range results {
		metadata := map[string]any{
			"index": result.Index,
			"tool":  result.Call.Name,
		}
		if result.Result != nil && result.Result.Ref != "" {
			metadata["ref"] = result.Result.Ref
		}
		if result.Result != nil {
			metadata["is_error"] = result.Result.IsError
		}
		state.Emit(ctx, Event{
			Type:     EventToolCompleted,
			Metadata: metadata,
		})
	}
	state.recordToolResults(results)
	state.ToolResults = results
	return StageContinue, nil
}
