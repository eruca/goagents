# Developer Examples Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a minimal observability example and tighten the example learning path so users can understand how to embed `goagent`.

**Architecture:** Keep examples as standalone `main` packages using mock LLMs and deterministic tools. `examples/events` demonstrates `WithEventSink` without adding logging or tracing dependencies. `examples/module` remains the business-module integration example and README links the examples in increasing complexity.

**Tech Stack:** Go standard library, existing `agentcore`, `ports`, `prompt`, `policy`, and `tools` packages, `go run`, `make verify`.

---

### Task 1: Add Events Example

**Files:**
- Create: `examples/events/main.go`
- Create: `examples/events/README.md`

**Step 1: Write the failing test command**

Run:

```bash
go run ./examples/events
```

Expected: FAIL because `examples/events` does not exist.

**Step 2: Implement the minimal example**

Create `examples/events/main.go` with:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/memory"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

type mockLLM struct {
	responses []*ports.ChatResponse
}

func (m *mockLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

type printSink struct{}

func (s printSink) Emit(ctx context.Context, event agentcore.Event) error {
	fmt.Printf("event=%s stage=%s iteration=%d metadata=%s\n", event.Type, event.Stage, event.Iteration, formatMetadata(event.Metadata))
	return nil
}

func formatMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, metadata[key]))
	}
	return strings.Join(parts, ",")
}

type lookupTool struct{}

func (t lookupTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "lookup",
		Description: "Returns deterministic account information.",
		Permission:  policy.PermissionRead,
	}
}

func (t lookupTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	return &tools.Result{ForLLM: "account status: active", ForUser: "active"}, nil
}

func main() {
	ctx := context.Background()
	registry := tools.NewRegistry()
	registry.Register(lookupTool{})

	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{"account":"demo"}`)}}},
			{Content: "Final answer: the account is active."},
		}}),
		agentcore.WithToolRegistry(registry),
		agentcore.WithMemoryProvider(memory.NewWindowMemory(8)),
		agentcore.WithEventSink(printSink{}),
	)
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(ctx, agentcore.RunRequest{SessionID: "demo-session", Input: "Check the demo account."})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content)
}
```

Create `examples/events/README.md` with a short description and run command.

**Step 3: Run the example**

Run:

```bash
go run ./examples/events
```

Expected: PASS and output includes `event=tool.started`, `event=tool.completed`, `event=memory.saved`, and `Final answer: the account is active.`

**Step 4: Commit**

```bash
git add examples/events/main.go examples/events/README.md
git commit -m "docs: add events example"
```

### Task 2: Tighten Module Example

**Files:**
- Modify: `examples/module/main.go`
- Modify: `examples/module/README.md`

**Step 1: Run the current example**

Run:

```bash
go run ./examples/module
```

Expected: PASS with `Final answer: the account is active.`

**Step 2: Update the example text**

Keep the code behavior unchanged unless clarity requires a small rename. Update the README to explain that a module is host-side glue for system prompts, skills, and request-scoped tools, not a dynamic plugin system.

**Step 3: Run the example**

Run:

```bash
go run ./examples/module
```

Expected: PASS with `Final answer: the account is active.`

**Step 4: Commit**

```bash
git add examples/module/main.go examples/module/README.md
git commit -m "docs: clarify module example"
```

### Task 3: Document Example Learning Path

**Files:**
- Modify: `README.md`

**Step 1: Update docs**

Add a `Learning Path` section near the quickstart:

```markdown
## Learning Path

- `examples/basic` shows the smallest ReAct run with one read-only tool.
- `examples/skills` shows model-facing instructions next to executable tools.
- `examples/module` shows host-side module wiring for prompts, skills, and tools.
- `examples/events` shows runtime observability with `WithEventSink`.
```

**Step 2: Verify**

Run:

```bash
make verify
go run ./examples/module
go run ./examples/events
```

Expected: PASS.

**Step 3: Commit**

```bash
git add README.md
git commit -m "docs: add example learning path"
```
