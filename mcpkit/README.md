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

`RegisterOptions.Permission` always takes precedence. Without an explicit
permission, annotations from an MCP server are ignored by default and tools keep
an empty permission, so the default `goagent` policy denies execution.

Set `TrustServerAnnotations: true` only after the host has explicitly verified
the MCP server identity and configuration. In that trusted case, `readOnlyHint`
maps to `read` and `destructiveHint` maps to `write`. Annotations are hints for
host policy; they are not an authorization boundary.

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

Those concerns stay in host applications or transport-specific adapter modules.

## Transport Adapters

The first transport-specific adapter is `github.com/eruca/goagents/mcpkit/officialsdk`
in `mcpkit/officialsdk/`. It uses the official MCP Go SDK to connect to stdio
and Streamable HTTP MCP servers, then exposes the existing `mcpkit.Client`
interface.

Streamable HTTP defaults to request/response mode by disabling standalone SSE.
Enable SSE explicitly only when a host needs server-initiated notifications and
has designed the associated session lifecycle.

## Verify

```bash
go test ./...
```
