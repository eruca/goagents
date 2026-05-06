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
  - alias: cloud-advanced
    model: advanced-model
    provider: openai_compatible
    account_alias: cloud-prod
    capability_level: advanced
    context_window_class: long
    price_class: high
    latency_class: normal
```

On startup, host-api refreshes `model-stats.json` from llmkit audit files and
passes those stats to the goagent adapter. That makes historical failures,
latency, token usage, and estimated cost visible in `/llmkit/models` and able
to influence future routing decisions.

## Endpoints

Endpoints:

- `POST /workflows`
- `GET /workflows/{id}`
- `POST /workflows/{id}/approve`
- `GET /workflows/{id}/llm-routes`
- `GET /agent-runs/{id}`
- `GET /llmkit/models`

`GET /workflows/{id}/llm-routes` returns the sanitized llmkit routing audit for
that workflow: effective task profile, selected model/account aliases, provider,
reason, score breakdown, candidate aliases, and outcome metadata such as
success, latency, tokens, and estimated cents. It does not return prompts,
responses, headers, or API keys.

`POST /workflows` accepts optional `run_mode`. The current example implements
only `sync`, which is also the default. `queued` is reserved for a future worker
model and currently returns `unsupported_run_mode`.

`POST /workflows` also accepts optional `task_profile_preset` and
`task_profile` so a host can describe the task before routing:

```json
{
  "id": "wf-review-1",
  "input": "Review this high-risk policy.",
  "task_profile_preset": "high_success",
  "task_profile": {
    "task_type": "policy_review",
    "needs_reasoning": true
  }
}
```

Available presets:

- `simple_local`: simple, low failure cost, local-preferred.
- `balanced`: medium complexity, medium failure cost, cloud allowed.
- `high_success`: hard, high failure cost, cloud allowed, reasoning required.
- `local_only`: simple, low failure cost, local-only.

If both fields are present, `task_profile_preset` provides the base profile and
`task_profile` overrides it. String fields override only when non-empty.
Boolean fields currently use Go zero values, so an omitted boolean inside a
present `task_profile` object is treated as `false`. The default profile
remains simple, low failure cost, and local-preferred. Invalid or unroutable
profiles return `invalid_task_profile`; for example, `local_only` plus
`complexity: hard` fails when no local advanced model exists. The routing
decision is visible through `GET /workflows/{id}/llm-routes`.

`GET /llmkit/models` returns:

- `models`: current routable model aliases and coarse capability metadata.
- `health`: in-memory provider health snapshot.
- `stats`: generated model statistics grouped by task type, account, model,
  and provider.

See `../../docs/host-api-contract.md` for request and response fields.

Run it:

```bash
go run .
```

Set `HOST_API_ADDR` to choose the listen address and `LLMKIT_HOME` to choose the
audit directory. If `LLMKIT_HOME` is unset, it defaults to
`$HOST_RUNTIME_HOME/.llmkit`.
