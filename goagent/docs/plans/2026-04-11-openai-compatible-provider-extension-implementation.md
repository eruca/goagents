# OpenAI-Compatible Provider Extension Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a provider-neutral tool-call transcript contract and a minimal OpenAI-compatible Chat Completions provider extension that implements `ports.LLMClient`.

**Architecture:** Keep `agentcore` provider-neutral. First add the small DTO fields needed for native tool calling: tool call IDs, assistant tool-call messages, tool output message IDs, and serializable JSON Schema. Then add `extensions/providers/openaiapi` using `net/http` and deterministic `httptest` tests. Do not add streaming, retries, provider-specific model options, Responses API, built-in tools, MCP tools, provider-managed conversation state, budget, compaction, or transport.

**Tech Stack:** Go standard library, existing `agentcore`, `ports`, `tools`, `prompt`, and `policy` packages, OpenAI-compatible Chat Completions API over HTTP, `go test`, `make verify`.

---

### Task 1: Add Provider-Ready DTO Fields

**Files:**
- Modify: `ports/llm.go`
- Test: `tools/middleware_test.go`

**Step 1: Write tests for additive schema behavior**

Add a focused test to `tools/middleware_test.go`:

```go
func TestToolSchemaKeepsJSONSchemaAndValidateBehavior(t *testing.T) {
	schema := Schema{
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		Validate: func(input json.RawMessage) error {
			if string(input) != `{"ok":true}` {
				return fmt.Errorf("invalid")
			}
			return nil
		},
	}
	if string(schema.JSONSchema) != `{"type":"object"}` {
		t.Fatalf("JSONSchema = %s", schema.JSONSchema)
	}
	if err := schema.ValidateInput(json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("ValidateInput returned error: %v", err)
	}
	if err := schema.ValidateInput(json.RawMessage(`{"ok":false}`)); err == nil {
		t.Fatal("ValidateInput returned nil error")
	}
}
```

`tools/middleware_test.go` already imports `encoding/json` and `fmt`.

**Step 2: Run test to verify it fails**

Run:

```bash
go test ./tools -run TestToolSchemaKeepsJSONSchemaAndValidateBehavior -v
```

Expected: FAIL because `ToolSchema.JSONSchema` does not exist.

**Step 3: Implement DTO fields**

Update `ports/llm.go`:

```go
type ChatMessage struct {
	Role       string
	Content    string
	ToolCallID string
	ToolCalls  []ToolCall
}
```

Update `ToolCall`:

```go
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}
```

Update `ToolSchema`:

```go
type ToolSchema struct {
	JSONSchema json.RawMessage
	Validate   func(json.RawMessage) error
}
```

Do not change `ValidateInput` behavior.

**Step 4: Run focused tests**

Run:

```bash
go test ./ports ./tools
```

Expected: PASS.

**Step 5: Commit**

```bash
git add ports/llm.go tools/middleware_test.go
git commit -m "feat: add provider-ready tool DTO fields"
```

### Task 2: Preserve Tool Call IDs Through ReAct Transcript

**Files:**
- Modify: `agentcore/request.go`
- Modify: `agentcore/think_stage.go`
- Modify: `agentcore/observe_stage.go`
- Modify: `agentcore/memory_stage.go`
- Test: `agentcore/react_test.go`
- Test: `agentcore/agent_test.go`

**Step 1: Write failing ReAct transcript test**

Add this test to `agentcore/react_test.go`:

