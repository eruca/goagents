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
	s.emitCompleted(ctx, state, results)
	state.recordToolResults(results)
	if err != nil {
		// The executor stops at the first failed group, so every call
		// without a result either failed or never ran.
		completed := make(map[int]struct{}, len(results))
		for _, result := range results {
			completed[result.Index] = struct{}{}
		}
		for i, call := range state.PendingCalls {
			if _, ok := completed[i]; ok {
				continue
			}
			state.Emit(ctx, Event{
				Type: EventToolFailed,
				Metadata: map[string]any{
					"index": i,
					"tool":  call.Name,
				},
			})
		}
		return StageAbort, err
	}
	state.ToolResults = results
	return StageContinue, nil
}

func (s ActStage) emitCompleted(ctx context.Context, state *RunState, results []tools.Execution) {
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
}
