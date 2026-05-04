# Stream Example

This example uses `Agent.Stream` to consume in-process runtime events while an Agent run is executing.

The stream emits bounded lifecycle events, then one terminal event with the final result summary. It stays transport-neutral; hosts can adapt the same stream to a CLI, UI, log tail, SSE endpoint, or approval layer.

Run it with:

```bash
go run ./examples/stream
```

Expected output includes:

```text
stream=tool.completed tool=lookup_account ref=account:acct_1
stream=done llm=2 tools=1
Final answer: acct_1 is active.
```
