# Host API Example

This example exposes the host-side runtime skeleton over HTTP. It is not a new
core module; it shows how a host application can compose:

- `workflowkit` for workflow lifecycle and approval resume.
- `goagent` for the agent run.
- `llmkit` for model routing, model stats, and provider health.
- `artifactkit` for payload refs.
- `runkit` for agent run audit records and events.

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
audit directory.
