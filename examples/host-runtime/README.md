# Host Runtime Example

This example is a minimal host-side runtime skeleton. It is intentionally not a
new core framework. It shows how an application can compose existing modules:

- `workflowkit` owns the application workflow lifecycle.
- `goagent` owns the single ReAct-style agent run.
- `llmkit` owns LLM model/account routing and route/outcome audit files.
- `artifactkit` keeps full payloads behind artifact refs.
- `runkit` keeps agent run records, lifecycle events, and terminal summaries.

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
`LLMKIT_HOME`, while `artifactkit.MemoryStore` and `runkit.MemoryStore` stay in
process-local memory for this skeleton. Production hosts can replace the agent
run store with `runkit/sqlitestore.Open("goagents-runs.db")` without changing
the workflow or agent contract.

The critical persistence path is strict: the agent review step writes the agent
output artifact before completing the terminal run summary. If either write
fails, the workflow step fails instead of advancing with refs that cannot be
resolved.

This example deliberately leaves production concerns to the host application:
HTTP APIs, durable database schemas, artifact blob storage, authentication,
tenant isolation, distributed workers, and dashboards.
