# Go Agent Framework Design

> Date: 2026-04-10
> Goal: Design a mature, high-cohesion and low-coupling Go framework that can be embedded into other Go projects to provide LLM Agent capabilities.

## 1. Design Positioning

This framework should be a library, not a full product platform. The core must stay small: it owns the ReAct execution model, state transitions, and typed extension ports. Business projects own domain prompts, tools, skills, memory backends, permissions, and transport concerns.

The design borrows selectively from:

- `smallnest/goclaw`: orchestrator, tool registry, skill loading, event emission.
- `NousResearch/hermes-agent`: structured prompts, tool schema discipline, output parsing, self-correction.
- `ultraworkers/claw-code`: permissions, hooks, runtime boundary, deterministic mock testing.
- `nextlevelbuilder/goclaw`: stage pipeline, stateless stages, `RunState`, prompt modes, tool result separation, deterministic tool ordering, safe parallel tool execution.

The framework should not copy platform concerns such as multi-channel messaging, multi-tenant PostgreSQL, agent teams, self-evolution, or product-specific workspaces into the core. Those belong in optional extension packages.

## 2. Architecture

```text
app project
  domain module
    SystemPromptProvider
    SkillProvider
    ToolProvider
    Policy config
    Memory adapter

agentcore
  Agent
  RunState
  Stage Pipeline
  ReAct state machine
  Output parser
  Error/self-correction loop

ports
  LLMClient
  PromptCompiler
  ToolRegistry
  PolicyEngine
  MemoryProvider
  EventSink
  SessionStore
  BudgetGuard

extensions
  providers/openai
  providers/anthropic
  providers/ollama
  memory/pgvector
  transport/http+sse
  observability/otel
  audit/postgres
```

## 3. Package Layout

```text
/agentcore
  agent.go
  run_state.go
  request.go
  stage.go
  pipeline.go
  react.go
  parser.go

/ports
  llm.go
  prompt.go
  tools.go
  policy.go
  memory.go
  events.go
  session.go
  budget.go

/prompt
  compiler.go
  blocks.go
  modes.go
  renderer_json.go
  renderer_xml.go

/tools
  registry.go
  spec.go
  result.go
  executor.go
  middleware.go
  schema.go

/policy
  engine.go
  permission.go
  capability.go

/memory
  provider.go
  window.go
  summary.go

/extensions
  providers/openai
  providers/anthropic
  providers/ollama
  transport/sse
  observability/otel
```

## 4. Core Types

```go
type Agent struct {
    llm    ports.LLMClient
    prompt ports.PromptCompiler
    tools  ports.ToolRegistry
    policy ports.PolicyEngine
    memory ports.MemoryProvider
    events ports.EventSink
    budget ports.BudgetGuard
    stages []Stage
}

type Stage interface {
    Name() string
    Run(ctx context.Context, state *RunState) (StageResult, error)
}

type StageResult int

const (
    StageContinue StageResult = iota
    StageBreak
    StageAbort
)
```

`Agent` should not know domain logic. It coordinates ports and stages only. Mutable execution data lives in `RunState`, not inside stages.

```go
type RunState struct {
    RunID       string
    Input       RunRequest
    Iteration   int
    Messages    MessageBuffer
    Prompt      *CompiledPrompt
    ToolCalls   []ToolCall
    Observations []Observation
    Final       *RunResult
    Usage       Usage
    Metadata    map[string]any
}
```

## 5. ReAct Pipeline

Default stages:

```text
ContextStage  -> load request, session, memory, runtime metadata
PromptStage   -> compile system/tools/skills/history/user blocks
ThinkStage    -> call LLM and parse thought/action/answer
PolicyStage   -> authorize tool calls and enforce budgets
ActStage      -> execute tools
ObserveStage  -> append observations and self-correction hints
CompactStage  -> summarize or prune when token budget is exceeded
FinalizeStage -> emit final response, audit, persist session
```

The loop continues while the model emits tool calls and stops when it emits a final answer, reaches max iterations, exceeds budget, or receives a hard policy failure.

## 6. Business Integration Interfaces

Business projects should implement small, cohesive providers instead of one large god interface.

```go
type SystemPromptProvider interface {
    SystemPrompt(ctx context.Context, req RunRequest) ([]prompt.Block, error)
}

type SkillProvider interface {
    Skills(ctx context.Context, req RunRequest) ([]Skill, error)
}

type ToolProvider interface {
    Tools(ctx context.Context, req RunRequest) ([]tools.Tool, error)
}

type Module interface {
    SystemPromptProvider
    SkillProvider
    ToolProvider
}
```

