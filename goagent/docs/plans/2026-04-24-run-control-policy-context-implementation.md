# Run Control Policy Context Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make per-run permissions and policy context part of the public `Agent.Run` contract.

**Architecture:** Add typed run-control fields to `RunRequest`, copy them into `RunState`, and pass them into `PolicyRequest` before tool execution. Keep the default policy engine behavior unchanged so existing users only gain a richer contract.

**Tech Stack:** Go, `agentcore`, `ports`, `policy`, `tools`, `go test`.

---

### Task 1: Expose Allowed Permissions On RunRequest

**Files:**
- Modify: `agentcore/request.go`
- Modify: `agentcore/run_state.go`
- Test: `agentcore/policy_stage_test.go`
- Test: `agentcore/react_test.go`

**Step 1: Write failing tests**

Add a test proving `Agent.Run` can allow a write tool by setting
`RunRequest.AllowedPermissions`.

Expected behavior:
- a write tool is denied without request-scoped permission;
- the same write tool executes when `RunRequest.AllowedPermissions` contains
  `policy.PermissionWrite`;
- the test goes through `Agent.Run`, not direct `RunState` mutation.

**Step 2: Run the focused tests**

Run:

```bash
go test ./agentcore -run 'AllowedPermissions|Policy'
```

Expected: fail because `RunRequest` does not expose allowed permissions yet.

**Step 3: Implement the public field**

Add to `RunRequest`:

```go
AllowedPermissions []policy.Permission
```

Copy the slice in `NewRunState`:

```go
AllowedPermissions: append([]policy.Permission(nil), req.AllowedPermissions...),
```

Keep the existing `RunState.AllowedPermissions` field so advanced runtime users
do not break.

**Step 4: Run tests**

Run:

```bash
go test ./agentcore
```

Expected: pass.

**Step 5: Update docs**

Update `README.md` policy section to say mutating actions can be allowed through
`RunRequest.AllowedPermissions`.

**Step 6: Commit**

```bash
git add agentcore/request.go agentcore/run_state.go agentcore/policy_stage_test.go agentcore/react_test.go README.md
git commit -m "feat: 增加运行级权限控制"
```

### Task 2: Add Typed Policy Context

**Files:**
- Modify: `agentcore/request.go`
- Modify: `ports/policy.go`
- Modify: `agentcore/policy_stage.go`
- Test: `agentcore/policy_stage_test.go`

**Step 1: Write failing tests**

Add a custom policy engine test that captures the `PolicyRequest` received by
`PolicyStage`.

Assert it receives:
- `RunID`
- `UserID`
- `SessionID`
- tool name
- permission
- raw tool input
- request metadata
- typed policy context

**Step 2: Run focused tests**

Run:

```bash
go test ./agentcore -run PolicyStage
```

Expected: fail because `PolicyRequest` lacks the new fields.

**Step 3: Add shared policy context type**

Define a small type in `ports/policy.go`:

```go
type PolicyContext struct {
	TenantID  string
	RequestID string
	TraceID   string
	Labels    map[string]string
}
```

Add it to `RunRequest`:

```go
PolicyContext ports.PolicyContext
```

**Step 4: Extend PolicyRequest**

Extend `ports.PolicyRequest`:

```go
type PolicyRequest struct {
	RunID      string
	UserID     string
	SessionID  string
	Tool       string
	Permission Permission
	Input      json.RawMessage
	Allowed    []Permission
	Context    PolicyContext
	Metadata   map[string]any
}
```

**Step 5: Thread fields through PolicyStage**

When building `policy.Request`, pass:
- `state.RunID.String()`
- `state.Input.UserID`
- `state.Input.SessionID`
- `call.Input`
- `state.Input.PolicyContext`
- `state.Metadata`

The default `policy.Engine` should ignore the new fields.

**Step 6: Run tests**

Run:

```bash
go test ./agentcore ./policy ./ports
```

Expected: pass.

**Step 7: Update docs**

Update:
- `README.md`
- `docs/plans/2026-04-24-run-control-and-tool-safety-design.md` if the final
  shape differs from the design.

**Step 8: Commit**

```bash
git add agentcore/request.go ports/policy.go agentcore/policy_stage.go agentcore/policy_stage_test.go README.md docs/plans/2026-04-24-run-control-and-tool-safety-design.md
git commit -m "feat: 为策略检查传递运行上下文"
```

### Task 3: Verify Public Contract

**Files:**
- Modify: `README.md`
- Modify: `examples/policy/README.md`
- Modify: `examples/policy/main.go` if a compact demo helps clarify the new API.

**Step 1: Add documentation examples**

Show one small snippet:

```go
result, err := agent.Run(ctx, agentcore.RunRequest{
	Input: "update the draft",
	AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	PolicyContext: ports.PolicyContext{
		RequestID: "request-123",
	},
})
```

**Step 2: Run full verification**

Run:

```bash
make verify
```

Expected: pass.

**Step 3: Commit**

```bash
git add README.md examples/policy/README.md examples/policy/main.go
git commit -m "docs: 说明运行级策略控制"
```

## Stop Condition

Stop after Task 3. Do not implement JSON Schema enforcement, execution modes, or
tool result references in this batch. Those are separate follow-up batches after
the run-control contract is stable.
