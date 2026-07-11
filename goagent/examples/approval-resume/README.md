# Durable Approval Resume Example

This example pauses a write tool before execution, serializes the returned
`RunCheckpoint`, rebuilds the Agent and its tool registry, then resumes with a
host resolution. It models the Agent boundary only: production hosts must store
the checkpoint with their own access control, encryption, and expiration.

Run it with:

```bash
go run ./examples/approval-resume
```

Expected output includes one paused tool call, one executed tool, and a final
answer after the resumed model request.
