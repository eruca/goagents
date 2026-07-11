# Host API Durable Tool Approval Design

## Goal

Close the local host runtime's missing path: a tool-capable agent pauses before
a write, saves encrypted state in the existing `agent-runs.db`, accepts an OIDC
operator decision, and either resumes exactly once or terminates without tool
execution.

## Scope and boundary

This is host composition only. `goagent` keeps its existing checkpoint and
resume API; `workflowkit` keeps its existing workflow state machine. The host
adds one deterministic demonstration tool, `record_review`, which only writes a
small artifact beneath the configured local artifact root. It does not publish,
call a network service, or mutate a user-owned external system.

The existing `POST /workflows/{id}/approve` remains a separate final-output
approval. Tool approval happens earlier through
`POST /workflows/{id}/agent-approve`.

## Chosen approach

`task_profile.needs_tools=true` opts the host demo agent into the local
`record_review` write tool. The host allows that one tool through the ordinary
agent policy, but its approver returns `Pending`; no write executes until a
verified operator resolves the checkpoint.

At the interruption boundary the host records only safe operator-facing
metadata (checkpoint ID, call index, call ID, tool name) in workflow metadata.
It never copies tool input, raw user input, messages, or checkpoint plaintext
there. The encrypted `RunCheckpoint` is stored through `runkit/sqlitestore`;
its AES-GCM data key is lazily opened from macOS Keychain for a real local run.
Tests inject a fake cipher and do not use the real Keychain.

## HTTP contract

`GET /workflows/{id}` and a tool-paused `POST /workflows` response include:

```json
{
  "agent_approval": {
    "checkpoint_id": "opaque-id",
    "tools": [
      {"index": 0, "tool_call_id": "call-record-review", "tool": "record_review"}
    ]
  }
}
```

Only tool name and call identity are exposed. Tool JSON input remains encrypted.

`POST /workflows/{id}/agent-approve` requires the same OIDC bearer
authentication as workflow final approval. Its strict JSON request is:

```json
{
  "resolutions": [
    {
      "index": 0,
      "tool_call_id": "call-record-review",
      "tool": "record_review",
      "allowed": true
    }
  ]
}
```

The host derives tenant (`local`), definition hash, audit reference, lease
owner, expiry, and next checkpoint ID; callers cannot provide them. It also
rejects caller-provided free-form reasons so they cannot enter agent events.
For an allowed batch, the adapter requires an exact match before it executes any tool.
For a rejected batch, the host records a rejection without decrypting or
executing the checkpoint, marks the agent run failed, and cancels the workflow.

On successful tool resume the host writes the resulting agent output artifact,
completes the agent run, and returns the workflow to its existing *final-output*
approval state. A caller must still use `POST /workflows/{id}/approve` to
finalize the workflow. If the agent pauses again, the adapter saves the next
checkpoint first and the host exposes its new safe metadata.

## Components

- `routingAgentRunner` becomes a `goagentapproval.Resumer`: it rebuilds the
  same host tools from checkpoint request metadata before `ResumeDetailed`.
- `hostAgentStep` detects `ErrApprovalPending`, delegates encrypted persistence
  to a host approval service, and maps it to `workflowkit.StatusWaitingApproval`.
- A host approval service owns local tenant/definition values, lazily builds the
  Keychain AES-GCM cipher for real runs, and coordinates the adapter with
  workflow/runkit finalization.
- The HTTP handler authenticates OIDC first, then calls the service. It neither
  stores the bearer token nor accepts caller-supplied approver identity.

## Failure semantics

- Missing/invalid OIDC token: `401`, no checkpoint transition or tool execution.
- Invalid resolution: adapter lease becomes terminal `failed`; workflow becomes
  failed and cannot replay the tool automatically.
- Explicit rejection: checkpoint becomes `rejected`; workflow becomes
  `cancelled`; the tool remains unexecuted.
- Artifact or run-summary write failure after resume: workflow becomes failed;
  there is no misleading successful final approval state.
- A non-macOS or locked/unavailable Keychain fails the tool-capable run closed;
  there is intentionally no file or environment fallback.

## Acceptance evidence

Tests prove the tool has no side effect before approval, a valid OIDC identity
is audited and resumes it once, rejection performs no tool write, a restart can
resume an encrypted stored checkpoint with the same injected test cipher, and
the ordinary final-output approval still remains required after tool approval.
