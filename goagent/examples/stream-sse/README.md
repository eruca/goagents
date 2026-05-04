# Stream SSE Example

This example shows how a host application can adapt `Agent.Stream` to
Server-Sent Events without adding transport code to `agentcore`.

Run a one-shot smoke check:

```bash
go run ./examples/stream-sse --once
```

Start a local SSE endpoint:

```bash
go run ./examples/stream-sse
curl -N http://localhost:8080/runs/stream
```

The endpoint streams selected runtime frames and one terminal `done` frame. This
host adapter forwards approval events, tool events, and finalization while
filtering lower-level stage lifecycle noise.

The payload intentionally includes only small facts such as event type, stage,
tool name, artifact ref, approval reason, final content, and execution summary.
Raw prompts, raw tool inputs, and full tool outputs remain host-owned concerns.
