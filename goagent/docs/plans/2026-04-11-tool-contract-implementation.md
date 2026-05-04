# Tool Contract Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make the tool extension contract explicit through focused tests, a runnable example, and concise documentation.

**Architecture:** Keep the existing tool API. Add tests that pin current executor and observe-stage behavior, add `examples/tools` as the canonical tool-authoring example, and update README/package docs to explain the contract. Do not introduce provider, transport, storage, marketplace, retry, rate-limit, or orchestration features.

**Tech Stack:** Go standard library, existing `agentcore`, `ports`, `policy`, `prompt`, and `tools` packages, `go test`, `go run`, `make verify`.

---

### Task 1: Pin Tool Observation Semantics

**Files:**
- Modify: `agentcore/react_test.go`

**Step 1: Write the failing test**

Add `TestSilentToolResultDoesNotAppendObservation`:

```go
func TestSilentToolResultDoesNotAppendObservation(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "audit", Input: json.RawMessage(`{}`)}}},
		{Content: "done"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "audit", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "internal audit trail", Silent: true}, nil
		},
	})
	state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
	runner := NewReActRunner(ReActConfig{
		LLM:            llm,
		PromptCompiler: prompt.NewCompiler(),
		ToolRegistry:   registry,
		PolicyEngine:   policy.NewEngine(),
		MaxIterations:  3,
	})

	_, err := runner.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("LLM requests = %d", len(llm.requests))
	}
	for _, message := range llm.requests[1].Messages {
		if message.Role == "tool" {
			t.Fatalf("unexpected tool observation: %#v", message)
		}
	}
}
```

This test may already pass because the implementation exists. That is acceptable for this task because it pins the public contract before documentation and examples depend on it.

**Step 2: Run the focused tests**

Run:

```bash
go test ./agentcore -run 'Test(SelfCorrectionObservesRecoverableToolError|SilentToolResultDoesNotAppendObservation)' -v
```

Expected: PASS.

**Step 3: Commit**

```bash
git add agentcore/react_test.go
git commit -m "test: pin tool observation contract"
```

### Task 2: Add Tools Example

**Files:**
- Create: `examples/tools/main.go`
- Create: `examples/tools/README.md`

**Step 1: Run the failing command**

Run:

```bash
go run ./examples/tools
```

Expected: FAIL because `examples/tools` does not exist.

**Step 2: Implement the minimal example**

Create `examples/tools/main.go` with one mock LLM flow and two tools:

- `accountLookupTool` uses `Spec.Schema` to require an `account` string, returns redacted `ForLLM` and richer `ForUser`.
- `auditTool` returns a `Silent` result to show host-visible side effects do not have to enter model context.
- The mock LLM first calls `account_lookup`, then `audit`, then returns `Final answer: the account is active.`

Use only standard library JSON validation:

```go
func requireAccount(input json.RawMessage) error {
	var payload struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return err
	}
	if payload.Account == "" {
		return fmt.Errorf("account is required")
	}
	return nil
}
```

Print the final answer and user-visible tool outputs recorded by the tools. Do not print raw prompt text or raw tool input.

Create `examples/tools/README.md` explaining:

- `Spec.Permission`
- `Spec.Schema`
- `Spec.Timeout`
- `Result.ForLLM`
- `Result.ForUser`
- `Result.Silent`
- `Result.IsError`

**Step 3: Run the example**

Run:

```bash
go run ./examples/tools
```

Expected: PASS with output containing:

- `Final answer: the account is active.`
- `User-visible tool output: active`

**Step 4: Commit**

```bash
git add examples/tools/main.go examples/tools/README.md
git commit -m "docs: add tools example"
```

### Task 3: Document Tool Contract

**Files:**
- Modify: `README.md`
- Modify: `tools/doc.go`

**Step 1: Update README**

Add a `Writing Tools` section after `Tool Result Separation` or near the learning path:

```markdown
## Writing Tools

A tool is a host-owned typed action. Keep each tool focused on one operation and describe its boundary in `Spec`.

- `Permission` declares whether the action is read, write, or exec so policy can approve it.
- `Schema` validates model-supplied JSON before the tool body runs.
- `Timeout` bounds tool and middleware execution.
- Return `ForLLM` for model observations and `ForUser` for host/UI output.
- Use `Silent` when a successful tool result should not be appended to model context.
- Use `IsError` for recoverable domain errors the model can correct; return a Go error for executor failures that should abort the run.

See `examples/tools` for a minimal tool-authoring example.
```

**Step 2: Update package docs**

Update `tools/doc.go` so `go doc` explains the same contract in package-level language.

**Step 3: Run docs-neutral verification**

Run:

```bash
go test ./tools ./agentcore
```

Expected: PASS.

**Step 4: Commit**

```bash
git add README.md tools/doc.go
git commit -m "docs: document tool contract"
```

### Task 4: Final Verification

**Files:**
- No code changes expected.

**Step 1: Run full verification**

Run:

```bash
make verify
go run ./examples/tools
```

Expected: PASS.

**Step 2: Confirm status**

Run:

```bash
git status --short
```

Expected: no output.
