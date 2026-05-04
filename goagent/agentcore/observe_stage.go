package agentcore

import "context"
import "fmt"

type ObserveStage struct{}

func (s ObserveStage) Name() string {
	return "observe"
}

func (s ObserveStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if len(state.ToolResults) == 0 {
		return StageContinue, nil
	}
	for _, result := range state.ToolResults {
		if result.Result == nil || result.Result.Silent {
			continue
		}
		content := result.Result.ForLLM
		if result.Result.IsError {
			content = fmt.Sprintf("Tool %s returned a recoverable error: %s. Correct the arguments and try again.", result.Call.Name, result.Result.ForLLM)
		}
		state.Messages = append(state.Messages, Message{
			Role:       "tool",
			Content:    content,
			ToolCallID: result.Call.ID,
		})
	}
	state.ToolResults = nil
	state.PendingCalls = nil
	state.Iteration++
	return StageContinue, nil
}