```go
func TestReActPreservesToolCallIDInNextModelRequest(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{
			Content: "I will look it up.",
			ToolCalls: []ports.ToolCall{{
				ID:    "call_lookup_1",
				Name:  "lookup",
				Input: json.RawMessage(`{"q":"go"}`),
			}},
		},
		{Content: "done"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "observation"}, nil
		},
	})
	state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
	runner := NewReActRunner(ReActConfig{
		LLM:            llm,
		PromptCompiler: prompt.NewCompiler(),
		ToolRegistry:   registry,
		PolicyEngine:   policy.NewEngine(),
		MaxIterations:  3,
	})

	_, err := runner.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	second := llm.requests[1].Messages
	if len(second) < 3 {
		t.Fatalf("second request messages = %#v", second)
	}
	assistant := second[len(second)-2]
	if assistant.Role != "assistant" || assistant.Content != "I will look it up." {
		t.Fatalf("assistant tool-call message = %#v", assistant)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_lookup_1" {
		t.Fatalf("assistant tool calls = %#v", assistant.ToolCalls)
	}
	toolMessage := second[len(second)-1]
	if toolMessage.Role != "tool" || toolMessage.ToolCallID != "call_lookup_1" || toolMessage.Content != "observation" {
		t.Fatalf("tool message = %#v", toolMessage)
	}
}
```

Add a memory boundary assertion to an existing memory test or a new focused test in `agentcore/agent_test.go`: saved memory should not include assistant tool-call messages because those are provider transcript mechanics, not user-facing conversation memory.

**Step 2: Run test to verify it fails**

Run:

```bash
go test ./agentcore -run TestReActPreservesToolCallIDInNextModelRequest -v
```

Expected: FAIL because tool call IDs and assistant tool-call messages are not preserved.

**Step 3: Implement message metadata**

Update `agentcore/request.go`:

```go
import "github.com/eruca/goagent/ports"

type Message struct {
	Role       string
	Content    string
	ToolCallID string
	ToolCalls  []ports.ToolCall
}
```

Update `ThinkStage.Run`:

- copy `resp.ToolCalls` into `state.PendingCalls` preserving `ID`
- when `len(resp.ToolCalls) > 0`, append `Message{Role: "assistant", Content: resp.Content, ToolCalls: append([]ports.ToolCall(nil), resp.ToolCalls...)}` to `state.Messages`
- keep the existing final-answer append path for `resp.Content != "" && len(resp.ToolCalls) == 0`

Update `chatMessages` to copy `ToolCallID` and `ToolCalls` into `ports.ChatMessage`.

Update `ObserveStage.Run`:

```go
state.Messages = append(state.Messages, Message{
	Role:       "tool",
	Content:    content,
	ToolCallID: result.Call.ID,
})
```

Update `memoryMessages` in `agentcore/memory_stage.go` to skip messages where `len(message.ToolCalls) > 0`. Keep existing loaded memory conversion unchanged because `ports.MemoryMessage` intentionally remains simple role/content memory.

**Step 4: Run focused tests**

Run:

```bash
go test ./agentcore -run 'TestReActPreservesToolCallIDInNextModelRequest|TestAgentLoadsAndSavesSessionMemory|TestReActToolCallObservesThenReturnsFinalAnswer' -v
```

Expected: PASS.

**Step 5: Run package tests**

Run:

```bash
go test ./agentcore
```

Expected: PASS.

**Step 6: Commit**

```bash
git add agentcore/request.go agentcore/think_stage.go agentcore/observe_stage.go agentcore/memory_stage.go agentcore/react_test.go agentcore/agent_test.go
git commit -m "feat: preserve tool call transcript metadata"
```

### Task 3: Add Compatible Provider Request Mapping Tests

**Files:**
- Create: `extensions/providers/openaiapi/client.go`
- Create: `extensions/providers/openaiapi/client_test.go`

**Step 1: Write failing tests**

Create `extensions/providers/openaiapi/client_test.go` with tests that use `httptest.Server`.

Add `TestClientBuildsChatCompletionsRequest`:

- server records method, path, authorization header, and request JSON
- client is configured with `BaseURL: server.URL + "/v1"`, `APIKey: "test-key"`, `Model: "test-model"`
- call `client.Chat` with:
  - system message
  - user message
  - assistant message containing one `ToolCall{ID: "call_1", Name: "lookup", Input: json.RawMessage(`{"q":"go"}`)}`
  - tool message with `ToolCallID: "call_1"`
  - one tool spec with `Schema.JSONSchema`
- server returns a text response

Assert request JSON contains:

