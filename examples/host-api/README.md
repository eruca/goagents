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
- `GET /agent-runs/{id}`
- `GET /llmkit/models`

Run it:

```bash
go run .
```

Set `HOST_API_ADDR` to choose the listen address and `LLMKIT_HOME` to choose the
audit directory. If `LLMKIT_HOME` is unset, it defaults to
`$HOST_RUNTIME_HOME/.llmkit`.
