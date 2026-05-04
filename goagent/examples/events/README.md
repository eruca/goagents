# Events Example

This example wires an `EventSink` into a mock Agent run.

The sink prints compact runtime events for host-side logs, traces, metrics, or UI status. It keeps event payloads small and avoids raw prompts, raw tool input, and full tool output.

Run it with:

```bash
go run ./examples/events
```
