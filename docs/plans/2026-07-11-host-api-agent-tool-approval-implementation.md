# Host API Durable Tool Approval Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Connect a real local host tool pause, encrypted SQLite checkpoint, OIDC decision, and exactly-once resume without changing the `goagent` or `workflowkit` cores.

**Architecture:** The host creates a safe `record_review` write tool only for `needs_tools` workflows. A host-owned approval service persists `agentcore.RunCheckpoint` through `goagentapproval.Adapter`, using lazy macOS Keychain AES-GCM in real runs and an injected cipher in tests. The tool-specific endpoint is separate from existing final-output workflow approval.

**Tech Stack:** Go 1.26, `goagent/agentcore`, `runkit/sqlitestore`, `runkit/goagentapproval`, `runkit/approvalcrypto`, macOS Keychain, OIDC HTTP authentication.

## Global Constraints

- Reuse `$HOST_RUNTIME_HOME/agent-runs.db`; do not add a database or secret file.
- Do not add a production key fallback outside macOS Keychain.
- Persist or expose no raw checkpoint, raw tool input, prompt, or bearer token.
- Keep `POST /workflows/{id}/approve` as final-output approval; use a distinct tool-approval endpoint.
- The demonstration write tool may only write an artifact beneath the configured artifact root.
- Every behavioral change begins with a failing test.

---

### Task 1: Specify host-level tool approval behavior with failing tests

**Files:**

- Create: `examples/host-api/agent_approval_test.go`
- Modify: `examples/host-api/server_test.go`

**Consumes:** existing `Config`, OIDC test authenticator, workflow HTTP helpers, and `runkit.CheckpointStore`.

**Produces:** tests defining the public workflow response and tool-approval endpoint semantics.

- [x] Add a test that posts a `needs_tools=true` workflow, asserts it is waiting with safe `agent_approval.tools` metadata, and verifies the `record_review` artifact does not exist before a decision.
- [x] Run `go test ./... -run TestHostAPIAgentToolApproval -count=1` from `examples/host-api`; observe expected failure before the response field and endpoint exist.
- [x] Add tests for: missing OIDC token returns `401` without a write; authenticated exact approval executes once then leaves the workflow awaiting final-output approval; explicit rejection cancels the workflow and never writes the artifact; reopening the same runtime with the same injected cipher can approve the persisted checkpoint.
- [x] Keep the injected test cipher local to the test file and assert it receives non-empty AAD; tests do not call macOS Keychain.

### Task 2: Build the host approval service and tool-capable runner

**Files:**

- Create: `examples/host-api/agent_approval.go`
- Modify: `examples/host-api/server.go`

**Consumes:** `runkit.CheckpointStore`, `goagentapproval.Adapter`, `approvalcrypto.OpenMacOSKeychainKeyProvider`, `agentcore.ErrApprovalPending`, and workflow metadata.

**Produces:** a lazy local approval service, a safe `record_review` tool, and `routingAgentRunner.ResumeDetailed`.

- [x] Implement a `hostAgentApprovalService` with constants `local` tenant and versioned host definition hash. Its `adapter()` method uses an injected `Config.AgentApprovalCipher` when supplied; otherwise it lazily constructs the Keychain AES-GCM cipher only on the tool path.
- [x] Implement a `recordReviewTool` with `PermissionWrite`, a strict empty-object schema, and an `Execute` method that writes only `artifact:<workflow-id>:review-action`. It returns a bounded reference and model observation, never the source artifact content.
- [x] Extend `routingAgentRunner` so `needs_tools` registers this tool, grants only `PermissionWrite` for that request, supplies a pending approver, and reconstructs the same configuration in `ResumeDetailed` from checkpoint metadata.
- [x] Update `hostAgentStep.Run` to save a pending checkpoint on `ErrApprovalPending`. It stores only checkpoint ID plus serialized safe tool identities in workflow metadata, maps the step to `waiting_approval`, and leaves the tool body unexecuted.
- [x] Run `go test ./... -run TestHostAPIAgentToolApproval -count=1`; the pause test passes before adding the HTTP decision handler.

### Task 3: Add authenticated decision handling and workflow state transitions

**Files:**

- Modify: `examples/host-api/server.go`
- Modify: `examples/host-api/agent_approval.go`
- Modify: `examples/host-api/agent_approval_test.go`

**Consumes:** host approval service, `ApprovalAuthenticator`, adapter lease requests, and existing `workflowkit.Executor` final approval behavior.

**Produces:** `POST /workflows/{id}/agent-approve` and safe response projection.

- [x] Register `POST /workflows/{id}/agent-approve` and authenticate before JSON decoding or checkpoint access.
- [x] Accept only `resolutions`; derive tenant, definition hash, audit reference, lease owner, expiry, and next checkpoint ID inside the server. For an allowed batch call `ApproveAndResume`; for any denial call `Reject` and never decrypt or execute state.
- [x] On a successful resume, write the output artifact, complete the run summary, then update the workflow's output reference and reset it to the existing final-output approval ref. If any host persistence step fails, mark the workflow failed and do not expose a successful final approval state. On another pause, publish only the new safe metadata. On rejection, complete the run as failed and cancel the workflow. On an adapter/resume failure, mark the workflow failed without retrying the lease.
- [x] Run the focused test command and then `go test ./...` from `examples/host-api`; all host-api tests pass.

### Task 4: Document and verify the local end-to-end boundary

**Files:**

- Modify: `examples/host-api/README.md`
- Modify: `examples/host-api/openapi.yaml`
- Modify: `docs/host-api-contract.md`
- Modify: `docs/plans/2026-07-11-host-api-agent-tool-approval-implementation.md`

**Consumes:** final HTTP request/response shapes and local Keychain behavior.

**Produces:** an accurate operator contract with no secret-handling ambiguity.

- [x] Document the new safe `agent_approval` projection, the OIDC-protected endpoint, exact-resolution requirement, separate final approval, and local Keychain behavior.
- [x] Add OpenAPI request/response schema entries; do not describe checkpoint plaintext or token storage.
- [x] Run `bash ./scripts/verify-all.sh`, `cd runkit && go test -race ./...`, and `git diff --check` from repository root.
- [x] Mark each completed checkbox only after its command succeeds.
