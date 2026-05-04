# Module Minimal Design

> Date: 2026-04-11
> Goal: Add the smallest useful business module integration layer while preserving the embedded-library core boundary.

## Positioning

The framework should let a business project provide domain prompts, skills, and tools as one cohesive module. The Agent core should coordinate those providers without knowing domain logic.

This is not a plugin loader or platform lifecycle system. The first module integration is in-process, explicit, and request-scoped.

## Interfaces

Add these interfaces in `agentcore`:

```go
type SystemPromptProvider interface {
    SystemPrompt(ctx context.Context, req RunRequest) ([]prompt.Block, error)
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

They live in `agentcore` because they consume `RunRequest`. This avoids making `ports` depend on `agentcore`.

## Agent Options

Add:

```go
func WithSystemPromptProvider(provider SystemPromptProvider) Option
func WithToolProvider(provider ToolProvider) Option
func WithModule(module Module) Option
```

`WithModule` wires the same object as system prompt provider, skill provider, and tool provider.

## Runtime Flow

Add stages before prompt compilation and tool use:

```text
MemoryLoadStage
ContextStage
SystemPromptStage
SkillStage
ToolProviderStage
PromptStage
ThinkStage
PolicyStage
ActStage
ObserveStage
FinalizeStage
memory save
```

`SystemPromptStage` appends provider blocks to `RunState.PromptBlocks`.

`ToolProviderStage` loads request-scoped tools and registers them into a per-run registry.

## Per-Run Tool Registry

Request-scoped tools must not pollute the Agent's base registry. Each run should get a fresh registry:

```text
base registry specs/tools
        +
tool provider tools for this request
        =
state.ToolRegistry
```

`ThinkStage`, `PolicyStage`, and `ActStage` should use the run registry.

The first implementation can clone only `*tools.Registry` base registries. If callers provide a custom `ports.ToolRegistry`, it remains the run registry and `ToolProvider` registration requires a concrete `*tools.Registry` created for the run. This keeps the implementation small and avoids adding mutation methods to `ports.ToolRegistry`.

## Error Handling

Provider errors abort the run. Missing providers are no-ops.

## Out Of Scope

- Filesystem module loading
- Module lifecycle
- Dynamic module routing
- Provider SDKs
- HTTP/SSE
- DB or vector storage
- Plugin marketplace
- Tool dependency installation
