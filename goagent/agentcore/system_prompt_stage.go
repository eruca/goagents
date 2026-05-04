package agentcore

import "context"

type SystemPromptStage struct {
	Provider SystemPromptProvider
}

func (s SystemPromptStage) Name() string {
	return "system_prompt"
}

func (s SystemPromptStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Provider == nil || state.Metadata[systemPromptLoadedKey] == true {
		return StageContinue, nil
	}
	blocks, err := s.Provider.SystemPrompt(ctx, state.Input)
	if err != nil {
		return StageAbort, err
	}
	state.PromptBlocks = append(state.PromptBlocks, blocks...)
	state.Metadata[systemPromptLoadedKey] = true
	return StageContinue, nil
}

const systemPromptLoadedKey = "agentcore.system_prompt.loaded"
