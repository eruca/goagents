# Policy Contract Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make policy the explicit host-side safety gate for model-requested tool execution.

**Architecture:** Tighten the default policy engine so only explicit `read` is allowed by default, while empty, unknown, `write`, and `exec` permissions are denied unless request-scoped permissions allow them. Pin `PolicyStage` behavior with tests, add a runnable `examples/policy`, and document the safety boundary. Do not add RBAC, ACLs, approval flows, policy DSLs, OPA, transport, or persistence.

**Tech Stack:** Go standard library, existing `agentcore`, `policy`, `ports`, `tools`, and `memory` packages, `go test`, `go run`, `make verify`.

---

### Task 1: Tighten Default Policy Engine

**Files:**
- Modify: `policy/engine_test.go`
- Modify: `policy/engine.go`

**Step 1: Write failing tests**

Update `policy/engine_test.go` to make the default contract explicit:

```go
func TestEngineAllowsReadByDefault(t *testing.T) {
	engine := NewEngine()
	decision := engine.Decide(Request{Permission: PermissionRead})
	if !decision.Allowed {
		t.Fatalf("Allowed = false, reason = %q", decision.Reason)
	}
}

func TestEngineDeniesWriteAndExecByDefaultUnlessAllowed(t *testing.T) {
	engine := NewEngine()
	for _, permission := range []Permission{PermissionWrite, PermissionExec} {
		denied := engine.Decide(Request{Permission: permission})
		if denied.Allowed {
			t.Fatalf("%s permission allowed by default", permission)
		}

		allowed := engine.Decide(Request{
			Permission: permission,
			Allowed:    []Permission{permission},
		})
		if !allowed.Allowed {
			t.Fatalf("%s not allowed by request: %q", permission, allowed.Reason)
		}
	}
}

func TestEngineDeniesEmptyAndUnknownPermissions(t *testing.T) {
	engine := NewEngine()
	for _, permission := range []Permission{"", "unknown"} {
		decision := engine.Decide(Request{Permission: permission})
		if decision.Allowed {
			t.Fatalf("%q permission allowed", permission)
		}
	}
}
```

**Step 2: Run tests to verify failure**

Run:

```bash
go test ./policy -run TestEngine -v
```

Expected: FAIL because empty permission is currently allowed.

**Step 3: Implement minimal policy change**

Update `policy/engine.go`:

```go
func (e *Engine) Decide(req Request) Decision {
	if req.Permission == PermissionRead {
		return Decision{Allowed: true, Reason: "read allowed"}
	}
	if slices.Contains(req.Allowed, req.Permission) {
		return Decision{Allowed: true, Reason: "permission allowed by request"}
	}
	return Decision{Allowed: false, Reason: "permission denied by default"}
}
```

**Step 4: Run tests**

Run:

```bash
go test ./policy -run TestEngine -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add policy/engine.go policy/engine_test.go
git commit -m "feat: deny unspecified tool permissions"
```

### Task 2: Pin PolicyStage Enforcement

**Files:**
- Create: `agentcore/policy_stage_test.go`

**Step 1: Write tests**

Create `agentcore/policy_stage_test.go` with focused stage tests:

```go
package agentcore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/tools"
)

func TestPolicyStageAllowsReadTool(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(policyTestTool{spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead}})
	state := NewRunState(NewRunID(), RunRequest{})
	state.PendingCalls = []tools.Call{{Name: "lookup", Input: json.RawMessage(`{}`)}}

	result, err := PolicyStage{Engine: policy.NewEngine(), ToolRegistry: registry}.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result != StageContinue {
		t.Fatalf("result = %v", result)
	}
}

func TestPolicyStageDeniesWriteBeforeExecution(t *testing.T) {
	called := false
	registry := tools.NewRegistry()
	registry.Register(policyTestTool{
		spec: tools.Spec{Name: "write_file", Permission: policy.PermissionWrite},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			called = true
			return &tools.Result{ForLLM: "wrote"}, nil
		},
	})
	state := NewRunState(NewRunID(), RunRequest{})
	state.PendingCalls = []tools.Call{{Name: "write_file", Input: json.RawMessage(`{}`)}}

	result, err := PolicyStage{Engine: policy.NewEngine(), ToolRegistry: registry}.Run(context.Background(), state)
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if result != StageAbort {
		t.Fatalf("result = %v", result)
	}
	if called {
		t.Fatal("tool body was called")
	}
}

func TestPolicyStageAllowsRequestScopedWritePermission(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(policyTestTool{spec: tools.Spec{Name: "write_file", Permission: policy.PermissionWrite}})
	state := NewRunState(NewRunID(), RunRequest{})
	state.PendingCalls = []tools.Call{{Name: "write_file", Input: json.RawMessage(`{}`)}}
	state.AllowedPermissions = []policy.Permission{policy.PermissionWrite}

	result, err := PolicyStage{Engine: policy.NewEngine(), ToolRegistry: registry}.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result != StageContinue {
		t.Fatalf("result = %v", result)
	}
}

func TestPolicyStageReturnsMissingToolError(t *testing.T) {
	state := NewRunState(NewRunID(), RunRequest{})
	state.PendingCalls = []tools.Call{{Name: "missing", Input: json.RawMessage(`{}`)}}

	result, err := PolicyStage{Engine: policy.NewEngine(), ToolRegistry: tools.NewRegistry()}.Run(context.Background(), state)
	if err == nil || err.Error() != `tool "missing" not registered` {
		t.Fatalf("err = %v", err)
	}
	if result != StageAbort {
		t.Fatalf("result = %v", result)
	}
}

type policyTestTool struct {
	spec tools.Spec
	run  func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error)
}

func (t policyTestTool) Spec() tools.Spec {
	return t.spec
}

func (t policyTestTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	if t.run == nil {
		return &tools.Result{ForLLM: "ok"}, nil
	}
	return t.run(ctx, input, env)
}

var _ ports.Tool = policyTestTool{}
```

