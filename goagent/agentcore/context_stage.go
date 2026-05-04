package agentcore

import "context"

const contextStageInputAdded = "agentcore.context.input_added"

type ContextStage struct{}

func (s ContextStage) Name() string {
	return "context"
}

func (s ContextStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if state.Input.Input == "" || state.Metadata[contextStageInputAdded] == true {
		return StageContinue, nil
	}
	state.Messages = append(state.Messages, Message{Role: "user", Content: state.Input.Input})
	state.Metadata[contextStageInputAdded] = true
	return StageContinue, nil
}
