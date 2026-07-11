# RunKit

## Approval checkpoints

`CheckpointStore` persists the host-encrypted state needed to resume an Agent
after human approval. `runkit` treats `ApprovalCheckpoint.Ciphertext` as opaque:
the host encrypts before storing, decrypts only after a successful lease, and
owns keys and caller authorization.

`pgstore` provides PostgreSQL persistence. `ApproveAndLease` binds the tenant
and agent definition hash, records the approving operator/audit reference, and
atomically moves one checkpoint from `pending` to `leased`. A lease can finish
as `consumed`, `failed`, `rejected`, or `expired`; failed or expired leases are
not retried automatically because an external write may already have occurred.
Failed leases retain a bounded failure code for operator audit without storing
the decrypted checkpoint or a raw runtime error.

Set `RUNKIT_POSTGRES_TEST_DSN` to run the PostgreSQL integration tests. Point
it at a disposable database: migration creates checkpoint tables in that
database.

## Goagent approval adapter

`goagentapproval` is the host-side bridge from a paused
`agentcore.RunCheckpoint` to `CheckpointStore`. The host still authenticates
the operator and supplies the encryption implementation; the adapter never
owns keys or authorizes a caller.

```go
adapter, err := goagentapproval.New(checkpointStore, hostCipher, agent)
if err != nil {
    return err
}

// After agent.RunDetailed returns agentcore.ErrApprovalPending:
err = adapter.SavePending(ctx, goagentapproval.PendingCheckpoint{
    ID:             checkpointID,
    TenantID:       tenantID,
    DefinitionHash: agentDefinitionHash,
    ExpiresAt:      time.Now().Add(time.Hour),
    Checkpoint:     result.Interruption.Checkpoint,
})
```

After authenticating an operator, call `ApproveAndResume` with the approved
tool decisions. Every resume reserves a host-generated next checkpoint ID and
expiry. If the Agent pauses again, the adapter encrypts and writes that new
checkpoint before it consumes the old lease. If writing the new checkpoint,
decrypting state, or resuming fails, the old lease becomes terminally
`failed`; it is never automatically replayed.

`Reject` only records a rejection. It never decrypts or executes the paused
Agent state.

## Local macOS encryption

For a local macOS host, put the active data key in Keychain and use the
versioned AES-256-GCM cipher:

```go
keys, err := approvalcrypto.OpenMacOSKeychainKeyProvider(
    "goagents.host-api.approvals", "local-v1",
)
if err != nil {
    return err
}
cipher, err := approvalcrypto.NewAESGCMCipher(keys)
if err != nil {
    return err
}
```

The provider generates the active 32-byte key only on its first use and stores
it only in macOS Keychain. SQLite receives a versioned envelope with `key_id`,
nonce, and ciphertext. The goagent adapter authenticates checkpoint ID, tenant
ID, and definition hash as associated data, so a ciphertext cannot be moved to
another approval record. Rotate by choosing a new active key ID; retain the old
Keychain item until every checkpoint encrypted with it has expired or consumed.
There is intentionally no file or environment-variable fallback.

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