**Step 2: Run tests**

Run:

```bash
go test ./agentcore -run TestPolicyStage -v
```

Expected: PASS after Task 1. If run before Task 1, write denial still passes but empty-permission behavior is not covered here.

**Step 3: Commit**

```bash
git add agentcore/policy_stage_test.go
git commit -m "test: pin policy stage contract"
```

### Task 3: Pin Policy Denial Memory Boundary

**Files:**
- Modify: `agentcore/agent_test.go`

**Step 1: Write test**

Add `TestAgentDoesNotSaveMemoryOnPolicyDeny`:

```go
func TestAgentDoesNotSaveMemoryOnPolicyDeny(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "write_file", Input: json.RawMessage(`{}`)}}},
	}}
	memory := &mockMemoryProvider{}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write_file", Permission: policy.PermissionWrite},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "wrote"}, nil
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
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if memory.saveCalls != 0 {
		t.Fatalf("save calls = %d", memory.saveCalls)
	}
}
```

**Step 2: Run focused test**

Run:

```bash
go test ./agentcore -run TestAgentDoesNotSaveMemoryOnPolicyDeny -v
```

Expected: PASS.

**Step 3: Commit**

```bash
git add agentcore/agent_test.go
git commit -m "test: pin policy denial memory boundary"
```

### Task 4: Add Policy Example

**Files:**
- Create: `examples/policy/main.go`
- Create: `examples/policy/README.md`

**Step 1: Run failing command**

Run:

```bash
go run ./examples/policy
```

Expected: FAIL because `examples/policy` does not exist.

**Step 2: Implement example**

Create a small example with:

- `readLLM` requesting a `lookup` tool with `PermissionRead`, then returning `read allowed`.
- `writeLLM` requesting a `write_file` tool with `PermissionWrite`.
- `lookupTool` returns `ForLLM: "read allowed"`.
- `writeTool` sets a `called` flag if executed.
- Run a read Agent and print the final content.
- Run a write Agent and print `write denied` if the run returns an error and `called` remains false.

Keep the example local and deterministic. Do not add RBAC, approval, OPA, files, HTTP, or external systems.

**Step 3: Run example**

Run:

```bash
go run ./examples/policy
```

Expected: PASS with output:

```text
read allowed
write denied
```

**Step 4: Commit**

```bash
git add examples/policy/main.go examples/policy/README.md
git commit -m "docs: add policy example"
```

### Task 5: Document Policy Contract

**Files:**
- Modify: `README.md`
- Modify: `policy/doc.go`

**Step 1: Update README**

Add a `Policy` section:

```markdown
## Policy

Policy is the host-side safety gate between model-requested tool calls and tool execution.

- The model can request a tool call, but policy must allow it before `ActStage` runs the tool.
- The default policy allows explicit `read` tools.
- The default policy denies `write`, `exec`, empty, and unknown permissions.
- Request-scoped allowed permissions can allow mutating actions for a run.
- A policy denial aborts the run before tool execution and memory is not saved.
- Use `WithPolicyEngine` to replace the default policy with host-specific checks.

This is not a full RBAC or approval system. It is the Agent-side enforcement point.

See `examples/policy` for a minimal policy example.
```

**Step 2: Update package docs**

Update `policy/doc.go` with the same contract in package-level language.

**Step 3: Run docs-neutral verification**

Run:

```bash
go test ./policy ./agentcore
```

Expected: PASS.

**Step 4: Commit**

```bash
git add README.md policy/doc.go
git commit -m "docs: document policy contract"
```

### Task 6: Final Verification

**Files:**
- No code changes expected.

**Step 1: Run full verification**

Run:

```bash
make verify
go run ./examples/policy
```

Expected: PASS.

**Step 2: Confirm status**

Run:

```bash
git status --short
```

Expected: no output.
