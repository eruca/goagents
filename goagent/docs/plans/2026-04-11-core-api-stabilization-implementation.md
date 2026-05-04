# Core API Stabilization Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Clarify the stable core API surface and add classifiable errors for framework-owned abort reasons.

**Architecture:** Keep the current exported runtime types intact, but document them as advanced runtime API. Add sentinel errors for policy denial and missing tools, wrapping existing error messages so hosts can use `errors.Is` without losing readable diagnostics.

**Tech Stack:** Go standard library errors, `agentcore`, `tools`, README/package docs, `make verify`.

---

## Task 1: Add Classifiable Policy And Tool Errors

**Files:**
- Modify: `agentcore/policy_stage.go`
- Modify: `agentcore/policy_stage_test.go`
- Modify: `agentcore/agent_test.go`
- Modify: `tools/registry.go`
- Modify: `tools/registry_test.go`
- Modify: `agentcore/tool_registry.go`

**Step 1: Write failing tests**

Add `errors.Is` assertions:

- `TestPolicyStageDeniesWriteBeforeExecution` should check `errors.Is(err, ErrPolicyDenied)`.
- `TestAgentDoesNotSaveMemoryOnPolicyDeny` should check `errors.Is(err, ErrPolicyDenied)`.
- `TestPolicyStageReturnsMissingToolError` should check `errors.Is(err, tools.ErrToolNotFound)`.
- Add `TestRegistryMissingToolErrorIsClassifiable` in `tools/registry_test.go`.

**Step 2: Run targeted tests to verify they fail**

Run:

```bash
go test ./agentcore ./tools -run 'TestPolicyStageDeniesWriteBeforeExecution|TestAgentDoesNotSaveMemoryOnPolicyDeny|TestPolicyStageReturnsMissingToolError|TestRegistryMissingToolErrorIsClassifiable' -count=1
```

Expected: FAIL because `ErrPolicyDenied` and `tools.ErrToolNotFound` do not exist or are not wrapped.

**Step 3: Implement sentinels and wrapping**

In `agentcore/policy_stage.go`:

```go
var ErrPolicyDenied = errors.New("policy denied")
```

Return:

```go
return StageAbort, fmt.Errorf("%w: tool %q denied: %s", ErrPolicyDenied, call.Name, decision.Reason)
```

In `tools/registry.go`:

```go
var ErrToolNotFound = errors.New("tool not found")
```

Return:

```go
return nil, fmt.Errorf("%w: %q", ErrToolNotFound, name)
```

In `agentcore/tool_registry.go`, use the same sentinel for the runtime overlay registry:

```go
return nil, fmt.Errorf("%w: %q", tools.ErrToolNotFound, name)
```

**Step 4: Run targeted tests**

Run:

```bash
go test ./agentcore ./tools -run 'TestPolicyStageDeniesWriteBeforeExecution|TestAgentDoesNotSaveMemoryOnPolicyDeny|TestPolicyStageReturnsMissingToolError|TestRegistryMissingToolErrorIsClassifiable' -count=1
```

Expected: PASS.

**Step 5: Commit**

```bash
git add agentcore/policy_stage.go agentcore/policy_stage_test.go agentcore/agent_test.go tools/registry.go tools/registry_test.go agentcore/tool_registry.go
git commit -m "feat: add classifiable core errors"
```

## Task 2: Document API Tiers

**Files:**
- Modify: `README.md`
- Modify: `agentcore/doc.go`
- Modify: `ports/doc.go`
- Modify: `tools/doc.go`

**Step 1: Update README**

Add a `Core API Stability` section after `What The Core Owns` with:

- stable host API
- advanced runtime API
- out-of-core responsibilities
- classifiable errors

Mention:

```go
errors.Is(err, agentcore.ErrPolicyDenied)
errors.Is(err, tools.ErrToolNotFound)
```

**Step 2: Update package docs**

`agentcore/doc.go` should say:

- `Agent`, options, requests/results, events, budget, skills, modules are host-facing API.
- stages, `Pipeline`, `RunState`, and `ReActRunner` are advanced runtime API.

`ports/doc.go` should say:

- `ports` owns cross-package extension contracts and DTOs.
- implementation packages should depend on ports without reverse dependencies.

`tools/doc.go` should mention:

- `ErrToolNotFound` is classifiable with `errors.Is`.

**Step 3: Run docs-neutral tests**

Run:

```bash
go test ./agentcore ./ports ./tools -count=1
```

Expected: PASS.

**Step 4: Commit**

```bash
git add README.md agentcore/doc.go ports/doc.go tools/doc.go
git commit -m "docs: document core api tiers"
```

## Task 3: Final Verification

**Files:**
- No new code changes expected.

**Step 1: Run full verification**

Run:

```bash
make verify
```

Expected: PASS.

**Step 2: Inspect status**

Run:

```bash
git status --short --branch
```

Expected: clean working tree on `codex/core-api-stabilization`.

**Step 3: Report**

Summarize:

- API tiers documented.
- `ErrPolicyDenied` and `tools.ErrToolNotFound` added.
- `errors.Is` contract tests added.
- Verification passed.
