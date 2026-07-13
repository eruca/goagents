# Host API contract

This document captures the current HTTP contract of `examples/host-api`. The
example is a host-side composition surface, not a new core module.

The machine-readable contract lives at
`examples/host-api/openapi.yaml`. Keep this document and that OpenAPI file in
sync when the example HTTP surface changes.

## Runtime

Environment:

- `HOST_API_ADDR`: listen address. If unset, the example uses its default from
  `main.go`: `127.0.0.1:8080`.
- `HOST_RUNTIME_HOME`: durable runtime directory. If unset, host-api creates a
  temporary directory.
- `LLMKIT_HOME`: llmkit config and audit directory. If unset, it defaults to
  `$HOST_RUNTIME_HOME/.llmkit`.
- `HOST_API_QUEUED_LEASE_DURATION`: optional Go duration for the in-process
  queued worker lease. Defaults to `1m`.

Runtime files:

```text
$HOST_RUNTIME_HOME/
  workflow.db
  agent-runs.db
  artifacts/
  .llmkit/
    config.yaml
    route-events.jsonl
    outcomes.jsonl
    model-stats.json
```

If `.llmkit/config.yaml` is missing, host-api uses static demo providers. If it
exists, provider clients are built from the config. Referenced `api_key_env`
values must be set or startup fails.

## Error Response

All handler-level errors use:

```json
{
  "error": "invalid_request",
  "message": "id is required"
}
```

Common error codes:

- `invalid_request`
- `invalid_json`
- `unsupported_run_mode`
- `invalid_task_profile`
- `workflow_error`
- `artifact_error`
- `llm_audit_error`
- `run_error`
- `not_found`

## POST /workflows

Creates and runs a synchronous workflow until it waits for approval.

Request:

```json
{
  "id": "wf-review-1",
  "input": "Review this policy.",
  "run_mode": "sync",
  "task_profile_preset": "high_success",
  "task_profile": {
    "task_type": "policy_review",
    "complexity": "hard",
    "latency": "normal",
    "failure_cost": "high",
    "privacy": "cloud_allowed",
    "max_estimated_cents": 10,
    "needs_reasoning": true,
    "needs_tools": false,
    "needs_json": false,
    "needs_long_context": false
  }
}
```

Fields:

- `id`: required workflow id.
- `input`: input text written as an artifact.
- `run_mode`: optional. `sync` is supported and default. `queued` is an
  in-process proof that saves a pending workflow and returns immediately while a
  background worker loop claims a `workflowkit.QueueLeaseStore` lease, advances
  it, extends the lease while the workflow is running, and releases the lease.
  Restarting the host with the same runtime home can recover pending or
  expired-lease workflows. `queued` does not provide worker crash supervision or
  multi-worker scheduling semantics.
- `task_profile_preset`: optional. Supported values are `simple_local`,
  `balanced`, `high_success`, and `local_only`.
- `task_profile`: optional host patch. Preset values are applied first. Missing
  fields inherit the preset or default profile. Present string fields must be
  non-empty and override the base profile. Present boolean fields explicitly
  override the base profile, including `false`. `max_estimated_cents` must be
  greater than zero when present and is a hard per-task budget filter when
  known candidate cost is available.

Response status: `202 Accepted`.

Response:

```json
{
  "id": "wf-review-1",
  "status": "waiting_approval",
  "run_mode": "sync",
  "input_ref": "artifact:wf-review-1:input",
  "output_ref": "artifact:wf-review-1:agent-output",
  "agent_run_id": "00000000-0000-0000-0000-000000000000",
  "approval_ref": "approval:wf-review-1",
  "waiting_reason": "operator approval required before finalizing host API output",
  "completed_steps": ["ingest", "agent_review"]
}
```

For queued submissions, `run_mode` remains `queued` in later `GET` and approval
responses because the submitted mode is stored in workflow metadata.

## GET /workflows

Returns a bounded operational list of stored workflows.

Query parameters:

