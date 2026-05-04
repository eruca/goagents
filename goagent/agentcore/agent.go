package agentcore

import (
	"context"
	"errors"

	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/prompt"
	"github.com/eruca/goagent/tools"
)

var ErrMissingLLM = errors.New("agent requires LLM client")

type Agent struct {
	llm                  ports.LLMClient
	promptCompiler       ports.PromptCompiler
	promptBlocks         []prompt.Block
	toolRegistry         ports.ToolRegistry
	policyEngine         ports.PolicyEngine
	memoryProvider       ports.MemoryProvider
	skillProvider        SkillProvider
	systemPromptProvider SystemPromptProvider
	toolProvider         ToolProvider
	contextProjector     ContextProjector
	eventSink            EventSink
	toolApprover         ToolApprover
	budgetGuard          BudgetGuard
	maxIterations        int
}

type Option func(*Agent)

func WithLLM(llm ports.LLMClient) Option {
	return func(a *Agent) {
		a.llm = llm
	}
}

func WithPromptCompiler(c ports.PromptCompiler) Option {
	return func(a *Agent) {
		a.promptCompiler = c
	}
}

func WithToolRegistry(r ports.ToolRegistry) Option {
	return func(a *Agent) {
		a.toolRegistry = r
	}
}

func WithPromptBlocks(blocks []prompt.Block) Option {
	return func(a *Agent) {
		a.promptBlocks = append([]prompt.Block(nil), blocks...)
	}
}

func WithPolicyEngine(engine ports.PolicyEngine) Option {
	return func(a *Agent) {
		a.policyEngine = engine
	}
}

func WithMemoryProvider(provider ports.MemoryProvider) Option {
	return func(a *Agent) {
		a.memoryProvider = provider
	}
}

func WithSkillProvider(provider SkillProvider) Option {
	return func(a *Agent) {
		a.skillProvider = provider
	}
}

func WithSystemPromptProvider(provider SystemPromptProvider) Option {
	return func(a *Agent) {
		a.systemPromptProvider = provider
	}
}

func WithToolProvider(provider ToolProvider) Option {
	return func(a *Agent) {
		a.toolProvider = provider
	}
}

func WithContextProjector(projector ContextProjector) Option {
	return func(a *Agent) {
		a.contextProjector = projector
	}
}

func WithEventSink(sink EventSink) Option {
	return func(a *Agent) {
		a.eventSink = sink
	}
}

func WithToolApprover(approver ToolApprover) Option {
	return func(a *Agent) {
		a.toolApprover = approver
	}
}

func WithBudget(budget Budget) Option {
	return func(a *Agent) {
		a.budgetGuard = NewBudgetGuard(budget)
	}
}

func WithBudgetGuard(guard BudgetGuard) Option {
	return func(a *Agent) {
		a.budgetGuard = guard
	}
}

func WithModule(module Module) Option {
	return func(a *Agent) {
		a.systemPromptProvider = module
		a.skillProvider = module
		a.toolProvider = module
	}
}

func WithMaxIterations(maxIterations int) Option {
	return func(a *Agent) {
		a.maxIterations = maxIterations
	}
}

func NewAgent(options ...Option) (*Agent, error) {
	agent := &Agent{
		promptCompiler: prompt.NewCompiler(),
		toolRegistry:   tools.NewRegistry(),
		policyEngine:   policy.NewEngine(),
		maxIterations:  8,
	}
	for _, option := range options {
		option(agent)
	}
	if agent.llm == nil {
		return nil, ErrMissingLLM
	}
	if agent.promptCompiler == nil {
		agent.promptCompiler = prompt.NewCompiler()
	}
	if agent.toolRegistry == nil {
		agent.toolRegistry = tools.NewRegistry()
	}
	if agent.policyEngine == nil {
		agent.policyEngine = policy.NewEngine()
	}
	return agent, nil
}

func (a *Agent) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	return a.run(ctx, req, false)
}

func (a *Agent) RunDetailed(ctx context.Context, req RunRequest) (*RunResult, error) {
	return a.run(ctx, req, true)
}

func (a *Agent) run(ctx context.Context, req RunRequest, detailed bool) (*RunResult, error) {
	return a.runWithEventSink(ctx, req, detailed, a.eventSink)
}

func (a *Agent) runWithEventSink(ctx context.Context, req RunRequest, detailed bool, sink EventSink) (*RunResult, error) {
	runID := req.RunID
	if runID.IsZero() {
		runID = NewRunID()
	}
	state := NewRunState(runID, req)
	state.EventSink = sink
	runRegistry := newRunToolRegistry(a.toolRegistry)
	runner := NewReActRunner(ReActConfig{
		LLM:                  a.llm,
		PromptCompiler:       a.promptCompiler,
		PromptBlocks:         a.promptBlocks,
		ToolRegistry:         runRegistry,
		PolicyEngine:         a.policyEngine,
		MemoryProvider:       a.memoryProvider,
		SkillProvider:        a.skillProvider,
		SystemPromptProvider: a.systemPromptProvider,
		ToolProvider:         a.toolProvider,
		ContextProjector:     a.contextProjector,
		ToolApprover:         a.toolApprover,
		BudgetGuard:          a.budgetGuard,
		MaxIterations:        a.maxIterations,
	})
	if _, err := runner.Run(ctx, state); err != nil {
		if detailed {
			return &RunResult{
				RunID:            state.RunID,
				Usage:            state.Usage,
				ExecutionSummary: state.executionSummary(err.Error()),
			}, err
		}
		return nil, err
	}
	return state.Final, nil
}

func newRunToolRegistry(registry ports.ToolRegistry) MutableToolRegistry {
	if registry == nil {
		registry = tools.NewRegistry()
	}
	if concrete, ok := registry.(*tools.Registry); ok {
		registry = concrete.Clone()
	}
	return &runToolRegistry{
		base:   registry,
		scoped: tools.NewRegistry(),
	}
}
