# Host API Agent Approval Expiry Design

## Goal

Ensure a local host does not leave a workflow indefinitely in
`waiting_approval` after its encrypted agent-tool checkpoint has expired.

## Scope

This change connects existing approval-checkpoint expiry to host workflow and
agent-run state. It does not change `goagent` pause/resume semantics, tool
authorization, OIDC authentication, Keychain behavior, or distributed-worker
scope.

## Chosen design

`host-api` gains `ReconcileExpiredAgentApprovals(ctx, now)`. It obtains the
existing `CheckpointStore`, first calls `ExpireCheckpoints(ctx, now)`, then
queries all local `waiting_approval` workflows through the existing metadata
query contract. It considers only workflows with safe agent-approval metadata,
reads their checkpoint state, and reconciles a workflow only when that state is
`expired`. It then:

1. verifies that the workflow still names the same agent-approval checkpoint;
2. completes the correlated agent run with `StatusFailed` and the stable abort
   reason `agent tool approval expired`;
3. clears safe approval metadata and marks the workflow `failed` with the same
   stable reason.

The sweep deliberately uses an internal query with no limit. This is an
in-process local-host operation, not the bounded HTTP list endpoint. It avoids
adding another store API and makes reconciliation idempotent: if persisting a
terminal run or workflow update fails, the same still-waiting workflow is
checked again in the next sweep.

The host starts an in-process janitor from `main`, on a configurable interval.
The janitor only calls the explicit reconciliation method; it introduces no new
HTTP endpoint or worker framework. The method remains directly callable in
tests with a supplied time, so tests do not wait for a clock or touch Keychain.

## Configuration

`HOST_API_AGENT_APPROVAL_SWEEP_INTERVAL` is a positive Go duration. It defaults
to `1m`. Invalid or non-positive values fail host startup, matching existing
queued-worker lease configuration. The interval controls cleanup latency; the
approval deadline remains the existing one-hour checkpoint lifetime.

## Error semantics

- A store expiry error leaves all host workflow state unchanged for that sweep.
- A run-summary persistence error leaves the workflow waiting; the expired
  checkpoint is already fail-closed and the next sweep retries the same state.
- A workflow update error is returned to the janitor after the run summary is
  terminal; because its metadata is still present, the next sweep retries the
  update. No successful final-output approval is emitted.
- An operator decision racing after expiry cannot lease the checkpoint because
  checkpoint leasing already requires `ExpiresAt` to be in the future.

## Acceptance evidence

Tests prove that a host expiry sweep changes the matching workflow and agent
run to failed without executing the tool, ignores unrelated expired
checkpoints, retries a previously expired waiting workflow, and rejects invalid
janitor configuration. Workspace verification remains green.
