# Policy Contract Design

## Goal

Make policy the clear host-side safety gate for model-requested tool execution.

## Context

Tools expose host-owned actions to the Agent. The model can request tool calls, but the host must decide which calls are allowed before any tool body runs. `PolicyStage` is the boundary between model intent and execution.

The current policy engine allows explicit `read`, denies `write` and `exec` unless requested permissions allow them, and also allows an empty permission. For a safer public contract, empty or unknown permissions should not be treated as safe. Tool authors should set `Spec.Permission` explicitly.

## Design

Keep the policy API small:

- `ports.PolicyEngine` remains `Decide(PolicyRequest) PolicyDecision`.
- The default `policy.Engine` remains deterministic and dependency-free.
- `WithPolicyEngine` remains the replacement point for host-specific policy.

Tighten the default policy contract:

1. `read` is allowed by default.
2. `write` and `exec` are denied by default.
3. `write` and `exec` may be allowed when `RunState.AllowedPermissions` contains that permission.
4. Empty or unknown permissions are denied by default.
5. A denied policy decision aborts the run before tool execution.
6. A policy denial does not save memory because the run did not reach a final answer.

This is not a full authorization system. It is the Agent-side enforcement point that host applications can replace or wrap. Real products may still need user identity, RBAC, audit logs, approvals, or external policy engines, but those are outside the core library.

## Example Shape

Add `examples/policy` with two small runs:

- A read-only lookup tool succeeds under the default policy.
- A write tool is denied under the default policy and its body is not executed.

The example should print compact lines such as:

- `read allowed`
- `write denied`

Use mock LLMs and deterministic tools only. Do not add RBAC, OPA, approval workflows, HTTP transport, or real external systems.

## Tests

Add focused tests:

- default engine allows explicit `read`
- default engine denies `write`
- default engine denies `exec`
- default engine denies empty permission
- default engine denies unknown permission
- request-scoped allowed permissions can allow `write`
- policy denial stops before tool execution
- policy denial does not save memory
- missing tool spec aborts before execution

## Non-Goals

- No RBAC or ACL model.
- No policy DSL.
- No OPA or external policy engine integration.
- No human approval flow.
- No audit log persistence.
- No HTTP/SSE transport.

## Success Criteria

- Default policy behavior is pinned by tests.
- `PolicyStage` denial behavior is pinned by tests.
- `examples/policy` demonstrates read allowed and write denied.
- README and policy package docs explain the safety boundary.
- `make verify` passes.