- `model: "test-model"`
- `stream` is absent or false
- `messages` includes `role: "assistant"` with `tool_calls[0].id: "call_1"`
- `messages` includes `role: "tool"` with `tool_call_id: "call_1"`
- `tools[0].type: "function"`
- `tools[0].function.name: "lookup"`
- `tools[0].function.parameters.type: "object"`
- `Authorization: Bearer test-key`

Add `TestClientOmitsAuthorizationWhenAPIKeyEmpty`.

Add `TestClientUsesDefaultObjectSchemaWhenToolSchemaMissing`:

- tool has no `Schema.JSONSchema`
- request should contain a parameters object equivalent to `{"type":"object","properties":{},"additionalProperties":true}`

**Step 2: Run tests to verify they fail**

Run:

```bash
go test ./extensions/providers/openaiapi -run 'TestClientBuildsChatCompletionsRequest|TestClientOmitsAuthorizationWhenAPIKeyEmpty|TestClientUsesDefaultObjectSchemaWhenToolSchemaMissing' -v
```

Expected: FAIL because package does not exist.

**Step 3: Implement minimal request mapping**

Create `extensions/providers/openaiapi/client.go` with:

```go
package openaiapi

type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
	Headers    map[string]string
}

type Client struct {
	config Config
	http   *http.Client
}
```

Implement:

- `New(config Config) (*Client, error)`
- `func (c *Client) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error)`
- private request/response DTO structs
- `buildMessages(messages []ports.ChatMessage) []chatMessage`
- `buildTools(specs []ports.ToolSpec) []chatTool`
- `chatCompletionsURL(baseURL string) (string, error)`

Use endpoint:

```text
POST <BaseURL>/chat/completions
```

If `BaseURL` already ends in `/chat/completions`, use it as-is.

Headers:

- `Content-Type: application/json`
- `Authorization: Bearer <APIKey>` only when `APIKey` is non-empty
- entries from `Headers`

Do not default `BaseURL` to an OpenAI hosted URL.

**Step 4: Run focused tests**

Run:

```bash
go test ./extensions/providers/openaiapi -run 'TestClientBuildsChatCompletionsRequest|TestClientOmitsAuthorizationWhenAPIKeyEmpty|TestClientUsesDefaultObjectSchemaWhenToolSchemaMissing' -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add extensions/providers/openaiapi/client.go extensions/providers/openaiapi/client_test.go
git commit -m "feat: add openai-compatible request mapping"
```

### Task 4: Parse Compatible Chat Completions Responses And Errors

**Files:**
- Modify: `extensions/providers/openaiapi/client.go`
- Modify: `extensions/providers/openaiapi/client_test.go`

**Step 1: Write failing response parsing tests**

Add tests:

- `TestClientParsesTextResponse`
- `TestClientParsesToolCallResponse`
- `TestClientMapsUsage`
- `TestClientRequiresBaseURLAndModel`
- `TestClientReturnsErrorForNon2xxResponse`
- `TestClientReturnsErrorForMalformedJSON`
- `TestClientReturnsErrorForToolCallWithoutID`

Use response shapes like:

```json
{
  "choices": [
    {
      "message": {
        "role": "assistant",
        "content": "hello"
      }
    }
  ],
  "usage": {"prompt_tokens": 3, "completion_tokens": 4}
}
```

and:

```json
{
  "choices": [
    {
      "message": {
        "role": "assistant",
        "tool_calls": [
          {
            "id": "call_1",
            "type": "function",
            "function": {
              "name": "lookup",
              "arguments": "{\"q\":\"go\"}"
            }
          }
        ]
      }
    }
  ]
}
```

**Step 2: Run tests to verify they fail**

Run:

```bash
go test ./extensions/providers/openaiapi -run 'TestClientParses|TestClientMapsUsage|TestClientRequires|TestClientReturns' -v
```

Expected: FAIL until parsing and error handling are implemented.

**Step 3: Implement response parsing**

Implement:

