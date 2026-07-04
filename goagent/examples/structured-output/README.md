# Structured Output Example

This example shows both paths of the `agentcore.OutputFormat` JSON Schema
contract:

- a model response that validates and is copied into `RunResult.StructuredOutput`
- a model response that fails schema validation and aborts with
  `agentcore.ErrOutputInvalid`

Run it from the `goagent` module:

```bash
go run ./examples/structured-output
```

Verify the example contract:

```bash
go test ./examples/structured-output
```
