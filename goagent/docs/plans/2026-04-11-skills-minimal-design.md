# Skills Minimal Design

> Date: 2026-04-11
> Goal: Add the smallest useful skill integration path for the embedded ReAct Agent library.

## Positioning

Skills are model-facing instructions, not executable tools. A skill tells the model how to approach a domain workflow, when to use tools, or which constraints to follow. A tool performs an action. Policy decides whether an action is allowed.

This keeps skills safe and portable. The first implementation must not load skills from disk, install dependencies, search a marketplace, bind tools automatically, or execute skill content.

## API Shape

Add the skill API to `agentcore` rather than `ports` for now:

```go
type Skill struct {
    Name        string
    Description string
    Content     string
    Priority    int
    Cacheable   bool
}

type SkillProvider interface {
    Skills(ctx context.Context, req RunRequest) ([]Skill, error)
}
```

`SkillProvider` accepts `RunRequest`, which currently lives in `agentcore`. Putting the interface in `agentcore` avoids forcing `ports` to depend on `agentcore` and keeps the dependency boundary intact.

Agent wiring:

```go
func WithSkillProvider(provider SkillProvider) Option
```

## Runtime Flow

Add `SkillStage` before `PromptStage`:

```text
MemoryLoadStage
ContextStage
SkillStage
PromptStage
ThinkStage
PolicyStage
ActStage
ObserveStage
FinalizeStage
memory save
```

`SkillStage` loads skills once per run and converts them to prompt blocks:

```go
prompt.Block{
    Name:     "skill:" + skill.Name,
    Mode:     prompt.ModeCacheable or prompt.ModeDynamic,
    Priority: skill.Priority,
    Content:  rendered skill content,
}
```

The rendered content should include name, description when present, and content. Deterministic ordering remains the prompt compiler's responsibility: cacheable blocks first, then priority, then name.

## Error Handling

If `SkillProvider.Skills` returns an error, the run aborts and returns that error. If no provider is configured, the stage is a no-op. If a skill has empty content, it is skipped.

## Documentation And Example

Add `examples/skills` with:

- a mock LLM
- one read-only tool
- a skill provider that returns a tool-use guide skill
- a run that prints the final answer

Update `README.md` to explain:

- skills are model-facing instructions
- tools are executable actions
- skills can be wired with `WithSkillProvider`

## Out Of Scope

- Filesystem skill loading
- Markdown parsing
- Skill marketplace or install flow
- Skill dependency discovery
- Skill search or embedding retrieval
- Automatic skill selection
- Skill-tool binding
- Provider-specific prompt rendering
