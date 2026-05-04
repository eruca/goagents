# Prompt Contract Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make prompt ordering and Agent prompt assembly explicit through tests, a runnable example, and concise documentation.

**Architecture:** Keep the existing prompt API and compiler. Add tests that pin deterministic block ordering and Agent message assembly, add `examples/prompt` to show compact message order, and document the prompt boundary. Do not add templates, token budgeting, compaction, prompt versioning, provider-specific adapters, streaming, or cache integrations.

**Tech Stack:** Go standard library, existing `agentcore`, `prompt`, `ports`, `policy`, and `tools` packages, `go test`, `go run`, `make verify`.

---

### Task 1: Pin Prompt Compiler Contract

**Files:**
- Modify: `prompt/compiler_test.go`

**Step 1: Write tests**

Add `TestCompilerKeepsEmptyBlocksButOmitsEmptyContent`:

```go
func TestCompilerKeepsEmptyBlocksButOmitsEmptyContent(t *testing.T) {
	compiler := NewCompiler()
	compiled, err := compiler.Compile(context.Background(), []Block{
		{Name: "empty", Mode: ModeCacheable, Priority: 1},
		{Name: "filled", Mode: ModeCacheable, Priority: 2, Content: "filled content"},
	})
	if err != nil {
		t.Fatalf("Compile returned error: %v", err)
	}
	if len(compiled.Blocks) != 2 {
		t.Fatalf("blocks = %#v", compiled.Blocks)
	}
	if compiled.Content != "filled content" {
		t.Fatalf("content = %q", compiled.Content)
	}
}
```

Add `TestCompilerJoinsContentWithNewlines`:

```go
func TestCompilerJoinsContentWithNewlines(t *testing.T) {
	compiler := NewCompiler()
	compiled, err := compiler.Compile(context.Background(), []Block{
		{Name: "b", Mode: ModeDynamic, Priority: 2, Content: "second"},
		{Name: "a", Mode: ModeCacheable, Priority: 1, Content: "first"},
	})
	if err != nil {
		t.Fatalf("Compile returned error: %v", err)
	}
	if compiled.Content != "first\nsecond" {
		t.Fatalf("content = %q", compiled.Content)
	}
}
```

These tests may already pass. That is acceptable because they pin public prompt compiler behavior.

**Step 2: Run focused tests**

Run:

```bash
go test ./prompt -run TestCompiler -v
```

Expected: PASS.

**Step 3: Commit**

```bash
git add prompt/compiler_test.go
git commit -m "test: pin prompt compiler contract"
```

### Task 2: Pin Agent Prompt Assembly

**Files:**
- Modify: `agentcore/agent_test.go`
- Modify: `agentcore/react_test.go`

**Step 1: Write tests**

Add `TestAgentCombinesStaticSystemAndSkillPromptBlocks` to `agentcore/agent_test.go`:

```go
func TestAgentCombinesStaticSystemAndSkillPromptBlocks(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithPromptBlocks([]prompt.Block{
			{Name: "static", Mode: prompt.ModeCacheable, Priority: 1, Content: "Static instruction."},
		}),
		WithSystemPromptProvider(staticSystemPromptProvider{
			blocks: []prompt.Block{{Name: "system", Mode: prompt.ModeCacheable, Priority: 2, Content: "System instruction."}},
		}),
		WithSkillProvider(staticSkillProvider{
			skills: []Skill{{Name: "skill", Content: "Skill instruction.", Priority: 3, Cacheable: true}},
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := llm.requests[0].Messages[0].Content
	want := "Static instruction.\nSystem instruction.\nSkill: skill\nSkill instruction."
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}
```

Add `TestReActPromptMessageOrderIncludesSystemMemoryUserAndToolObservation` to `agentcore/react_test.go`:

