# Audit Log Example

This example shows how a host application can wrap `Agent.Stream` and persist a
bounded JSONL audit trail without adding storage code to `agentcore`.

Run the deterministic example:

```bash
go run ./examples/audit-log --once
```

The output contains two record types:

- `run_event`: selected bounded runtime events such as approval, tool, and
  finalization events.
- `run_terminal`: the final run status, content preview, and execution summary.

The adapter allowlists event metadata keys such as `tool`, `ref`, `index`, and
`reason`. It intentionally does not write raw request input, raw tool input,
compiled prompts, model messages, or full tool output. Full payload retention is
a host policy that should live behind artifact refs.
