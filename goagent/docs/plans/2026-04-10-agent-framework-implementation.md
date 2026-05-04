# Go Agent Framework Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the first production-quality slice of a Go Agent framework with a small ReAct core, typed extension ports, prompt compilation, policy-aware tools, deterministic tests, and a clean path for business projects to plug in domain prompts, tools, and skills.

**Architecture:** Implement a small `agentcore` package that owns `Agent`, `RunState`, and the stage pipeline. Define all external dependencies as `ports` interfaces, with default implementations for prompt compilation, tool registry, policy checks, and mock LLM testing. Keep storage, HTTP/SSE, vector memory, and provider-specific clients out of the first slice.

**Tech Stack:** Go, standard library `context`, `encoding/json`, `testing`; optional `go test ./...`; no external dependencies in the first slice unless a later task explicitly justifies one.

---

### Task 1: Initialize Go Module And Baseline Layout

**Files:**
- Create: `go.mod`
- Create: `agentcore/doc.go`
- Create: `ports/doc.go`
- Create: `prompt/doc.go`
- Create: `tools/doc.go`
- Create: `policy/doc.go`
- Create: `memory/doc.go`

**Step 1: Initialize module**

Run:

```bash
go mod init github.com/yourorg/goagent
```

Expected: `go.mod` exists with module path.

**Step 2: Create package docs**

Create minimal package comments for each package. Example:

```go
// Package agentcore contains the ReAct execution engine and stage pipeline.
package agentcore
```

**Step 3: Verify**

Run:

```bash
go test ./...
```

Expected: packages compile with no tests.

**Step 4: Commit**

```bash
git add go.mod agentcore ports prompt tools policy memory
git commit -m "chore: initialize agent framework module"
```

Skip commit if the workspace is not a git repository.

### Task 2: Define Core Request, Response, And RunState

**Files:**
- Create: `agentcore/request.go`
- Create: `agentcore/run_state.go`
- Test: `agentcore/run_state_test.go`

**Step 1: Write failing test**

Test that `NewRunState` copies request fields and initializes non-nil buffers/maps.

```go
func TestNewRunStateInitializesState(t *testing.T) {
    req := RunRequest{Input: "hello", UserID: "u1"}
    state := NewRunState("run_1", req)

    if state.RunID != "run_1" {
        t.Fatalf("RunID = %q", state.RunID)
    }
    if state.Input.Input != "hello" {
        t.Fatalf("input = %q", state.Input.Input)
    }
    if state.Metadata == nil {
        t.Fatal("Metadata is nil")
    }
}
```

**Step 2: Run test to verify it fails**

Run:

```bash
go test ./agentcore -run TestNewRunStateInitializesState -v
```

Expected: FAIL because types do not exist.

**Step 3: Implement minimal types**

Define:

```go
type RunRequest struct {
    Input     string
    UserID    string
    SessionID string
    Metadata  map[string]any
}

type RunResult struct {
    Content string
    Usage   Usage
}

type Usage struct {
    InputTokens  int
    OutputTokens int
}

type RunState struct {
    RunID     string
    Input     RunRequest
    Iteration int
    Messages  []Message
    Final     *RunResult
    Usage     Usage
    Metadata  map[string]any
}
```

**Step 4: Verify**

Run:

```bash
go test ./...
```

Expected: PASS.

### Task 3: Define Stage Pipeline

**Files:**
- Create: `agentcore/stage.go`
- Create: `agentcore/pipeline.go`
- Test: `agentcore/pipeline_test.go`

**Step 1: Write failing test**

Test that stages run in order and stop on `StageBreak`.

**Step 2: Implement stage contracts**

Define:

```go
type StageResult int

const (
    StageContinue StageResult = iota
    StageBreak
    StageAbort
)

type Stage interface {
    Name() string
    Run(ctx context.Context, state *RunState) (StageResult, error)
}
```

Implement:

