# Host API Example

This example exposes the host-side runtime skeleton over HTTP. It is not a new
core module; it shows how a host application can compose:

- `workflowkit` for workflow lifecycle and approval resume.
- `goagent` for the agent run.
- `llmkit` for model routing, model stats, and provider health.
- `artifactkit` for durable payload refs.
- `runkit` for durable agent run audit records and events.

By default, the example creates a temporary runtime directory. Set
`HOST_RUNTIME_HOME` to make workflow state, agent run audit, artifacts, and
llmkit audit survive process restarts:

```text
$HOST_RUNTIME_HOME/
  workflow.db
  agent-runs.db
  artifacts/
  .llmkit/
```

## LLM Configuration

If `$LLMKIT_HOME/config.yaml` exists, host-api loads candidates and
OpenAI-compatible providers from that file. If `LLMKIT_HOME` is unset, it
defaults to `$HOST_RUNTIME_HOME/.llmkit`.

If no `config.yaml` exists, the example falls back to deterministic demo models:

- `local-free`: local, free, simple, fast.
- `cloud-advanced`: cloud, high price, advanced, normal latency.

If `config.yaml` exists but references an unset `api_key_env`, startup fails.
This is intentional: configured providers should fail closed instead of making
unauthenticated provider calls.

Minimal config:

```yaml
accounts:
  - alias: local-dev
    provider: openai_compatible
    base_url: http://127.0.0.1:11434/v1
  - alias: cloud-prod
    provider: openai_compatible
    base_url: https://api.example.com/v1
    api_key_env: CLOUD_PROD_API_KEY

models:
  - alias: local-free
    model: qwen2.5:7b
    provider: openai_compatible
    account_alias: local-dev
    is_local: true
    capability_level: simple
    context_window_class: medium
    price_class: free
    latency_class: fast
    estimated_cents: 0
  - alias: cloud-advanced
    model: advanced-model
    provider: openai_compatible
    account_alias: cloud-prod
    capability_level: advanced
    context_window_class: long
    price_class: high
    latency_class: normal
    estimated_cents: 8
```

Host-api refreshes `model-stats.json` from llmkit audit files before route
decisions and when serving `/llmkit/models`. That makes new outcomes from a
long-running process visible without waiting for restart.

## Endpoints

Endpoints:

- `POST /workflows`
- `GET /workflows`
- `GET /workflows/{id}`
- `POST /workflows/{id}/requeue`
- `POST /workflows/{id}/approve`
- `GET /workflows/{id}/llm-routes`
- `GET /agent-runs/{id}`
- `GET /llmkit/models`
- `GET /workers/queued`

`GET /workflows/{id}/llm-routes` returns the sanitized llmkit routing audit for
that workflow: effective task profile, selected model/account aliases, provider,
reason, score breakdown, candidate aliases, full candidate-level scores or
filter reasons, and outcome metadata such as success, latency, tokens, and
estimated cents. It does not return prompts, responses, headers, or API keys.

`GET /workflows` returns a bounded operational list. It accepts optional
`status` and `limit` query parameters, for example
`GET /workflows?status=pending&limit=50`. Results are ordered by workflow
creation time, then workflow id.

`POST /workflows` accepts optional `run_mode`. `sync` is the default and runs
the workflow during the HTTP request. `queued` is an in-process proof: it writes
the input artifact and pending workflow, returns immediately, then a background
worker loop claims a `workflowkit.QueueLeaseStore` lease, advances the workflow
until `waiting_approval` or terminal, extends the lease while the workflow is
running, and releases the lease. The CLI starts the worker loop on boot, so
restarting with the same `HOST_RUNTIME_HOME` can recover pending or
expired-lease workflows. It is still not a distributed worker model; worker
crash supervision and multi-worker scheduling are intentionally not implemented.

`POST /workflows/{id}/requeue` is an explicit operator action for failed or
cancelled workflows. It moves the existing workflow back to `pending`, preserves
step history, records `run_mode: "queued"`, and lets the queued worker retry the
unfinished work. It does not create a new workflow id.

`GET /workers/queued` returns in-process worker observability: whether the
worker loop has been started, the worker id, claim/completion/idle/error counts,
lease extension and heartbeat error counts, the last workflow id, and the latest
error. These counters are process-local diagnostics, not durable metrics.

`POST /workflows` also accepts optional `task_profile_preset` and
`task_profile` so a host can describe the task before routing:

```json
{
  "id": "wf-review-1",
  "input": "Review this high-risk policy.",
  "task_profile_preset": "high_success",
  "task_profile": {
    "task_type": "policy_review",
    "needs_reasoning": true,
    "max_estimated_cents": 10
  }
}
```

Available presets:

- `simple_local`: simple, low failure cost, local-preferred.
- `balanced`: medium complexity, medium failure cost, cloud allowed.
- `high_success`: hard, high failure cost, cloud allowed, reasoning required.
- `local_only`: simple, low failure cost, local-only.

If both fields are present, `task_profile_preset` provides the base profile and
`task_profile` patches it. Missing fields inherit the preset/default profile.
Present string fields must be non-empty. Present boolean fields explicitly
override the base profile, including `false`. Invalid or unroutable profiles
return `invalid_task_profile`; for example, `local_only` plus `complexity: hard`
fails when no local advanced model exists. The routing decision is visible
through `GET /workflows/{id}/llm-routes`.

`GET /llmkit/models` returns:

- `models`: current routable model aliases and coarse capability metadata.
- `health`: in-memory provider health snapshot.
- `stats`: generated model statistics grouped by task type, account, model,
  and provider.

`POST /workflows/{id}/approve` records a business outcome signal on the
selected LLM route: `business_outcome=success` and
`success_signal=human_accepted`. Provider-level success remains available in the
same route outcome.

The agent review step uses strict host-owned persistence for the critical audit
path. If writing the agent output artifact or terminal run summary fails, the
workflow step fails instead of moving to `waiting_approval` with broken refs.

See `../../docs/host-api-contract.md` for the prose contract and
`openapi.yaml` for the machine-readable HTTP contract.

Run it:

```bash
go run .
```

Set `HOST_API_ADDR` to choose the listen address and `LLMKIT_HOME` to choose the
audit directory. If `LLMKIT_HOME` is unset, it defaults to
`$HOST_RUNTIME_HOME/.llmkit`. Set `HOST_API_QUEUED_LEASE_DURATION` to tune the
in-process queued worker lease duration; it accepts Go durations such as `30s`
or `2m` and defaults to `1m`.
