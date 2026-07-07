# MCPKit Official SDK Adapter

`officialsdk` adapts the official `github.com/modelcontextprotocol/go-sdk`
client session into the transport-neutral `mcpkit.Client` interface.

It supports stdio MCP servers through the SDK's `mcp.CommandTransport` and
Streamable HTTP MCP servers through `mcp.StreamableClientTransport`. It
intentionally does not expose resources, prompts, or server implementation
helpers.

## Use

```go
client, err := officialsdk.ConnectStdio(ctx, officialsdk.StdioConfig{
    Command: "node",
    Args: []string{"./server.js"},
    Name: "goagents-host",
    Version: "v0.1.0",
})
if err != nil {
    return err
}
defer client.Close()

_, err = mcpkit.RegisterTools(ctx, registry, client, mcpkit.RegisterOptions{
    MaxLLMChars: 2000,
})
```

For Streamable HTTP:

```go
client, err := officialsdk.ConnectStreamableHTTP(ctx, officialsdk.StreamableHTTPConfig{
    Endpoint: "https://example.com/mcp",
    Name: "goagents-host",
    Version: "v0.1.0",
})
if err != nil {
    return err
}
defer client.Close()
```

## Verify

```bash
go test ./...
go run ./examples/stdio-smoke
go run ./examples/http-smoke
```
