# EvalKit GoAgent Regression Example

This example shows the host-side glue between `evalkit` and `goagent`.

It uses a deterministic mock LLM, runs a real `agentcore.Agent` through an
`evalkit.Harness`, repeats the same task for two trials, and applies two graders:
one for answer shape and one for trace shape.

Run it from the workspace root:

```bash
go run ./examples/evalkit-goagent-regression
```

Verify the example contract:

```bash
go test ./examples/evalkit-goagent-regression
```