```go
type Pipeline struct {
    stages []Stage
}

func NewPipeline(stages ...Stage) *Pipeline
func (p *Pipeline) Run(ctx context.Context, state *RunState) (StageResult, error)
```

**Step 3: Verify**

Run:

```bash
go test ./agentcore -run Pipeline -v
```

Expected: PASS.

### Task 4: Define LLM And Prompt Ports

**Files:**
- Create: `ports/llm.go`
- Create: `ports/prompt.go`
- Create: `prompt/blocks.go`
- Create: `prompt/compiler.go`
- Test: `prompt/compiler_test.go`

**Step 1: Write failing prompt compiler test**

Test that cacheable blocks are emitted before dynamic blocks and lower priority sorts later.

**Step 2: Implement ports**

Define:

```go
type LLMClient interface {
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

type PromptCompiler interface {
    Compile(ctx context.Context, blocks []prompt.Block) (*prompt.Compiled, error)
}
```

**Step 3: Implement prompt blocks**

Define `Block`, `Mode`, and a deterministic compiler that sorts by priority and name.

**Step 4: Verify**

Run:

```bash
go test ./prompt ./ports
```

Expected: PASS.

### Task 5: Define Tool Spec, Result, Registry, And Sequential Executor

**Files:**
- Create: `tools/spec.go`
- Create: `tools/result.go`
- Create: `tools/tool.go`
- Create: `tools/registry.go`
- Create: `tools/executor.go`
- Test: `tools/registry_test.go`
- Test: `tools/executor_test.go`

**Step 1: Write failing registry test**

Test that registered tool specs are returned in deterministic name order.

**Step 2: Write failing executor test**

Test that a tool receives raw JSON input and returns separate `ForLLM` and `ForUser`.

**Step 3: Implement types**

Define:

```go
type Tool interface {
    Spec() Spec
    Execute(ctx context.Context, input json.RawMessage, env Env) (*Result, error)
}

type Result struct {
    ForLLM  string
    ForUser string
    Silent  bool
    IsError bool
}
```

**Step 4: Verify**

Run:

```bash
go test ./tools -v
```

Expected: PASS.

### Task 6: Add Tool Middleware

**Files:**
- Create: `tools/middleware.go`
- Create: `tools/schema.go`
- Test: `tools/middleware_test.go`

**Step 1: Write failing test**

Test middleware order: schema validation runs before execution and output masking runs after execution.

**Step 2: Implement middleware chain**

Define:

```go
type Handler func(ctx context.Context, input json.RawMessage, env Env) (*Result, error)
type Middleware func(Handler) Handler
```

Implement `Chain(m ...Middleware) Middleware`.

**Step 3: Verify**

Run:

```bash
go test ./tools -run Middleware -v
```

Expected: PASS.

### Task 7: Add Policy Engine

**Files:**
- Create: `policy/permission.go`
- Create: `policy/engine.go`
- Create: `ports/policy.go`
- Test: `policy/engine_test.go`

**Step 1: Write failing allow/deny tests**

Test read-only allowed by default and mutating denied by default unless request policy allows it.

**Step 2: Implement policy**

Define:

```go
type Permission string

const (
    PermissionRead  Permission = "read"
    PermissionWrite Permission = "write"
    PermissionExec  Permission = "exec"
)
```

Expose a `Decision` with `Allowed bool` and `Reason string`.

**Step 3: Verify**

Run:

```bash
go test ./policy ./ports
```

Expected: PASS.

### Task 8: Implement ReAct Stages

**Files:**
- Create: `agentcore/context_stage.go`
- Create: `agentcore/prompt_stage.go`
- Create: `agentcore/think_stage.go`
- Create: `agentcore/policy_stage.go`
- Create: `agentcore/act_stage.go`
- Create: `agentcore/observe_stage.go`
- Create: `agentcore/finalize_stage.go`
- Test: `agentcore/react_test.go`

**Step 1: Write failing final-answer test**

Mock LLM returns final content. Assert pipeline stops with final result.

**Step 2: Write failing tool-call test**