```go
func TestReActPromptMessageOrderIncludesSystemMemoryUserAndToolObservation(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}}},
		{Content: "done"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "observation"}, nil
		},
	})
	state := NewRunState(NewRunID(), RunRequest{Input: "current"})
	state.Messages = append(state.Messages, Message{Role: "assistant", Content: "remembered"})
	runner := NewReActRunner(ReActConfig{
		LLM:            llm,
		PromptCompiler: prompt.NewCompiler(),
		PromptBlocks:   []prompt.Block{{Name: "system", Mode: prompt.ModeCacheable, Content: "system"}},
		ToolRegistry:   registry,
		PolicyEngine:   policy.NewEngine(),
		MaxIterations:  3,
	})

	_, err := runner.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	first := llm.requests[0].Messages
	if got := []string{first[0].Role, first[1].Role, first[2].Role}; got[0] != "system" || got[1] != "assistant" || got[2] != "user" {
		t.Fatalf("first request roles = %v", got)
	}
	second := llm.requests[1].Messages
	if second[len(second)-1].Role != "tool" || second[len(second)-1].Content != "observation" {
		t.Fatalf("second request messages = %#v", second)
	}
}
```

These tests may already pass because they pin existing behavior.

**Step 2: Run focused tests**

Run:

```bash
go test ./agentcore -run 'TestAgentCombinesStaticSystemAndSkillPromptBlocks|TestReActPromptMessageOrderIncludesSystemMemoryUserAndToolObservation' -v
```

Expected: PASS.

**Step 3: Commit**

```bash
git add agentcore/agent_test.go agentcore/react_test.go
git commit -m "test: pin agent prompt assembly"
```

### Task 3: Add Prompt Example

**Files:**
- Create: `examples/prompt/main.go`
- Create: `examples/prompt/README.md`

**Step 1: Run failing command**

Run:

```bash
go run ./examples/prompt
```

Expected: FAIL because `examples/prompt` does not exist.

**Step 2: Implement example**

Create `examples/prompt/main.go` with:

- a `recordingLLM` that records the first request and returns `Final answer: prompt assembled.`
- static `WithPromptBlocks`
- a small module that provides one system prompt block and one cacheable skill
- after the run, print compact message order:

```text
message[0]=system
message[1]=user
Final answer: prompt assembled.
```

Also print whether the first system message contains the expected short labels:

```text
system contains static=true module=true skill=true
```

Do not print the full prompt content.

Create `examples/prompt/README.md` explaining that prompt blocks are model-facing instructions and that this example prints compact message order only.

**Step 3: Run example**

Run:

```bash
go run ./examples/prompt
```

Expected: PASS with output containing:

- `message[0]=system`
- `message[1]=user`
- `system contains static=true module=true skill=true`
- `Final answer: prompt assembled.`

**Step 4: Commit**

```bash
git add examples/prompt/main.go examples/prompt/README.md
git commit -m "docs: add prompt example"
```

### Task 4: Document Prompt Contract

**Files:**
- Modify: `README.md`
- Modify: `prompt/doc.go`

**Step 1: Update README**

Add a `Prompt Assembly` section:

```markdown
## Prompt Assembly

Prompt blocks are model-facing instructions. They are not tools, memory, policy, or orchestration.

- `ModeCacheable` blocks sort before `ModeDynamic` blocks.
- Lower `Priority` sorts earlier.
- Blocks with the same mode and priority sort by `Name`.
- Empty block content is omitted from compiled text.
- Non-empty block content is joined with newlines.
- The compiled prompt is sent as the first `system` message when non-empty.
- Memory messages load before current user input.
- Tool observations are appended before the next LLM turn unless the tool result is `Silent`.

Do not put secrets or raw sensitive data into prompt blocks unless the host intentionally wants that data in model context.

See `examples/prompt` for a compact prompt assembly example.
```

**Step 2: Update package docs**

Update `prompt/doc.go` with the same deterministic compiler contract in package-level language.

**Step 3: Run docs-neutral verification**

Run:

```bash
go test ./prompt ./agentcore
```

Expected: PASS.

**Step 4: Commit**

```bash
git add README.md prompt/doc.go
git commit -m "docs: document prompt contract"
```

### Task 5: Final Verification

**Files:**
- No code changes expected.

**Step 1: Run full verification**

Run:

```bash
make verify
go run ./examples/prompt
```

Expected: PASS.

**Step 2: Confirm status**

Run:

```bash
git status --short
```

Expected: no output.
