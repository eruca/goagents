package agentcore

import (
	"context"

	"github.com/eruca/goagent/tools"
)

type ToolProviderStage struct {
	Provider ToolProvider
	Registry interface {
		Register(tool tools.Tool)
	}
}

func (s ToolProviderStage) Name() string {
	return "tool_provider"
}

func (s ToolProviderStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Provider == nil || state.Metadata[toolProviderLoadedKey] == true {
		return StageContinue, nil
	}
	provided, err := s.Provider.Tools(ctx, state.Input)
	if err != nil {
		return StageAbort, err
	}
	for _, tool := range provided {
		s.Registry.Register(tool)
	}
	state.Metadata[toolProviderLoadedKey] = true
	return StageContinue, nil
}

const toolProviderLoadedKey = "agentcore.tool_provider.loaded"
