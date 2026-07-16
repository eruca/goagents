package openaiapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
)

func TestClientBuildsChatCompletionsRequest(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotAuth string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer server.Close()

	client, err := New(Config{
		BaseURL: server.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	_, err = client.Chat(context.Background(), ports.ChatRequest{
		Messages: []ports.ChatMessage{
			{Role: "system", Content: "system"},
			{Role: "user", Content: "hello"},
			{
				Role:    "assistant",
				Content: "I will look it up.",
				ToolCalls: []ports.ToolCall{{
					ID:    "call_1",
					Name:  "lookup",
					Input: json.RawMessage(`{"q":"go"}`),
				}},
			},
			{Role: "tool", Content: "observation", ToolCallID: "call_1"},
		},
		Tools: []ports.ToolSpec{{
			Name:        "lookup",
			Description: "Looks up facts.",
			Schema: ports.ToolSchema{
				JSONSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			},
		}},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q", gotMethod)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotBody["model"] != "test-model" {
		t.Fatalf("model = %#v", gotBody["model"])
	}
	if stream, ok := gotBody["stream"]; ok && stream != false {
		t.Fatalf("stream = %#v", stream)
	}

	messages := gotBody["messages"].([]any)
	assistant := messages[2].(map[string]any)
	toolCalls := assistant["tool_calls"].([]any)
	toolCall := toolCalls[0].(map[string]any)
	if toolCall["id"] != "call_1" || toolCall["type"] != "function" {
		t.Fatalf("tool call = %#v", toolCall)
	}
	function := toolCall["function"].(map[string]any)
	if function["name"] != "lookup" || function["arguments"] != `{"q":"go"}` {
		t.Fatalf("function = %#v", function)
	}
	toolMessage := messages[3].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "call_1" {
		t.Fatalf("tool message = %#v", toolMessage)
	}

	tools := gotBody["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("tool = %#v", tool)
	}
	toolFunction := tool["function"].(map[string]any)
	if toolFunction["name"] != "lookup" {
		t.Fatalf("tool function = %#v", toolFunction)
	}
	parameters := toolFunction["parameters"].(map[string]any)
	if parameters["type"] != "object" {
		t.Fatalf("parameters = %#v", parameters)
	}
}

func TestClientOmitsAuthorizationWhenAPIKeyEmpty(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("authorization = %q", gotAuth)
	}
}

func TestClientUsesDefaultObjectSchemaWhenToolSchemaMissing(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = client.Chat(context.Background(), ports.ChatRequest{
		Tools: []ports.ToolSpec{{Name: "lookup"}},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}

	tools := gotBody["tools"].([]any)
	toolFunction := tools[0].(map[string]any)["function"].(map[string]any)
	parameters := toolFunction["parameters"].(map[string]any)
	if parameters["type"] != "object" {
		t.Fatalf("parameters = %#v", parameters)
	}
	properties := parameters["properties"].(map[string]any)
	if len(properties) != 0 {
		t.Fatalf("properties = %#v", properties)
	}
	if parameters["additionalProperties"] != true {
		t.Fatalf("additionalProperties = %#v", parameters["additionalProperties"])
	}
}

func TestClientParsesTextResponse(t *testing.T) {
	client := testClient(t, `{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`)

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Content != "hello" {
		t.Fatalf("Content = %q", resp.Content)
	}
}

func TestClientParsesToolCallResponse(t *testing.T) {
	client := testClient(t, `{
		"choices": [{
			"message": {
				"role": "assistant",
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {
						"name": "lookup",
						"arguments": "{\"q\":\"go\"}"
					}
				}]
			}
		}]
	}`)

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %#v", resp.ToolCalls)
	}
	call := resp.ToolCalls[0]
	if call.ID != "call_1" || call.Name != "lookup" || string(call.Input) != `{"q":"go"}` {
		t.Fatalf("ToolCall = %#v", call)
	}
}

func TestClientMapsUsage(t *testing.T) {
	client := testClient(t, `{
		"choices": [{"message": {"role": "assistant", "content": "hello"}}],
		"usage": {"prompt_tokens": 3, "completion_tokens": 4}
	}`)

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("Usage = %#v", resp.Usage)
	}
}

func TestClientRequiresBaseURLAndModel(t *testing.T) {
	if _, err := New(Config{Model: "test-model"}); err == nil {
		t.Fatal("New returned nil error without BaseURL")
	}
	if _, err := New(Config{BaseURL: "http://example.test/v1"}); err == nil {
		t.Fatal("New returned nil error without Model")
	}
}

func TestClientReturnsErrorForNon2xxResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = client.Chat(context.Background(), ports.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("err = %v", err)
	}
	var responseErr *ResponseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("err type = %T, want *ResponseError", err)
	}
	if responseErr.StatusCode != http.StatusBadRequest || !strings.Contains(responseErr.Body, "bad request") {
		t.Fatalf("ResponseError = %+v", responseErr)
	}
}

func TestClientReturnsErrorForMalformedJSON(t *testing.T) {
	client := testClient(t, `{`)

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err == nil {
		t.Fatal("Chat returned nil error")
	}
}

func TestClientReturnsErrorForToolCallWithoutID(t *testing.T) {
	client := testClient(t, `{
		"choices": [{
			"message": {
				"role": "assistant",
				"tool_calls": [{
					"type": "function",
					"function": {
						"name": "lookup",
						"arguments": "{\"q\":\"go\"}"
					}
				}]
			}
		}]
	}`)

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "tool call id") {
		t.Fatalf("err = %v", err)
	}
}

func testClient(t *testing.T, response string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)

	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	return client
}
