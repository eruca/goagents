# MCP Client Adapter 选型设计

## 结论

第一版 MCP client adapter 应采用官方
`github.com/modelcontextprotocol/go-sdk@v1.6.1`，先实现 stdio client adapter，
暂不自研 JSON-RPC transport，也暂不实现 Streamable HTTP。

落地形态建议是新增一个可选 adapter module，例如：

```text
mcpkit/officialsdk
```

该 adapter 只负责把官方 SDK 的 `ClientSession` 包装成现有 `mcpkit.Client`：

```text
official SDK transport/session
  -> mcpkit.Client
    -> mcpkit.RegisterTools
      -> goagent/tools.Tool
```

`mcpkit` 继续保持 transport-neutral descriptor adapter，不直接依赖官方 SDK。

实现状态：

- `mcpkit/officialsdk` 已按该结论落地 stdio client adapter。
- `mcpkit/officialsdk/examples/stdio-smoke` 已提供 fake MCP server 的可运行证明。
- Streamable HTTP adapter 已追加落地，默认关闭 standalone SSE，只覆盖 request/response
  的 `tools/list` 和 `tools/call`。
- OAuth、远程生产认证、retry policy 和 SSE/session 生命周期仍需单独扩展设计。

## 调研基线

调研时间：2026-07-07。

可验证事实：

- 官方仓库 `modelcontextprotocol/go-sdk` 是 MCP 官方 Go SDK，并提供 `mcp`、
  `jsonrpc`、`auth` 等包。
- `go list -m -versions github.com/modelcontextprotocol/go-sdk` 显示当前稳定版本到
  `v1.6.1`，另有 `v1.7.0-pre.1` 预发布。
- `go list -m -json github.com/modelcontextprotocol/go-sdk@latest` 当前解析为
  `v1.6.1`，发布时间 `2026-05-22T11:30:38Z`，Go version 为 `1.25.0`。
- v1.6.1 中存在 `mcp.ClientSession.ListTools`、`mcp.ClientSession.CallTool`、
  `mcp.CommandTransport` 和 `mcp.StreamableClientTransport`。
- MCP 规范中的标准 transport 是 stdio 与 Streamable HTTP；HTTP+SSE 已是旧版兼容路径。

注意：检索时官方 README 已出现 `2026-07-28` spec/version 信息。按本设计的
当前日期 `2026-07-07`，这些未来日期内容不作为当前实现基线，只作为后续升级风险提示。

参考来源：

- Official Go SDK: https://github.com/modelcontextprotocol/go-sdk
- Go SDK protocol docs: https://github.com/modelcontextprotocol/go-sdk/blob/main/docs/protocol.md
- Go SDK package docs: https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp
- MCP transport spec: https://modelcontextprotocol.io/specification/2025-03-26/basic/transports
- Local module check: `go list -m -json github.com/modelcontextprotocol/go-sdk@latest`

## 为什么不自研 JSON-RPC transport

自研最小 JSON-RPC client 看起来短期代码量小，但会把这些协议职责推回本仓库：

- MCP lifecycle / initialize negotiation。
- pagination cursor。
- cancellation、ping、progress。
- stdio 进程生命周期和退出清理。
- Streamable HTTP 的 POST/GET、SSE、session id、重连和取消语义。
- OAuth 和 remote authorization 的扩展点。
- protocol version 兼容与未来 spec 演进。

这些不是 `goagent` 的核心价值。当前 repo 已经有清晰边界：`mcpkit` 负责把 MCP-style
tools 映射成 `goagent` tools；transport/session 应该交给专门 SDK 或 host adapter。

## 为什么第一版只做 stdio

stdio 是最小可验证路径：

- 本地 MCP server 进程由 `mcp.CommandTransport` 管理。
- 不需要 HTTP auth、Origin、防 DNS rebinding、session id 或 SSE reconnect。
- 可以直接用 mock MCP server command 做端到端测试。
- 足够验证 `tools/list` 和 `tools/call` 能进入 `goagent` tool registry。

Streamable HTTP 已作为第二步追加，但第一版只覆盖 request/response 工具调用。完整
远程生产使用仍然引入安全边界：

- endpoint、headers、session id、standalone SSE 的生命周期。
- OAuth/authorization 或 token 注入。
- HTTP client instrumentation、retry、timeout 与 proxy。
- 本地 HTTP MCP server 的 Origin/localhost 约束。

## 第一版范围

只做：

