# Skills Minimal Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a minimal SkillProvider path that turns model-facing skills into prompt blocks during Agent runs.

**Architecture:** Skills live in `agentcore` because they consume `RunRequest` and are part of Agent wiring. A new `SkillStage` runs before `PromptStage`, loads skills once, converts them into prompt blocks, and appends them to `RunState.PromptBlocks`. The prompt compiler keeps deterministic ordering.

**Tech Stack:** Go, standard library, existing `prompt` and `agentcore` packages, TDD with `go test`, full verification with `make verify`.

---

### Task 1: Add Skill Types And Agent Wiring

**Files:**
- Create: `agentcore/skill.go`
- Modify: `agentcore/agent.go`
- Test: `agentcore/agent_test.go`

**Step 1: Write failing test**

Add a test that constructs an Agent with `WithSkillProvider`, runs it, and asserts skill content appears in the LLM request before the user input.

**Step 2: Run test to verify it fails**

Run:

```bash
go test ./agentcore -run TestAgentIncludesSkillsInPrompt -v
```

Expected: FAIL because `WithSkillProvider` and skill types do not exist.

**Step 3: Implement minimal API**

Create `Skill`, `SkillProvider`, and `WithSkillProvider`. Store the provider on `Agent` and pass it through `ReActConfig`.

**Step 4: Run test**

Run:

```bash
go test ./agentcore -run TestAgentIncludesSkillsInPrompt -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/skill.go agentcore/agent.go agentcore/agent_test.go
git commit -m "feat: add skill provider wiring"
```

### Task 2: Add SkillStage Behavior

**Files:**
- Create: `agentcore/skill_stage.go`
- Modify: `agentcore/finalize_stage.go`
- Test: `agentcore/agent_test.go`

**Step 1: Write failing tests**

Add tests for:

- skills are sorted deterministically by prompt compiler priority/name
- skill provider error aborts the run
- missing skill provider is a no-op

**Step 2: Run tests to verify failure**

Run:

```bash
go test ./agentcore -run 'TestAgent(SortsSkillsInPrompt|ReturnsSkillProviderError|RunsWithoutSkillProvider)' -v
```

Expected: FAIL for missing behavior.

**Step 3: Implement SkillStage**

Implement `SkillStage` before `PromptStage`. It should:

- skip if provider is nil
- load once per run using metadata guard
- skip empty-content skills
- convert skills to `prompt.Block`
- use `prompt.ModeCacheable` when `Skill.Cacheable` is true, otherwise `prompt.ModeDynamic`

**Step 4: Run tests**

Run:

```bash
go test ./agentcore -run 'TestAgent.*Skill' -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/skill_stage.go agentcore/finalize_stage.go agentcore/agent_test.go
git commit -m "feat: compile skills into prompts"
```

### Task 3: Add Skills Example And Docs

**Files:**
- Create: `examples/skills/main.go`
- Create: `examples/skills/README.md`
- Modify: `README.md`

**Step 1: Write example**

Create an example with:

- mock LLM
- a read-only tool
- a skill provider returning a tool-use guide
- `agentcore.WithSkillProvider`

**Step 2: Verify example**

Run:

```bash
go run ./examples/skills
```

Expected: prints a final answer.

**Step 3: Update README**

Explain the distinction:

- Skill: model-facing instructions
- Tool: executable action
- Policy: permission decision

**Step 4: Run full verification**

Run:

```bash
make verify
go run ./examples/skills
```

Expected: PASS.

**Step 5: Commit**

```bash
git add README.md examples/skills
git commit -m "docs: add skills example"
```