Mock LLM returns a tool call, tool returns observation, next LLM response returns final answer.

**Step 3: Implement minimal stages**

Keep each stage focused:

- `PromptStage` compiles prompt blocks.
- `ThinkStage` calls LLM and parses response.
- `PolicyStage` checks tool permissions.
- `ActStage` executes tools.
- `ObserveStage` appends observations.
- `FinalizeStage` builds `RunResult`.

**Step 4: Verify**

Run:

```bash
go test ./agentcore -run ReAct -v
```

Expected: PASS.

### Task 9: Add Self-Correction For Recoverable Tool Errors

**Files:**
- Modify: `agentcore/observe_stage.go`
- Modify: `agentcore/react_test.go`

**Step 1: Write failing test**

Tool returns schema error. Assert the next LLM call receives an observation telling it to correct arguments.

**Step 2: Implement recoverable observation**

Convert recoverable tool errors into structured observations. Hard policy failures should still abort.

**Step 3: Verify**

Run:

```bash
go test ./agentcore -run SelfCorrection -v
```

Expected: PASS.

### Task 10: Add Deterministic Parallel Tool Execution

**Files:**
- Modify: `tools/executor.go`
- Modify: `tools/executor_test.go`

**Step 1: Write failing order test**

Create two tools that finish in reverse order. Assert returned results preserve original call index.

**Step 2: Implement two-phase execution**

Phase 1 executes raw tool I/O in goroutines without mutating `RunState`. Phase 2 returns ordered results to `ActStage` for sequential state merge.

**Step 3: Verify**

Run:

```bash
go test ./tools -run Parallel -race -v
```

Expected: PASS.

### Task 11: Add Memory Port And Window Memory

**Files:**
- Create: `ports/memory.go`
- Create: `memory/provider.go`
- Create: `memory/window.go`
- Test: `memory/window_test.go`

**Step 1: Write failing bounded-memory test**

Append more messages than the configured limit and assert older messages are removed.

**Step 2: Implement memory port**

Define `MemoryProvider` with `Load`, `Save`, and `Summarize`.

**Step 3: Verify**

Run:

```bash
go test ./memory ./ports
```

Expected: PASS.

### Task 12: Add Public Agent Constructor

**Files:**
- Create: `agentcore/agent.go`
- Test: `agentcore/agent_test.go`

**Step 1: Write failing constructor test**

Assert `NewAgent` wires default stages and returns error if required ports are missing.

**Step 2: Implement options**

Define:

```go
type Option func(*Agent)

func WithLLM(llm ports.LLMClient) Option
func WithPromptCompiler(c ports.PromptCompiler) Option
func WithToolRegistry(r ports.ToolRegistry) Option
```

**Step 3: Verify**

Run:

```bash
go test ./agentcore -run Agent -v
```

Expected: PASS.

### Task 13: Add Minimal Example

**Files:**
- Create: `examples/basic/main.go`
- Create: `examples/basic/README.md`

**Step 1: Write example**

Example should define:

- mock LLM
- one read-only tool
- prompt blocks
- agent run

**Step 2: Verify**

Run:

```bash
go run ./examples/basic
```

Expected: prints final answer after one tool observation.

### Task 14: Final Verification

**Files:**
- Modify: `README.md`

**Step 1: Document core concepts**

Add sections:

- What this framework is
- What the core owns
- How to implement a module
- How ReAct stages work
- How tool results are separated

**Step 2: Run all checks**

Run:

```bash
go test ./...
go test -race ./...
go run ./examples/basic
```

Expected: all pass.

**Step 3: Commit**

```bash
git add .
git commit -m "feat: add initial agent framework core"
```

Skip commit if the workspace is not a git repository.

## Execution Recommendation

Use the plan sequentially. Do not implement memory backends, real LLM providers, HTTP/SSE transport, or multi-agent orchestration until Tasks 1-14 are complete and stable. The first milestone is a small but correct ReAct library core with deterministic tests.
