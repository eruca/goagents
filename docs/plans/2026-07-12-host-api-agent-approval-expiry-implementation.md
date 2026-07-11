# Host API Agent Approval Expiry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Reconcile expired encrypted agent-tool approvals into terminal host workflow and agent-run state.

**Architecture:** Host API calls the existing checkpoint expiry operation, internally lists local waiting workflows, and checks only those carrying safe agent-approval metadata. When their checkpoint is expired, it completes the agent run as failed and then marks the workflow failed. An in-process janitor invokes that testable method at startup-configured intervals.

**Tech Stack:** Go 1.26, runkit memory/SQLite/PostgreSQL stores, workflowkit metadata query, host-api runtime loop.

## Global Constraints

- Do not expose checkpoint ciphertext, raw checkpoint content, tool inputs, prompts, or bearer tokens.
- Do not add a database, an HTTP endpoint, or a distributed scheduler.
- Preserve existing `goagent`, `workflowkit`, and `runkit` public APIs.
- Treat an expired approval as terminally failed; never replay or execute its tool.
- Use stable host-visible reason `agent tool approval expired`.

---

### Task 1: Reconcile expired approval records in Host API

**Files:**

- Create: `examples/host-api/agent_approval_expiry.go`
- Create: `examples/host-api/agent_approval_expiry_test.go`
- Modify: `examples/host-api/server.go`

**Consumes:** existing `CheckpointStore.ExpireCheckpoints`, `CheckpointStore.GetCheckpoint`, `workflowkit.WorkflowQuery{Status, Limit}`, `runkit.Store.Complete`, and safe agent-approval metadata helpers.

**Produces:** `Server.ReconcileExpiredAgentApprovals(ctx, now) (int, error)`.

- [x] Write a failing host test that creates a tool-paused workflow, sweeps at `time.Now().Add(2*time.Hour)`, and asserts checkpoint expired, workflow failed, run failed with the stable reason, safe metadata cleared, and no review-action artifact.
- [x] Write a failing test that leaves a workflow waiting after a prior expiry, then asserts a later sweep retries its reconciliation.
- [x] Implement the reconciliation method. Expire checkpoints, internally list all waiting workflows, inspect only records with safe agent-approval metadata, and complete/update each matching expired record. Skip non-agent waiting workflows and no-longer-pending metadata.
- [x] Run `go test ./... -run TestHostAPIAgentApprovalExpiry -count=1` from `examples/host-api`.

### Task 2: Start and configure the local approval janitor

**Files:**

- Modify: `examples/host-api/server.go`
- Modify: `examples/host-api/main.go`
- Modify: `examples/host-api/agent_approval_expiry_test.go`

**Consumes:** `Server.ReconcileExpiredAgentApprovals`, runtime configuration style from queued-worker lease configuration, and process context cancellation.

**Produces:** `StartAgentApprovalJanitor(context.Context)` and validated sweep interval configuration.

- [x] Write failing tests for invalid/zero `HOST_API_AGENT_APPROVAL_SWEEP_INTERVAL` and for a short configured interval invoking reconciliation through the janitor loop.
- [x] Add `loadAgentApprovalJanitorConfig`, default one-minute interval, cancellation-aware ticker loop, and startup invocation in `main`.
- [x] Run `go test ./...` from `examples/host-api`.

### Task 3: Document and verify

**Files:**

- Modify: `examples/host-api/README.md`
- Modify: `docs/host-api-contract.md`
- Modify: `docs/plans/2026-07-12-host-api-agent-approval-expiry-implementation.md`

- [x] Document the one-hour approval lifetime, sweep interval environment variable, expiry failure state, and no-replay behavior.
- [x] Run `bash ./scripts/verify-all.sh`, `cd runkit && go test -race ./...`, and `git diff --check` from repository root.
- [x] Mark each completed checkbox only after its command succeeds.
