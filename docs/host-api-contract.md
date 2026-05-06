# Host API contract

This document captures the current HTTP contract of `examples/host-api`. The
example is a host-side composition surface, not a new core module.

## Runtime

Environment:

- `HOST_API_ADDR`: listen address. If unset, the example uses its default from
  `main.go`: `127.0.0.1:8080`.
- `HOST_RUNTIME_HOME`: durable runtime directory. If unset, host-api creates a
  temporary directory.
- `LLMKIT_HOME`: llmkit config and audit directory. If unset, it defaults to
  `$HOST_RUNTIME_HOME/.llmkit`.

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
- `run_mode`: optional. `sync` is supported and default. `queued` is reserved
  and currently returns `unsupported_run_mode`.
- `task_profile_preset`: optional. Supported values are `simple_local`,
  `balanced`, `high_success`, and `local_only`.
- `task_profile`: optional host override. Preset values are applied first.
  Non-empty string fields override the preset. Boolean fields are decoded with
  Go zero values, so an omitted boolean in a present `task_profile` object is
  currently treated as `false`.

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

## GET /workflows/{id}

Returns the stored workflow state.

Response status: `200 OK`.

Response shape is the same workflow response used by `POST /workflows`.

Missing workflow response: `404 not_found`.

## POST /workflows/{id}/approve

Approves and finalizes a waiting workflow.

Request:

```json
{
  "approved_by": "operator",
  "note": "accepted"
}
```

Response status: `200 OK`.

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
        "estimated_cents": 0
      }
    }
  ]
}
```

The endpoint does not return prompts, responses, headers, or API keys.
`candidate_model_aliases` is a compact compatibility field. `candidates`
contains the full candidate-level explanation, including filtered candidates
with `available: false` and their rejection reason.

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

`stats` is generated from llmkit audit files at server startup. During a long
running process, new outcomes are still written to `outcomes.jsonl`, but the
in-memory `stats` response and history-aware routing are refreshed only when
the host refreshes/restarts the server in the current example.

## Current Non-Goals

- Authentication and multi-tenant authorization.
- Queued worker execution.
- Server-sent events or live workflow updates.
- Distributed provider health.
- Hard cost budgets.
- Business outcome ingestion beyond provider-level outcome records.
