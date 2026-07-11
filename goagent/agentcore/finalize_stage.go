package agentcore

import (
	"context"
	"errors"

	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/prompt"
	"github.com/eruca/goagent/tools"
)

var ErrMaxIterations = errors.New("max iterations reached")

type FinalizeStage struct {
	OutputFormat    OutputFormat
	OutputValidator OutputValidator
}

func (s FinalizeStage) Name() string {
	return "finalize"
}

func (s FinalizeStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if state.LastResponse == nil || state.LastResponse.Content == "" || len(state.LastResponse.ToolCalls) > 0 || len(state.PendingCalls) > 0 || len(state.ToolResults) > 0 {
		return StageContinue, nil
	}
	structured, metadata, err := validateFinalOutput(ctx, state, state.LastResponse.Content, s.OutputFormat, s.OutputValidator)
	if err != nil {
		return StageAbort, err
	}
	state.Final = &RunResult{
		RunID:            state.RunID,
		Content:          state.LastResponse.Content,
		StructuredOutput: structured,
		OutputMetadata:   metadata,
		Usage:            state.Usage,
		ExecutionSummary: state.executionSummary(""),
	}
	if len(structured) > 0 || len(metadata) > 0 {
		eventMetadata := map[string]any{}
		if s.OutputFormat.Name != "" {
			eventMetadata["output_format"] = s.OutputFormat.Name
		}
		if len(structured) > 0 {
			eventMetadata["structured"] = true
		}
		state.Emit(ctx, Event{Type: EventOutputValidated, Metadata: eventMetadata})
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
	InputGuard           InputGuard
	ToolApprover         ToolApprover
	OutputFormat         OutputFormat
	OutputValidator      OutputValidator
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
			InputGuardStage{Guard: config.InputGuard},
			MemoryLoadStage{Provider: config.MemoryProvider},
			ContextStage{},
			SystemPromptStage{Provider: config.SystemPromptProvider},
			SkillStage{Provider: config.SkillProvider},
			ToolProviderStage{Provider: config.ToolProvider, Registry: registry},
			OutputFormatStage{Format: config.OutputFormat},
			PromptStage{Compiler: config.PromptCompiler, Blocks: config.PromptBlocks},
			ContextProjectionStage{Projector: config.ContextProjector, Budget: budgetFromGuard(config.BudgetGuard)},
			ThinkStage{LLM: config.LLM, ToolRegistry: registry},
			BudgetStage{Guard: config.BudgetGuard},
			PolicyStage{Engine: config.PolicyEngine, ToolRegistry: registry},
			ApprovalStage{Approver: config.ToolApprover},
			ActStage{Executor: tools.NewExecutor(registry)},
			ObserveStage{},
			FinalizeStage{OutputFormat: config.OutputFormat, OutputValidator: config.OutputValidator},
		),
	}
}

func (r *ReActRunner) Run(ctx context.Context, state *RunState) (StageResult, error) {
	for i := state.Iteration; i < r.maxIterations; i++ {
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