- `status`: optional workflow status filter.
- `run_mode`: optional. Supported values are `sync` and `queued`; this filters
  the submitted mode stored in workflow metadata.
- `order`: optional. Supported values are `asc` and `desc`. Defaults to `asc`.
- `limit`: optional positive integer. Defaults to `50` and caps at `100`.

Response:

```json
{
  "workflows": [
    {
      "id": "wf-review-1",
      "status": "waiting_approval",
      "run_mode": "sync",
      "input_ref": "artifact:wf-review-1:input",
      "output_ref": "artifact:wf-review-1:agent-output",
      "agent_run_id": "00000000-0000-0000-0000-000000000000",
      "approval_ref": "approval:wf-review-1"
    }
  ]
}
```

Invalid `status`, `run_mode`, `order`, or `limit` returns
`400 invalid_request`.

## GET /workflows/{id}

Returns the stored workflow state.

Response status: `200 OK`.

Response shape is the same workflow response used by `POST /workflows`.

Missing workflow response: `404 not_found`.

## GET /workflows/{id}/events

Returns a workflow operation timeline for debugging and operator review.

Response status: `200 OK`.

Response:

```json
{
  "workflow_id": "wf-review-1",
  "status": "waiting_approval",
  "run_mode": "queued",
  "current_step": "agent_review",
  "completed_steps": ["ingest", "agent_review"],
  "events": [
    {
      "type": "step",
      "name": "ingest",
      "status": "failed",
      "attempt": 1,
      "error": "artifact not found",
      "started_at": "2026-05-09T12:00:00Z",
      "ended_at": "2026-05-09T12:00:01Z"
    },
    {
      "type": "workflow_requeued",
      "from_status": "failed",
      "to_status": "pending",
      "at": "2026-05-09T12:01:00Z"
    }
  ]
}
```

Step records are returned as `type: "step"` events and preserve step name,
status, attempt, refs, error, approval fields, and timestamps. Manual requeues
are returned as `type: "workflow_requeued"` events. Missing workflow response:
`404 not_found`.

## POST /workflows/{id}/requeue

Explicitly moves a failed or cancelled workflow back to the queued worker.

Request body: none.

Behavior:

- Allowed only when the stored workflow status is `failed` or `cancelled`.
- Updates the same workflow id back to `pending`; it does not create a new
  workflow.
- Clears the terminal error, current step, waiting approval fields, and any
  stale lease.
- Preserves completed steps, step attempts, and step records so the executor can
  continue from unfinished work.
- Stores `run_mode: "queued"` in workflow metadata and wakes the in-process
  queued worker.
- Appends a `workflow_requeued` event to workflow metadata for
  `GET /workflows/{id}/events`.

Response status: `202 Accepted`.

Response:

```json
{
  "id": "wf-review-1",
  "status": "pending",
  "run_mode": "queued",
  "input_ref": "artifact:wf-review-1:input"
}
```

Requeueing `pending`, `running`, `waiting_approval`, or `succeeded` returns
`400 invalid_request`. Missing workflow response: `404 not_found`.

## POST /workflows/{id}/agent-approve

Resolves a tool call paused by an agent whose task profile requested tools.
This is distinct from final workflow approval: an allowed tool resumes the
agent, writes its output, then leaves the workflow in `waiting_approval` for
the normal `POST /workflows/{id}/approve` decision.

The paused workflow response exposes only safe metadata:

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

Checkpoint plaintext, tool JSON input, prompt content, and bearer tokens are
never returned. The checkpoint is encrypted in `agent-runs.db`; on a real local
macOS run the data key is lazily created and kept only in Keychain. Default unit
and integration tests inject a cipher and do not access a machine Keychain. The
real-process test selected by the `hostapisystemsmoke` build tag is the
exception and uses a test-only Keychain item. Production defaults to Keychain
service `goagents.host-api.approvals` and key ID `local-v1`. Hosts may set
`HOST_API_AGENT_APPROVAL_KEYCHAIN_SERVICE` and
`HOST_API_AGENT_APPROVAL_KEY_ID` together to select another host-owned item;
these environment variables are lookup identifiers, never key material. When
both raw environment values are exactly empty (`""`), whether unset or
explicitly set empty, the production defaults apply. In every other case, both
trimmed values must be non-empty: partial configuration or any whitespace-only
value, including two whitespace-only values, fails startup. There is still no
file, environment-variable, or SQLite key fallback.

