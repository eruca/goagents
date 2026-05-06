# RunKit

`runkit` is a host-side run store contract for Go agent applications. It keeps
agent run records, lifecycle events, terminal summaries, and workflow/task
correlation separate from any one runtime.

The core `Store` contract is DTO-based:

- `RunRecord`: run id, workflow id, task id, status, metadata, timestamps.
- `RunEvent`: ordered lifecycle events with bounded metadata.
- `TerminalSummary`: content ref, token usage, call counts, tools, abort reason.

The core package includes `MemoryStore` for examples, tests, and prototypes.
The `sqlitestore` package provides SQLite persistence for host-side audit logs.

## Use

```go
store := runkit.NewMemoryStore()

err := store.Create(ctx, runkit.RunRecord{
    RunID:      "agent-run-1",
    WorkflowID: "wf-1",
    TaskID:     "task-1",
    Status:     runkit.StatusRunning,
})
if err != nil {
    return err
}

err = store.AppendEvent(ctx, runkit.RunEvent{
    RunID: "agent-run-1",
    Type:  "stage.started",
    Stage: "think",
})
```

For durable audit storage, open a SQLite store instead:

```go
store, err := sqlitestore.Open("goagents-runs.db")
if err != nil {
    return err
}
defer store.Close()
```

For `goagent`, use the adapter sink:

```go
sink := runkit.NewGoagentEventSink(store, func(event agentcore.Event) runkit.RunRecord {
    return runkit.RunRecord{
        RunID:      event.RunID.String(),
        WorkflowID: workflowID,
        TaskID:     taskID,
        Status:     runkit.StatusRunning,
    }
})

agent, err := agentcore.NewAgent(
    agentcore.WithLLM(llm),
    agentcore.WithEventSink(sink),
)
```

After the run completes, hosts should write a terminal summary:

```go
err = store.Complete(ctx, result.RunID.String(), runkit.TerminalSummary{
    Status:       runkit.StatusSucceeded,
    ContentRef:   "artifact:wf-1:agent-output",
    InputTokens:  result.Usage.InputTokens,
    OutputTokens: result.Usage.OutputTokens,
    LLMCalls:     result.ExecutionSummary.LLMCalls,
    ToolCalls:    result.ExecutionSummary.ToolCalls,
    UsedTools:    result.ExecutionSummary.UsedTools,
})
```

## Boundary

`runkit` stores refs and bounded metadata. It should not store raw prompts, full
model messages, full tool outputs, or large artifacts. Put those payloads in an
artifact store and reference them through `ContentRef` or host metadata.

## Verify

```bash
go test ./...
```
