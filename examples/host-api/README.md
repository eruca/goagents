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

Endpoints:

- `POST /workflows`
- `GET /workflows/{id}`
- `POST /workflows/{id}/approve`
- `GET /workflows/{id}/llm-routes`
- `GET /agent-runs/{id}`
- `GET /llmkit/models`

`GET /workflows/{id}/llm-routes` returns the sanitized llmkit routing audit for
that workflow: selected model/account aliases, provider, reason, score
breakdown, candidate aliases, and outcome metadata such as success, latency,
tokens, and estimated cents. It does not return prompts, responses, headers, or
API keys.

`POST /workflows` accepts optional `run_mode`. The current example implements
only `sync`, which is also the default. `queued` is reserved for a future worker
model and currently returns `unsupported_run_mode`.

`POST /workflows` also accepts optional `task_profile` so a host can describe
the task before routing:

```json
{
  "id": "wf-review-1",
  "input": "Review this high-risk policy.",
  "task_profile": {
    "task_type": "policy_review",
    "complexity": "hard",
    "failure_cost": "high",
    "privacy": "cloud_allowed",
    "needs_reasoning": true
  }
}
```

The default profile remains simple, low failure cost, and local-preferred. That
keeps simple tasks on the local/free candidate while allowing complex,
high-risk tasks to route to the advanced cloud candidate. The routing decision
is visible through `GET /workflows/{id}/llm-routes`.

Run it:

```bash
go run .
```

Set `HOST_API_ADDR` to choose the listen address and `LLMKIT_HOME` to choose the
audit directory. If `LLMKIT_HOME` is unset, it defaults to
`$HOST_RUNTIME_HOME/.llmkit`.
