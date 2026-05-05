# Host Runtime Example

This example is a minimal host-side runtime skeleton. It is intentionally not a
new core framework. It shows how an application can compose existing modules:

- `workflowkit` owns the application workflow lifecycle.
- `goagent` owns the single ReAct-style agent run.
- `llmkit` owns LLM model/account routing and route/outcome audit files.
- Host-owned stores keep artifacts and agent events behind refs.

The example workflow is:

```text
Task input -> ArtifactStore -> workflow ingest step -> llmkit-routed goagent run
-> agent output artifact -> waiting_approval -> approval audit ref
-> final artifact
```

Run it:

```bash
go run .
```

Set `LLMKIT_HOME` to choose where route audit files are written:

```bash
LLMKIT_HOME=/tmp/host-runtime-llmkit go run .
```

The runtime writes `route-events.jsonl` and `outcomes.jsonl` under
`LLMKIT_HOME`, while artifacts and agent events stay in process-local memory for
this skeleton.

This example deliberately leaves production concerns to the host application:
HTTP APIs, durable database schemas, artifact blob storage, authentication,
tenant isolation, distributed workers, and dashboards.
