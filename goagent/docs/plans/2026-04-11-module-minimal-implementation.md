# Module Minimal Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add minimal SystemPromptProvider, ToolProvider, and Module wiring so business projects can provide prompts, skills, and tools through one cohesive API.

**Architecture:** Provider interfaces live in `agentcore` because they consume `RunRequest`. System prompts and skills become prompt blocks before `PromptStage`. ToolProvider tools are registered into a fresh per-run `tools.Registry` so request-scoped tools do not pollute the Agent base registry.

**Tech Stack:** Go, existing `agentcore`, `prompt`, and `tools` packages, TDD with `go test`, full verification with `make verify`.

---

### Task 1: Add SystemPromptProvider

**Files:**
- Create: `agentcore/module.go`
- Create: `agentcore/system_prompt_stage.go`
- Modify: `agentcore/agent.go`
- Modify: `agentcore/finalize_stage.go`
- Test: `agentcore/agent_test.go`

**Step 1: Write failing test**

Add `TestAgentIncludesSystemPromptProviderBlocks` asserting a block from `WithSystemPromptProvider` appears in the LLM request before user input.

**Step 2: Run test to verify failure**

Run:

```bash
go test ./agentcore -run TestAgentIncludesSystemPromptProviderBlocks -v
```

Expected: FAIL because the API does not exist.

**Step 3: Implement minimal system prompt provider**

Create `SystemPromptProvider`, add `WithSystemPromptProvider`, pass it through `ReActConfig`, and add `SystemPromptStage` before `SkillStage`.

**Step 4: Run test**

Run:

```bash
go test ./agentcore -run TestAgentIncludesSystemPromptProviderBlocks -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/module.go agentcore/system_prompt_stage.go agentcore/agent.go agentcore/finalize_stage.go agentcore/agent_test.go
git commit -m "feat: add system prompt provider"
```

### Task 2: Add ToolProvider With Per-Run Registry

**Files:**
- Modify: `agentcore/module.go`
- Create: `agentcore/tool_provider_stage.go`
- Modify: `agentcore/run_state.go`
- Modify: `agentcore/finalize_stage.go`
- Modify: `tools/registry.go`
- Test: `agentcore/agent_test.go`

**Step 1: Write failing tests**

Add tests:

- `TestAgentUsesToolProviderTools`
- `TestAgentDoesNotPolluteBaseRegistryWithProvidedTools`
- `TestAgentReturnsToolProviderError`

**Step 2: Run tests to verify failure**

Run:

```bash
go test ./agentcore -run 'TestAgent(UsesToolProviderTools|DoesNotPolluteBaseRegistryWithProvidedTools|ReturnsToolProviderError)' -v
```

Expected: FAIL because ToolProvider and per-run registry do not exist.

**Step 3: Implement minimal tool provider**

Add `ToolProvider`, `WithToolProvider`, run registry setup, and `ToolProviderStage`. Add `tools.CloneRegistry` or `(*Registry).Clone` for concrete base registries.

**Step 4: Run tests**

Run:

```bash
go test ./agentcore -run 'TestAgent.*ToolProvider' -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/module.go agentcore/tool_provider_stage.go agentcore/run_state.go agentcore/finalize_stage.go agentcore/agent.go agentcore/agent_test.go tools/registry.go
git commit -m "feat: add tool provider wiring"
```

### Task 3: Add Module Wiring And Docs

**Files:**
- Modify: `agentcore/module.go`
- Modify: `agentcore/agent.go`
- Create: `examples/module/main.go`
- Create: `examples/module/README.md`
- Modify: `README.md`
- Test: `agentcore/agent_test.go`

**Step 1: Write failing test**

Add `TestAgentWithModuleWiresPromptSkillsAndTools`.

**Step 2: Run test to verify failure**

Run:

```bash
go test ./agentcore -run TestAgentWithModuleWiresPromptSkillsAndTools -v
```

Expected: FAIL because `WithModule` does not exist.

**Step 3: Implement Module and example**

Add `Module`, `WithModule`, and an example that provides system prompt, skills, and tools.

**Step 4: Verify**

Run:

```bash
go test ./agentcore -run TestAgentWithModuleWiresPromptSkillsAndTools -v
go run ./examples/module
make verify
```

Expected: PASS.

**Step 5: Commit**

```bash
git add README.md agentcore/module.go agentcore/agent.go agentcore/agent_test.go examples/module
git commit -m "feat: add module wiring"
```