This keeps the framework reusable. A medical project, finance project, or coding assistant project can provide different modules without modifying the Agent core.

## 7. Prompt Design

Prompts must be compiled from blocks, not concatenated as raw strings.

```go
type Block struct {
    Name      string
    Priority  int
    Cacheable bool
    Content   string
}

type Mode string

const (
    ModeFull    Mode = "full"
    ModeTask    Mode = "task"
    ModeMinimal Mode = "minimal"
    ModeNone    Mode = "none"
)
```

Stable content such as identity, tool schema, and skills summary should be cacheable. Dynamic content such as user input, memory snippets, and observations should not be cacheable. The compiler should support provider-specific renderers such as JSON tool calls, XML tags, or native function calling.

## 8. Tool Design

Tools must expose explicit metadata. The framework should not infer security by tool name.

```go
type Tool interface {
    Spec() Spec
    Execute(ctx context.Context, input json.RawMessage, env Env) (*Result, error)
}

type Spec struct {
    Name        string
    Description string
    Schema      json.RawMessage
    Permission  Permission
    Capability  Capability
    ReadOnly    bool
    Timeout     time.Duration
    RateLimit   *RateLimit
}

type Result struct {
    ForLLM  string
    ForUser string
    Silent  bool
    IsError bool
    Async   bool
    Media   []Media
    Usage   *Usage
}
```

`ForLLM` is the observation sent back into the ReAct loop. `ForUser` is user-facing content. This separation is required for auditability, privacy filtering, and non-text outputs.

## 9. Tool Execution Model

Tool execution should support safe parallelism:

```text
Phase 1: execute raw tool I/O in parallel without mutating RunState
Phase 2: merge results into RunState sequentially by original tool-call index
```

This provides concurrency without nondeterministic memory/message mutation.

## 10. Middleware And Hooks

Middleware is for the tool call path:

```go
type ToolMiddleware func(ToolHandler) ToolHandler
```

Recommended default order:

```text
schema validation
permission check
budget check
rate limit
timeout
input sanitizer
tool execution
output masking
audit log
```

Hooks are for pipeline observability and orchestration:

```go
type Hook interface {
    BeforeStage(ctx context.Context, s Stage, state *RunState) error
    AfterStage(ctx context.Context, s Stage, state *RunState, err error) error
}
```

Do not mix hooks and tool middleware. They solve different extension problems.

## 11. Policy Model

Policy should be explicit and layered:

```text
global policy
provider policy
agent policy
request policy
tool metadata
runtime override
```

The default policy should deny dangerous or mutating tools unless explicitly allowed. The policy engine should produce structured denials so the Agent can either stop or ask for correction/approval.

## 12. Memory Model

Memory should be a port, not a built-in database dependency.

```go
type MemoryProvider interface {
    Load(ctx context.Context, req MemoryQuery) ([]MemoryItem, error)
    Save(ctx context.Context, item MemoryItem) error
    Summarize(ctx context.Context, req SummaryRequest) (*Summary, error)
}
```

Recommended layers:

```text
working memory  -> current messages and observations
episodic memory -> session summaries
semantic memory -> vector or graph retrieval
```

The core should only know when to ask memory for context and when to flush summaries. It should not know whether the backend is in-memory, PostgreSQL, pgvector, Redis, or a knowledge graph.

## 13. Error Handling And Self-Correction

Tool argument parse errors, schema errors, and recoverable tool errors should be converted into structured observations:

```text
Tool call failed: invalid argument `patient_id`.
Expected: non-empty string.
Please correct the tool arguments and try again.
```

The framework should retry inside the ReAct loop up to configured limits. Hard policy failures, budget exhaustion, and unsafe operations should abort or require approval.

## 14. Testing Strategy

The framework needs deterministic tests:

- Mock LLM with scripted responses.
- Mock tools with fixed outputs and injected failures.
- Golden prompt tests for each prompt mode.
- Tool schema and argument parsing tests.
- Policy allow/deny tests.
- ReAct loop tests for final answer, tool call, self-correction, max iteration, budget exceeded.
- Parallel tool execution test that proves raw execution can be concurrent while state merge is deterministic.

## 15. First Implementation Slice

The first version should build only the reusable core:

```text
agentcore pipeline
ports interfaces
prompt blocks and compiler
tool registry and sequential executor
policy engine
mock llm
unit tests
```

Do not implement PostgreSQL, vector memory, HTTP/SSE, or multi-agent orchestration in the first slice. Add them as extensions after the core loop is stable.
