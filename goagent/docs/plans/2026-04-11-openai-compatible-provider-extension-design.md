# OpenAI-Compatible Provider Extension Design

> Date: 2026-04-11
> Goal: Add the first real LLM provider extension for services that implement the OpenAI-compatible Chat Completions API, without binding the framework to OpenAI's hosted service.

## Context

The framework now has a stable deterministic core: staged ReAct execution, prompt assembly, tools, policy, memory, events, and runnable examples. The remaining gap is that all LLM behavior is still mock-driven. A real provider is the next useful validation point because it tests whether the current `ports.LLMClient` contract can map to production model APIs without moving provider-specific code into `agentcore`.

The target is not OpenAI's hosted service. The target is the widely used OpenAI-compatible HTTP contract implemented by model gateways, self-hosted inference servers, local runtimes, and commercial model providers. In practice, the broadest compatibility target is the Chat Completions API shape:

```text
POST <base_url>/chat/completions
```

Many compatible services do not implement OpenAI's newer Responses API. For this framework, compatibility matters more than following the newest official OpenAI endpoint. The provider should therefore be a protocol adapter for OpenAI-compatible Chat Completions, not an OpenAI account integration.

## Approaches Considered

### 1. Responses-Compatible Provider First

This matches OpenAI's newer official API direction and has explicit function-call output items, but it excludes many OpenAI-compatible services that only implement `/v1/chat/completions`. It is a poor first target for "any OpenAI-compatible service".

### 2. Chat-Completions-Compatible Provider First

This maps to the widest deployed OpenAI-compatible surface. It supports messages, tool definitions, assistant tool calls, tool result messages, and token usage. This is the recommended first target.

### 3. Dual Protocol Provider Immediately

A single provider could support both Chat Completions and Responses with a protocol mode. That would be more complete but would double request/response mapping and test surface before the first real-provider integration proves the extension boundary.

## Decision

Implement an OpenAI-compatible Chat Completions provider first.

The first implementation should:

- Add call IDs to `ports.ToolCall` so providers can preserve native tool-call identity.
- Add optional tool-call metadata to messages so tool-call turns and tool outputs can round-trip through compatible APIs.
- Add a serializable JSON Schema field to `ports.ToolSchema` while preserving the existing `Validate func` path for local executor validation.
- Add `extensions/providers/openaiapi` implementing `ports.LLMClient`.
- Require `BaseURL` and `Model` so callers explicitly choose a compatible service and model.
- Treat `APIKey` as optional; send `Authorization: Bearer ...` only when configured.
- Keep streaming, retries, provider-specific model options, Responses API, built-in tools, MCP tools, images, audio, files, prompt templates, and provider-managed conversation state out of the first provider slice.

## Provider API Shape

Create package:

```text
extensions/providers/openaiapi
```

Expose a small explicit client:

```go
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
	Headers    map[string]string
}

type Client struct { ... }

func New(config Config) (*Client, error)
func (c *Client) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error)
```

`BaseURL` and `Model` are required. `BaseURL` should be the service root such as `http://localhost:11434/v1`, `http://localhost:8000/v1`, or an API gateway URL. The client appends `/chat/completions` unless the base URL already ends with that path.

`APIKey` is optional because many local OpenAI-compatible services do not require authentication. `Headers` lets callers pass provider-specific routing or tenancy headers without adding those concepts to `agentcore`.

## Port Contract Updates

Extend existing DTOs without changing existing call sites:

```go
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type ChatMessage struct {
	Role       string
	Content    string
	ToolCallID string
	ToolCalls  []ToolCall
}

type ToolSchema struct {
	JSONSchema json.RawMessage
	Validate   func(json.RawMessage) error
}
```

`ToolSchema.ValidateInput` keeps the existing validation behavior. Providers use `JSONSchema` to build native function tool definitions. If a tool has no schema, the compatible provider should send a permissive object schema with no required fields rather than inventing validation behavior.

## Runtime Flow

The Agent flow stays the same at a high level:

```text
PromptStage -> ThinkStage -> PolicyStage -> ActStage -> ObserveStage -> next ThinkStage
```

The transcript needs one provider-ready addition:

1. When `ThinkStage` receives tool calls, append an assistant message containing those tool calls.
2. `PolicyStage` and `ActStage` use the same pending calls, including provider call IDs.
3. `ObserveStage` appends tool messages with `ToolCallID` copied from the executed call.
4. `ThinkStage` sends the full provider-ready transcript on the next turn.

This lets an OpenAI-compatible Chat Completions provider map:

- system/user/assistant text messages to chat messages
- assistant tool-call messages to assistant messages with `tool_calls`
- tool observation messages to `role: "tool"` messages with `tool_call_id`

## Compatible API Mapping

Request mapping:

- `ports.ChatRequest.Messages` becomes `messages`.
- `ports.ToolSpec` becomes `tools` entries with `type: "function"` and nested `function` metadata.
- `ports.ToolSchema.JSONSchema` becomes `function.parameters`.
- assistant messages with `ToolCalls` become messages with `tool_calls`.
- tool messages use `tool_call_id`.
- `model` comes from config.
- `stream` is omitted or set to false; streaming is out of scope.

Response mapping:

- `choices[0].message.content` becomes `ports.ChatResponse.Content`.
- `choices[0].message.tool_calls` becomes `ports.ToolCall{ID, Name, Input}`.
- `usage.prompt_tokens` maps to `Usage.InputTokens`.
- `usage.completion_tokens` maps to `Usage.OutputTokens`.
- HTTP/API errors return Go errors and abort the current Agent run.

## Error Handling

Provider construction should fail fast when `BaseURL` or `Model` is missing.

HTTP failures, non-2xx responses, malformed JSON, missing tool call names, invalid tool call arguments, and missing tool call IDs should return errors from `Client.Chat`. Tool argument validation remains the executor's responsibility through `ToolSchema.ValidateInput`.

If the model returns both text and tool calls, the core should preserve the text on the assistant tool-call message but continue the tool loop. Finalization still requires content with no pending tool calls.

## Testing

Use deterministic tests only. Do not call any real model service in automated tests.

Test the core contract:

- tool call IDs returned by an LLM are preserved through `PolicyStage`, `ActStage`, and `ObserveStage`
- assistant tool-call messages are included in the next LLM request
- tool observation messages include `ToolCallID`
- existing memory behavior remains stable and does not save provider-only assistant tool-call messages unless explicitly designed

Test the provider package with `httptest.Server`:

- builds a Chat Completions request with messages, function tools, schemas, and optional authorization
- does not send authorization when `APIKey` is empty
- parses a plain text response
- parses one or more tool calls with IDs
- maps token usage
- returns clear errors for missing config, non-2xx responses, malformed responses, and tool calls without IDs

Add one example:

```text
examples/openai-compatible
```

The example should require `OPENAI_COMPAT_BASE_URL` and `OPENAI_COMPAT_MODEL`. `OPENAI_COMPAT_API_KEY` is optional. If required values are missing, print a short skip message and exit successfully. This keeps local verification deterministic and avoids defaulting to OpenAI's hosted service.

## Non-Goals

- No default URL for OpenAI's hosted service.
- No Responses API in the first implementation.
- No streaming.
- No provider-managed conversation state.
- No OpenAI built-in tools.
- No MCP tools.
- No image, audio, or file input.
- No retry, backoff, or rate-limit framework.
- No budget or compaction implementation.
- No provider-specific code in `agentcore`.
- No durable storage or HTTP/SSE transport.

## Success Criteria

- `agentcore` remains provider-neutral.
- `extensions/providers/openaiapi` implements only `ports.LLMClient`.
- Any service matching the documented OpenAI-compatible Chat Completions request/response contract can be used by setting `BaseURL` and `Model`.
- Native tool calling works through provider-neutral tool call IDs and JSON Schema.
- Existing examples and tests continue to pass.
- Provider tests do not require network access or secrets.
- README documents the compatible-provider boundary and example.