Authentication is `Authorization: Bearer <OIDC JWT>` and uses the same verified
`sub` identity rule as final workflow approval. The request accepts only exact
tool call identities and an allow/deny decision:

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

Callers cannot send checkpoint plaintext, tenant, definition hash, approver
identity, lease values, or a free-form approval reason. For an allowed batch,
the host requires an exact match before any tool runs. A mismatched decision is
terminally failed and returns `400 invalid_request`; the tool does not run. A
denied decision records the verified operator, never decrypts or executes the
checkpoint, and cancels the workflow. Missing or invalid tokens return
`401 unauthorized` without changing the checkpoint.

If another request has already leased the same checkpoint, the competing
request returns `409 approval_conflict`. It neither executes a tool nor changes
the workflow or agent run; the request that owns the lease continues processing
the approval.

If an allowed approval succeeded but its response was lost, retrying the exact
same safe tool identities with a valid bearer token returns the existing
`waiting_approval` workflow response and does not execute the tool again.
Changed, missing, denied, or otherwise mismatched resolutions remain
`400 invalid_request`; completed approval metadata is never exposed in the
workflow response.

Agent tool approvals expire one hour after their pause. The local host janitor
calls the checkpoint expiry operation every minute by default; set
`HOST_API_AGENT_APPROVAL_SWEEP_INTERVAL` to a positive Go duration to change
that cadence. Once expired, a checkpoint cannot be leased or replayed. The
matching agent run and workflow are marked `failed` with
`agent tool approval expired`; no tool executes. If writing either terminal
record temporarily fails, the workflow retains its safe pending metadata and a
later sweep retries reconciliation.

## POST /workflows/{id}/approve

Approves and finalizes a waiting workflow.

Authentication: `Authorization: Bearer <OIDC JWT>`. The host verifies the
issuer, audience, signature, and expiry through OIDC discovery/JWKS, then
records the verified `sub` claim as the approver. Tokens are never stored.

Request:

```json
{
  "note": "accepted"
}
```

Response status: `200 OK`.

Missing, malformed, expired, wrong-issuer, or wrong-audience tokens return
`401 unauthorized`. `approved_by` is not an accepted request field, so callers
cannot supply or override the audited identity.

Response:

```json
{
  "id": "wf-review-1",
  "status": "succeeded",
  "run_mode": "sync",
  "output_ref": "artifact:wf-review-1:final",
  "audit_ref": "audit:wf-review-1:approval",
  "completed_steps": ["ingest", "agent_review", "finalize"]
}
```

## GET /workflows/{id}/llm-routes

Returns sanitized llmkit route audit records for the workflow.

Response status: `200 OK`.

Response:

```json
{
  "workflow_id": "wf-review-1",
  "routes": [
    {
      "route_id": "route:wf-review-1:1",
      "task_id": "wf-review-1",
      "attempt": 1,
      "task_type": "high_success",
      "task_profile": {
        "task_type": "high_success",
        "complexity": "hard",
        "latency": "normal",
        "failure_cost": "high",
        "privacy": "cloud_allowed",
        "needs_reasoning": true
      },
      "account_alias": "cloud-prod",
      "model_alias": "cloud-advanced",
      "provider": "openai",
      "selected": true,
      "reason": "selected cloud-advanced with score 73 (...)",
      "score": 73,
      "score_breakdown": {
        "capability": 54,
        "price": 0,
        "local": 0,
        "latency": 5,
        "reliability": 0,
        "health": 2
      },
      "candidate_model_aliases": ["local-free", "cloud-advanced"],
      "candidates": [
        {
          "alias": "local-free",
          "account_alias": "local-dev",
          "available": false,
          "reason": "model does not match task requirements"
        },
        {
          "alias": "cloud-advanced",
          "account_alias": "cloud-prod",
          "available": true,
          "score": 73,
          "score_breakdown": {
            "capability": 54,
            "price": 0,
            "local": 0,
            "latency": 5,
            "reliability": 0,
            "health": 2
          },
          "reason": "selected cloud-advanced with score 73 (...)"
        }
      ],
      "outcome": {
        "success": true,
        "latency_ms": 1200,
        "input_tokens": 11,
        "output_tokens": 13,
        "estimated_cents": 0,
        "business_outcome": "success",
        "success_signal": "human_accepted"
      }
    }
  ]
}
```