- `tools/list` 到 `[]mcpkit.ToolDescriptor`。
- `tools/call` 到 `*mcpkit.ToolCallResult`。
- stdio command transport。
- context cancellation。
- close/shutdown。
- output content 映射：text content、structured content、isError。
- pagination：使用 SDK 的 `ClientSession.Tools` iterator 或循环 `ListTools` cursor。
- 单元测试和一个可运行示例。

不做：

- resources。
- prompts。
- roots。
- sampling。
- elicitation。
- logging subscription。
- progress UI。
- OAuth。
- Streamable HTTP。
- server implementation。
- automatic tool refresh/watch。
- durable MCP session storage。

## 建议 API

新增 adapter module 不改变 `mcpkit.Client`：

```go
package officialsdk

type Client struct {
    // owns an MCP client session and maps SDK DTOs into mcpkit DTOs
}

type StdioConfig struct {
    Command string
    Args []string
    Env []string
    Dir string
    Name string
    Version string
    TerminateDuration time.Duration
}

func ConnectStdio(ctx context.Context, cfg StdioConfig) (*Client, error)
func (c *Client) ListTools(ctx context.Context) ([]mcpkit.ToolDescriptor, error)
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*mcpkit.ToolCallResult, error)
func (c *Client) Close() error
```

调用方示例：

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

## DTO 映射规则

Tool descriptor：

```text
mcp.Tool.Name          -> mcpkit.ToolDescriptor.Name
mcp.Tool.Description   -> mcpkit.ToolDescriptor.Description
mcp.Tool.InputSchema   -> mcpkit.ToolDescriptor.InputSchema
mcp.Tool.Annotations   -> mcpkit.ToolAnnotations
```

Tool call result：

```text
mcp.CallToolResult.Content           -> []mcpkit.ContentPart
mcp.CallToolResult.StructuredContent -> mcpkit.ToolCallResult.StructuredContent
mcp.CallToolResult.IsError           -> mcpkit.ToolCallResult.IsError
```

Text content 映射为 `ContentPart{Type: "text", Text: ...}`。非文本 content 第一版不把
binary 直接塞进模型上下文，只保留 type、MIME 和必要 metadata；完整 payload 应由 host
通过 artifact/ref 管理。

## 安全与运行边界

- stdio MCP server 的日志必须写 stderr；stdout 只能是 MCP JSON-RPC 消息。
- adapter 不自动授予权限；权限继续由 `mcpkit` annotations 和 `goagent` policy 决定。
- unknown permission 继续保持空权限，由默认 policy 拒绝。
- command、args、env、dir 都是 host 配置，不从模型输出生成。
- adapter 不保存 raw tool input/output；如需审计，由 host 使用 `runkit`/`artifactkit`。
- context cancel 必须传入 SDK call；关闭 adapter 时必须关闭 session/transport。

## 测试计划

第一版应包含：

- fake SDK/session adapter 单测：验证 descriptor/result 映射。
- stdio integration example：启动一个极小 MCP server command，注册一个 read-only tool。
- `mcpkit.RegisterTools` 联动测试：list 后能注册到 `goagent/tools.Registry`。
- `goagent` smoke：mock LLM 调用 MCP tool，最终拿到 tool observation。
- cancellation 测试：长时间 tool call 能被 context cancel 中断。
- close 测试：`Client.Close` 后子进程退出或被 terminate。

## 后续 HTTP adapter 决策点

第一版 Streamable HTTP adapter 已完成。后续增强仍需要单独设计：

- 是否继续用 request/response 模式，还是显式开启 standalone SSE。
- 是否需要注入自定义 `*http.Client`、OpenTelemetry、proxy、retry。
- auth 边界放 adapter config，还是 host 传入 OAuth handler。
- 是否需要 resumability/event store；如果需要，必须先设计持久化边界。

## 实施拆分

1. 已完成：新增 `mcpkit/officialsdk` module，依赖 `github.com/modelcontextprotocol/go-sdk@v1.6.1`。
2. 已完成：实现 `ConnectStdio` 和 SDK DTO -> `mcpkit` DTO 映射。
3. 已完成：增加 stdio fake server 示例和测试。
4. 已完成：把示例接入 `scripts/verify-all.sh`。
5. 已完成：追加 `ConnectStreamableHTTP`，默认关闭 standalone SSE。
6. 已完成：增加 Streamable HTTP fake server 示例并接入 `scripts/verify-all.sh`。
7. 后续：生产远程 HTTP/OAuth/SSE 生命周期单独设计，不和基础 adapter 混做。
