package agentcore

import (
	"context"

	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/prompt"
)

type PromptStage struct {
	Compiler ports.PromptCompiler
	Blocks   []prompt.Block
}

func (s PromptStage) Name() string {
	return "prompt"
}

func (s PromptStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if state.CompiledPrompt != nil || s.Compiler == nil {
		return StageContinue, nil
	}
	blocks := append([]prompt.Block(nil), s.Blocks...)
	blocks = append(blocks, state.PromptBlocks...)
	compiled, err := s.Compiler.Compile(ctx, blocks)
	if err != nil {
		return StageAbort, err
	}
	state.CompiledPrompt = compiled
	return StageContinue, nil
}
