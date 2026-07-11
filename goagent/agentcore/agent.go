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
	inputGuard           InputGuard
	toolApprover         ToolApprover
	outputFormat         OutputFormat
	outputValidator      OutputValidator
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

// WithInputGuard screens raw requests before memory, context, model, or tool execution.
func WithInputGuard(guard InputGuard) Option {
	return func(a *Agent) {
		a.inputGuard = guard
	}
}

func WithToolApprover(approver ToolApprover) Option {
	return func(a *Agent) {
		a.toolApprover = approver
	}
}

func WithOutputFormat(format OutputFormat) Option {
	return func(a *Agent) {
		a.outputFormat = cloneOutputFormat(format)
	}
}

func WithOutputValidator(validator OutputValidator) Option {
	return func(a *Agent) {
		a.outputValidator = validator
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

// Resume continues a run interrupted by pending tool approval.
// It returns a nil result with ErrApprovalPending when another approval is required.
func (a *Agent) Resume(ctx context.Context, checkpoint RunCheckpoint, resolutions []ToolApprovalResolution) (*RunResult, error) {
	return a.resume(ctx, checkpoint, resolutions, false, a.eventSink)
}

// ResumeDetailed continues a run interrupted by pending tool approval and preserves partial results on errors.
// A request-scoped ToolProvider must recreate the same tools for the checkpoint request.
func (a *Agent) ResumeDetailed(ctx context.Context, checkpoint RunCheckpoint, resolutions []ToolApprovalResolution) (*RunResult, error) {
	return a.resume(ctx, checkpoint, resolutions, true, a.eventSink)
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
	runner := a.newReActRunner(runRegistry)
	result, err := runner.Run(ctx, state)
	if err != nil {
		return a.runErrorResult(state, detailed, err)
	}
	if result == StageInterrupt {
		return a.pendingRunResult(state, detailed)
	}
	return state.Final, nil
}

func (a *Agent) resume(ctx context.Context, checkpoint RunCheckpoint, resolutions []ToolApprovalResolution, detailed bool, sink EventSink) (*RunResult, error) {
	state, err := checkpoint.restore(sink)
	if err != nil {
		return nil, err
	}
	if _, err := validateApprovalResolutions(state.PendingCalls, resolutions); err != nil {
		return a.runErrorResult(state, detailed, err)
	}
	runRegistry := newRunToolRegistry(a.toolRegistry)
	if err := a.rehydrateToolProvider(ctx, state, runRegistry); err != nil {
		return a.runErrorResult(state, detailed, err)
	}
	result, err := NewPipeline(
		PolicyStage{Engine: a.policyEngine, ToolRegistry: runRegistry},
		ResolvedApprovalStage{Resolutions: resolutions},
		ActStage{Executor: tools.NewExecutor(runRegistry)},
		ObserveStage{},
	).Run(ctx, state)
	if err != nil {
		return a.runErrorResult(state, detailed, err)
	}
	if result != StageContinue {
		return a.runErrorResult(state, detailed, errors.New("resumed approval did not continue"))
	}
	result, err = a.newReActRunner(runRegistry).Run(ctx, state)
	if err != nil {
		return a.runErrorResult(state, detailed, err)
	}
	if result == StageInterrupt {
		return a.pendingRunResult(state, detailed)
	}
	return state.Final, nil
}

func (a *Agent) rehydrateToolProvider(ctx context.Context, state *RunState, registry MutableToolRegistry) error {
	if a.toolProvider == nil {
		return nil
	}
	provided, err := a.toolProvider.Tools(ctx, state.Input)
	if err != nil {
		return err
	}
	for _, tool := range provided {
		registry.Register(tool)
	}
	return nil
}

func (a *Agent) newReActRunner(runRegistry MutableToolRegistry) *ReActRunner {
	return NewReActRunner(ReActConfig{
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
		InputGuard:           a.inputGuard,
		ToolApprover:         a.toolApprover,
		OutputFormat:         a.outputFormat,
		OutputValidator:      a.outputValidator,
		BudgetGuard:          a.budgetGuard,
		MaxIterations:        a.maxIterations,
	})
}

func (a *Agent) runErrorResult(state *RunState, detailed bool, err error) (*RunResult, error) {
	if detailed {
		return &RunResult{
			RunID:            state.RunID,
			Usage:            state.Usage,
			ExecutionSummary: state.executionSummary(err.Error()),
		}, err
	}
	return nil, err
}

func (a *Agent) pendingRunResult(state *RunState, detailed bool) (*RunResult, error) {
	partial, checkpointErr := interruptedRunResult(state)
	if checkpointErr != nil {
		return a.runErrorResult(state, detailed, checkpointErr)
	}
	if detailed {
		return partial, ErrApprovalPending
	}
	return nil, ErrApprovalPending
}

func interruptedRunResult(state *RunState) (*RunResult, error) {
	checkpoint, err := checkpointFromState(state)
	if err != nil {
		return nil, err
	}
	return &RunResult{
		RunID:            state.RunID,
		Usage:            state.Usage,
		ExecutionSummary: state.executionSummary(ErrApprovalPending.Error()),
		Interruption:     &ToolApprovalInterruption{Checkpoint: checkpoint},
	}, nil
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
