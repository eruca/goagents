# Run Control And Tool Safety Design

> Date: 2026-04-24
> Goal: Strengthen `goagent` as a reusable runtime for LLM-operated tools without turning the core into a domain product.

## Context

`goagent` already has the right high-level boundary: the core owns the ReAct
loop, stage pipeline, prompt assembly, tool execution, policy gate, memory,
events, budgets, and context projection. Domain systems own research workflows,
databases, durable audit logs, approval UX, retrieval indexes, and concrete
business tools.

The remaining gap is not domain knowledge. The gap is that host applications
need stronger generic contracts when a model is allowed to operate external
capabilities. A single natural-language request can cause tool calls, and the
runtime should make the execution boundary explicit, inspectable, and hard to
misuse.

## Non-Goals

- Do not add research-specific types such as `ResearchIntent`,
  `CohortDefinition`, or `AnalysisPlan` to `agentcore`.
- Do not add RBAC, OPA, human approval workflows, HTTP/SSE transport, durable
  audit storage, PostgreSQL, queues, or multi-agent orchestration to core.
- Do not make `agentcore` depend on sibling modules such as `contextkit` or
  `ocrs`.
- Do not expose broad shell, filesystem, or database execution as framework
  defaults.

## Design Principles

1. Core contracts should describe execution risk, not domain meaning.
2. Host applications should be able to deny, defer, or audit a model-requested
   action before the tool body runs.
3. Tool inputs should be validated locally, not only described to the model.
4. Tool outputs should support compact model observations and stable host
   references.
5. Parallel execution should be opt-in for operations that are safe to reorder.

## Recommended Changes

### 1. Make Request-Scoped Run Control Public

Add explicit run control fields to `RunRequest` instead of requiring hosts to
mutate advanced `RunState` or encode policy in free-form metadata.

Proposed shape:

```go
type RunRequest struct {
	RunID     RunID
	Input     string
	UserID    string
	SessionID string
	Metadata  map[string]any

	AllowedPermissions []policy.Permission
	PolicyContext      ports.PolicyContext
}

// Defined in ports so policy engines can use it without importing agentcore.
type PolicyContext struct {
	TenantID   string
	RequestID string
	TraceID   string
	Labels    map[string]string
}
```

`AllowedPermissions` remains a coarse execution override. `PolicyContext` gives
host policy engines stable, typed context without forcing a full authorization
system into core.

### 2. Pass Tool Input Context To Policy

Extend `PolicyRequest` so policy can inspect the concrete action, not only the
tool permission class.

Proposed shape:

```go
type PolicyRequest struct {
	RunID      string
	UserID     string
	SessionID  string
	Tool       string
	Permission Permission
	Input      json.RawMessage
	Allowed    []Permission
	Context    ports.PolicyContext
	Metadata   map[string]any
}
```

The default `policy.Engine` can keep the current deterministic behavior:
explicit `read` is allowed, `write` and `exec` require request-scoped permission,
and empty or unknown permissions are denied. Host engines can use the richer
request when they need tenant, user, input, or label checks.

### 3. Clarify Local Schema Validation

`ToolSpec.Schema.JSONSchema` is model-facing today, while local validation only
runs when `ToolSchema.Validate` is set. This is easy to misunderstand.

Pick one of two options:

- Preferred: add built-in JSON Schema validation for `JSONSchema`, with
  `Validate` as an extra host hook.
- Minimal: document clearly that `JSONSchema` is provider-facing and local
  enforcement requires `Validate`.

The preferred option is safer for host applications because the schema shown to
the model becomes the same baseline enforced before tool execution.

### 4. Add Tool Execution Semantics

The executor currently runs all tool calls in parallel. That is fine for
read-only, idempotent tools, but unsafe for ordered writes or tools with shared
side effects.

Add an execution hint to `ToolSpec`:

```go
type ExecutionMode string

const (
	ExecutionModeAuto       ExecutionMode = ""
	ExecutionModeParallel   ExecutionMode = "parallel"
	ExecutionModeSequential ExecutionMode = "sequential"
	ExecutionModeExclusive  ExecutionMode = "exclusive"
)
```

Initial behavior can be conservative:

- read tools without an explicit mode may run in parallel;
- write and exec tools run sequentially unless explicitly marked parallel-safe;
- exclusive tools run alone and preserve model call order.

### 5. Add Stable Tool Result References

Keep `ForLLM` and `ForUser`, but let tools return stable host references for
large or auditable outputs.

Proposed shape:

```go
type ToolResult struct {
	ForLLM  string
	ForUser string
	Silent  bool
	IsError bool

	Ref      string
	Metadata map[string]any
}
```

`Ref` can point to a host artifact, stored query result, OCR output, generated
file, or audit record. Core should not interpret it. It should be copied into
events and observations only in bounded form.

## Execution Priority

### P0: Request-Scoped Run Control

This is the most important change because the public API currently does not
give hosts a clean way to allow mutating actions per run. Add
`AllowedPermissions` to `RunRequest`, thread it into `RunState`, and update
policy tests and docs.

### P1: PolicyRequest Context

After `AllowedPermissions` is public, pass run/user/session/input context into
`PolicyRequest`. This unlocks real host policy engines while keeping the default
engine simple.

### P2: Local Schema Validation

Make `JSONSchema` locally enforceable or document the current split very
explicitly. This reduces the risk that a tool accepts inputs the model was never
supposed to produce.

### P3: Tool Execution Semantics

Prevent accidental parallel side effects. Keep the first implementation small:
parallel reads, sequential writes/execs.

### P4: Tool Result References

Add `Ref` and `Metadata` after the execution gate is stronger. This improves
auditability and large-result handling but is less urgent than preventing unsafe
execution.

## Test Strategy

- Add contract tests showing `RunRequest.AllowedPermissions` allows a write tool
  through `Agent.Run`.
- Add tests that `PolicyRequest` receives run ID, user ID, session ID, metadata,
  and raw tool input.
- Add schema tests where invalid JSON Schema input is denied before the tool body
  runs.
- Add executor tests proving write tools preserve order and read tools can still
  run in parallel.
- Add result-reference tests proving refs are preserved in tool executions and
  bounded event metadata.

## Success Criteria

- Host code can configure per-run execution permissions without advanced
  `RunState` access.
- Host policy engines can make decisions from concrete tool inputs and request
  context.
- Model-facing tool schemas have a clear local enforcement story.
- Side-effecting tools do not run concurrently by accident.
- Large or auditable tool outputs can be referenced without entering model
  context.
