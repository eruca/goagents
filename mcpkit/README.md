# MCPKit

`mcpkit` contains a minimal adapter layer between Model Context Protocol style
tool descriptors and `goagent` tools.

It does not implement an MCP transport, server, authorization layer, resource
browser, or prompt registry. Hosts provide a `Client` that can list tools and
call one tool. `mcpkit` maps those tool descriptors into `goagent/tools.Tool`
instances with schema, permission, execution mode, timeout, and bounded result
projection.

## Use

```go
registry := tools.NewRegistry()

_, err := mcpkit.RegisterTools(ctx, registry, client, mcpkit.RegisterOptions{
    Permission:  policy.PermissionRead,
    MaxLLMChars: 1000,
})
if err != nil {
    return err
}

agent, err := agentcore.NewAgent(
    agentcore.WithLLM(llm),
    agentcore.WithToolRegistry(registry),
)
```

If `RegisterOptions.Permission` is empty, `mcpkit` uses MCP-style annotations:
`readOnlyHint` maps to `read`, `destructiveHint` maps to `write`, and tools
without either hint keep an empty permission so the default `goagent` policy
denies execution until the host explicitly allows it.

## Boundary

`mcpkit` owns:

- MCP-style tool descriptor DTOs
- tool call result DTOs
- mapping descriptors to `goagent` tool specs
- bounding model-visible MCP tool output

It does not own:

- JSON-RPC sessions
- stdio, HTTP, SSE, or Streamable HTTP transports
- MCP resources or prompts
- OAuth or remote authorization
- tool approval UI

Those concerns stay in host applications or future transport-specific adapters.

## Next Adapter Layer

The next production layer should be a real MCP client adapter for stdio and/or
HTTP transports. Do not add that inside the current descriptor adapter until the
project chooses one of these paths:

- adopt a maintained MCP Go SDK and keep `mcpkit` as the `goagent` mapping layer
- implement a small JSON-RPC client transport locally, with explicit lifecycle,
  cancellation, timeout, and auth boundaries

Until that decision is made, `mcpkit.Client` remains the stable seam for tests
and host-owned integrations.

## Verify

```bash
go test ./...
```
