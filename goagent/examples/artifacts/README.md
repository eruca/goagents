# Artifact Example

This example shows a common host pattern for large tool results:

- one tool performs a bounded query and stores the full result in a host artifact store;
- the tool returns a compact `ForLLM` preview plus `Ref` and `Metadata`;
- a second read-only tool reads a bounded artifact slice by `ref`;
- runtime events expose the lightweight result ref without copying the full artifact.

Run it with:

```bash
go run ./examples/artifacts
```

Expected output includes:

```text
event=tool.completed metadata=index=0,ref=artifact:query-1,tool=query_accounts
Final answer: active accounts were found.
artifact artifact:query-1 rows=3
```