- parse `choices[0].message.content` into `ports.ChatResponse.Content`
- parse `choices[0].message.tool_calls` into `ports.ToolCall`
- require non-empty tool call ID
- preserve raw function `arguments` as `json.RawMessage`
- map usage to `ports.Usage`
- include response body excerpt in non-2xx errors without logging secrets

**Step 4: Run provider tests**

Run:

```bash
go test ./extensions/providers/openaiapi -v
```

Expected: PASS.

**Step 5: Commit**

```bash
git add extensions/providers/openaiapi/client.go extensions/providers/openaiapi/client_test.go
git commit -m "feat: parse openai-compatible responses"
```

### Task 5: Add Compatible Provider Example And Docs

**Files:**
- Create: `examples/openai-compatible/main.go`
- Create: `examples/openai-compatible/README.md`
- Modify: `README.md`

**Step 1: Add deterministic-skipping example**

Create `examples/openai-compatible/main.go`:

- read `OPENAI_COMPAT_BASE_URL` and `OPENAI_COMPAT_MODEL`
- read optional `OPENAI_COMPAT_API_KEY`
- if required values are missing, print `Skipping OpenAI-compatible example: set OPENAI_COMPAT_BASE_URL and OPENAI_COMPAT_MODEL to run it.` and return nil
- create `openaiapi.New(openaiapi.Config{BaseURL: baseURL, APIKey: apiKey, Model: model})`
- register one read-only tool with `Schema.JSONSchema` and local `Validate`
- run an Agent with one prompt block and the compatible client
- print only `result.Content`

Do not hardcode any hosted service URL or model fallback.

**Step 2: Add README**

Create `examples/openai-compatible/README.md` explaining:

- this example calls an OpenAI-compatible Chat Completions service selected by `OPENAI_COMPAT_BASE_URL`
- required and optional env vars
- it exits successfully when required env vars are missing
- tools still execute locally under policy after the model requests them

**Step 3: Update root README**

Add a short `Provider Extensions` section:

```markdown
## Provider Extensions

The core depends only on `ports.LLMClient`. Real model clients live outside `agentcore`.

`extensions/providers/openaiapi` implements `ports.LLMClient` for OpenAI-compatible Chat Completions services. It maps framework messages, tool specs, tool call IDs, and tool outputs to the compatible request/response shape while keeping provider-specific code out of the core.

See `examples/openai-compatible` for a real-provider example. The example requires `OPENAI_COMPAT_BASE_URL` and `OPENAI_COMPAT_MODEL`; `OPENAI_COMPAT_API_KEY` is optional. Without the required values it prints a skip message and exits successfully.
```

Also add `examples/openai-compatible` to the learning path after `examples/prompt`.

**Step 4: Run example without env vars**

Run:

```bash
env -u OPENAI_COMPAT_BASE_URL -u OPENAI_COMPAT_MODEL -u OPENAI_COMPAT_API_KEY go run ./examples/openai-compatible
```

Expected output:

```text
Skipping OpenAI-compatible example: set OPENAI_COMPAT_BASE_URL and OPENAI_COMPAT_MODEL to run it.
```

**Step 5: Run tests**

Run:

```bash
go test ./extensions/providers/openaiapi ./agentcore ./tools
```

Expected: PASS.

**Step 6: Commit**

```bash
git add examples/openai-compatible README.md extensions/providers/openaiapi
git commit -m "docs: add openai-compatible provider example"
```

### Task 6: Final Verification

**Files:**
- No code changes expected.

**Step 1: Run full verification**

Run:

```bash
make verify
env -u OPENAI_COMPAT_BASE_URL -u OPENAI_COMPAT_MODEL -u OPENAI_COMPAT_API_KEY go run ./examples/openai-compatible
go run ./examples/prompt
```

Expected: PASS. `make verify` should not require provider credentials or a running model service.

**Step 2: Confirm status**

Run:

```bash
git status --short
```

Expected: no output.

**Step 3: Summarize commits**

Run:

```bash
git log --oneline -n 8
```

Confirm the branch contains focused commits for DTOs, transcript metadata, request mapping, response parsing, example/docs, and verification.
