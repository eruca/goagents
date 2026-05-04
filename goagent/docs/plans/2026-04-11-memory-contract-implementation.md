# Memory Contract Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make the memory extension contract explicit through focused tests, a runnable example, and concise documentation.

**Architecture:** Keep `ports.MemoryProvider` focused on load/save behavior. Add tests for load-once and no-save-on-failure behavior, add `examples/memory` using the built-in in-process `WindowMemory`, and document the session memory contract. Do not add durable storage, vector memory, summarization, compaction, or provider integrations.

**Tech Stack:** Go standard library, existing `agentcore`, `memory`, `ports`, `policy`, `prompt`, and `tools` packages, `go test`, `go run`, `make verify`.

---

### Task 1: Pin Agent Memory Failure And Loop Semantics

**Files:**
- Modify: `agentcore/agent_test.go`

**Step 1: Write tests**

Add `TestAgentLoadsMemoryOnceAcrossToolIterations`:

```go
func TestAgentLoadsMemoryOnceAcrossToolIterations(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}}},
		{Content: "agent answer"},
	}}
	memory := &mockMemoryProvider{loaded: []ports.MemoryMessage{{Role: "assistant", Content: "remembered"}}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "observation"}, nil
		},
	})
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
		WithToolRegistry(registry),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{SessionID: "session_1", Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if memory.loadCalls != 1 {
		t.Fatalf("load calls = %d", memory.loadCalls)
	}
}
```

Add `TestAgentDoesNotSaveMemoryOnMaxIterations`:

```go
func TestAgentDoesNotSaveMemoryOnMaxIterations(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{}}}
	memory := &mockMemoryProvider{}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
		WithMaxIterations(1),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{SessionID: "session_1", Input: "hello"})
	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
	if memory.saveCalls != 0 {
		t.Fatalf("save calls = %d", memory.saveCalls)
	}
}
```

These tests may already pass because the behavior exists. That is acceptable because the task pins the public memory contract.

**Step 2: Run focused tests**

Run:

```bash
go test ./agentcore -run 'TestAgent(LoadsMemoryOnceAcrossToolIterations|DoesNotSaveMemoryOnMaxIterations)' -v
```

Expected: PASS.

**Step 3: Commit**

```bash
git add agentcore/agent_test.go
git commit -m "test: pin agent memory contract"
```

### Task 2: Add Memory Example

**Files:**
- Create: `examples/memory/main.go`
- Create: `examples/memory/README.md`

**Step 1: Run failing command**

Run:

```bash
go run ./examples/memory
```

Expected: FAIL because `examples/memory` does not exist.

**Step 2: Implement example**

Create `examples/memory/main.go` with:

- a shared `memory.NewWindowMemory(8)`
- a `recordingLLM` that records requests and returns two deterministic final answers
- one `Agent` using `WithMemoryProvider`
- two `Agent.Run` calls with `SessionID: "demo-session"`
- output:
  - `First answer: remembered account status.`
  - `Second answer: used remembered context.`
  - `Second run saw messages: 3`

The second run should see messages from the first run plus the new user input. Do not print full memory content.

**Step 3: Run example**

Run:

```bash
go run ./examples/memory
```

Expected: PASS with the three output lines above.

**Step 4: Commit**

```bash
git add examples/memory/main.go examples/memory/README.md
git commit -m "docs: add memory example"
```

### Task 3: Document Memory Contract

**Files:**
- Modify: `README.md`
- Modify: `memory/doc.go`

**Step 1: Update README**

Add a `Session Memory` section:

```markdown
## Session Memory

Memory is enabled only when an Agent has a `MemoryProvider` and the request includes `SessionID`.

- Memory loads before the current user input is appended.
- Memory loads at most once per run, even across multiple ReAct iterations.
- Memory saves only after a successful final answer.
- Memory is not saved when load, policy, tool, or max-iteration errors abort the run.
- Saved messages include prior loaded messages, current user input, non-silent tool observations, and the final assistant answer.

`memory.WindowMemory` is an in-process bounded session store. It is safe for concurrent use, drops older messages past its limit, and does not survive process restart. Summarization, compaction, vector retrieval, and durable storage are extension concerns, not core memory behavior.

See `examples/memory` for a minimal session continuity example.
```

**Step 2: Update package docs**

Update `memory/doc.go` with the same contract in package-level language.

**Step 3: Run docs-neutral verification**

Run:

```bash
go test ./memory ./agentcore
```

Expected: PASS.

**Step 4: Commit**

```bash
git add README.md memory/doc.go
git commit -m "docs: document memory contract"
```

### Task 4: Final Verification

**Files:**
- No code changes expected.

**Step 1: Run full verification**

Run:

```bash
make verify
go run ./examples/memory
```

Expected: PASS.

**Step 2: Confirm status**

Run:

```bash
git status --short
```

Expected: no output.
