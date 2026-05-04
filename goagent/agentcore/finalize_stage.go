package agentcore

import (
	"context"
	"errors"

	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/prompt"
	"github.com/eruca/goagent/tools"
)

var ErrMaxIterations = errors.New("max iterations reached")

type FinalizeStage struct{}

func (s FinalizeStage) Name() string {
	return "finalize"
}

func (s FinalizeStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if state.LastResponse == nil || state.LastResponse.Content == "" || len(state.LastResponse.ToolCalls) > 0 || len(state.PendingCalls) > 0 || len(state.ToolResults) > 0 {
		return StageContinue, nil
	}
	state.Final = &RunResult{
		RunID:            state.RunID,
		Content:          state.LastResponse.Content,
		Usage:            state.Usage,
		ExecutionSummary: state.executionSummary(""),
	}
	return StageBreak, nil
}

type ReActConfig struct {
	LLM                  ports.LLMClient
	PromptCompiler       ports.PromptCompiler
	PromptBlocks         []prompt.Block
	ToolRegistry         MutableToolRegistry
	PolicyEngine         ports.PolicyEngine
	MemoryProvider       ports.MemoryProvider
	SkillProvider        SkillProvider
	SystemPromptProvider SystemPromptProvider
	ToolProvider         ToolProvider
	ContextProjector     ContextProjector
	ToolApprover         ToolApprover
	BudgetGuard          BudgetGuard
	MaxIterations        int
}

type ReActRunner struct {
	pipeline       *Pipeline
	maxIterations  int
	memoryProvider ports.MemoryProvider
}

func NewReActRunner(config ReActConfig) *ReActRunner {
	maxIterations := config.MaxIterations
	if maxIterations <= 0 {
		maxIterations = 8
	}
	registry := config.ToolRegistry
	if registry == nil {
		registry = tools.NewRegistry()
	}
	return &ReActRunner{
		maxIterations:  maxIterations,
		memoryProvider: config.MemoryProvider,
		pipeline: NewPipeline(
			MemoryLoadStage{Provider: config.MemoryProvider},
			ContextStage{},
			SystemPromptStage{Provider: config.SystemPromptProvider},
			SkillStage{Provider: config.SkillProvider},
			ToolProviderStage{Provider: config.ToolProvider, Registry: registry},
			PromptStage{Compiler: config.PromptCompiler, Blocks: config.PromptBlocks},
			ContextProjectionStage{Projector: config.ContextProjector, Budget: budgetFromGuard(config.BudgetGuard)},
			ThinkStage{LLM: config.LLM, ToolRegistry: registry},
			BudgetStage{Guard: config.BudgetGuard},
			PolicyStage{Engine: config.PolicyEngine, ToolRegistry: registry},
			ApprovalStage{Approver: config.ToolApprover},
			ActStage{Executor: tools.NewExecutor(registry)},
			ObserveStage{},
			FinalizeStage{},
		),
	}
}

func (r *ReActRunner) Run(ctx context.Context, state *RunState) (StageResult, error) {
	for i := 0; i < r.maxIterations; i++ {
		result, err := r.pipeline.Run(ctx, state)
		if err != nil {
			return result, err
		}
		if result == StageBreak {
			if err := r.saveMemory(ctx, state); err != nil {
				return StageAbort, err
			}
			state.Emit(ctx, Event{Type: EventFinalized})
			return result, nil
		}
		if result != StageContinue {
			return result, nil
		}
	}
	return StageAbort, ErrMaxIterations
}

func (r *ReActRunner) saveMemory(ctx context.Context, state *RunState) error {
	if r.memoryProvider == nil || state.Input.SessionID == "" || state.Final == nil {
		return nil
	}
	messages := memoryMessages(state.Messages)
	if err := r.memoryProvider.Save(ctx, state.Input.SessionID, messages); err != nil {
		return err
	}
	state.Emit(ctx, Event{
		Type: EventMemorySaved,
		Metadata: map[string]any{
			"session_id":    state.Input.SessionID,
			"message_count": len(messages),
		},
	})
	return nil
}
