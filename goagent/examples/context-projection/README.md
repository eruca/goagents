# Context Projection Example

This example shows the adapter shape used to connect a host-side compressor to
`goagent.WithContextProjector`.

The example keeps the compressor deterministic and local so it can run in the
normal verification suite. In a host application, the same adapter boundary can
map `agentcore.Message` values into `contextkit.Message`, call a `contextkit`
compressor, and map the projected messages back into `agentcore.Message`.

Run it with:

```bash
go run ./examples/context-projection
```

Expected output:

```text
model saw 3 messages
Final answer: projected context used.
```