The endpoint does not return prompts, responses, headers, or API keys.
`candidate_model_aliases` is a compact compatibility field. `candidates`
contains the full candidate-level explanation, including filtered candidates
with `available: false` and their rejection reason.
Failed provider outcomes may also include `error_class`, such as `timeout` or
`rate_limited`, when the host/adapter provides an error classifier.

## GET /agent-runs/{id}

Returns durable goagent run audit state and events.

Response status: `200 OK`.

Response:

```json
{
  "run_id": "00000000-0000-0000-0000-000000000000",
  "workflow_id": "wf-review-1",
  "task_id": "wf-review-1",
  "status": "succeeded",
  "summary": {
    "Status": "succeeded",
    "ContentRef": "artifact:wf-review-1:agent-output",
    "InputTokens": 11,
    "OutputTokens": 13,
    "LLMCalls": 1,
    "ToolCalls": 0
  },
  "events": []
}
```

Missing run response: `404 not_found`.

## GET /llmkit/models

Returns current candidates, in-memory provider health, and generated model
stats.

Response status: `200 OK`.

Response:

```json
{
  "models": [
    {
      "alias": "local-free",
      "provider": "local",
      "account_alias": "local-dev",
      "is_local": true,
      "price_class": "free"
    }
  ],
  "health": {
    "generated_at": "2026-05-06T00:00:00Z",
    "entries": {
      "local-dev|local-free|local": {
        "account_alias": "local-dev",
        "model_alias": "local-free",
        "provider": "local",
        "availability": "available"
      }
    }
  },
  "stats": [
    {
      "task_type": "simple_local",
      "account_alias": "local-dev",
      "model_alias": "local-free",
      "provider": "local",
      "route_attempts": 10,
      "outcome_count": 10,
      "pending_outcomes": 0,
      "successes": 8,
      "failures": 2,
      "success_rate": 0.8,
      "failure_rate": 0.2,
      "avg_latency_ms": 320,
      "avg_input_tokens": 100,
      "avg_output_tokens": 20,
      "avg_estimated_cents": 0
    }
  ]
}
```

`stats` is generated from llmkit audit files when the endpoint is served.
Host-api also refreshes stats before route decisions, so long-running processes
can use newly written outcomes.

## GET /workers/queued

Returns process-local observability for the in-process queued worker loop.

Response:

```json
{
  "started": true,
  "worker_id": "host-api-inprocess-worker",
  "claim_attempts": 3,
  "claimed": 1,
  "completed": 1,
  "idle": 2,
  "errors": 0,
  "lease_extensions": 4,
  "heartbeat_errors": 0,
  "last_heartbeat_workflow_id": "wf-review-1",
  "last_workflow_id": "wf-review-1"
}
```

`last_error` and `last_error_workflow_id` are present only after a claim or
workflow execution error. `last_heartbeat_error` is present only after a lease
extension error. These counters reset when the host process restarts; they are
diagnostics, not durable metrics.

## Current Non-Goals

- Fine-grained multi-tenant authorization.
- Distributed queued worker scheduling.
- Durable worker metrics.
- Server-sent events or live workflow updates.
- Distributed provider health.
- Project-wide or account-wide cost budgets.

Project/account budget governance is a host concern. See
`docs/llmkit-budget-governance.md` for the boundary.
