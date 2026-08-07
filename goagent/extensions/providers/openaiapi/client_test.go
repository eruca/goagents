package openaiapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
)

func TestClientProviderRequestSHA256MatchesDispatchedBody(t *testing.T) {
	var dispatchedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		dispatchedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	req := ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}},
	}
	want, err := client.ProviderRequestSHA256WithMaxOutputTokens(req, 123)
	if err != nil {
		t.Fatalf("ProviderRequestSHA256WithMaxOutputTokens returned error: %v", err)
	}
	if _, err := client.ChatWithMaxOutputTokens(context.Background(), req, 123); err != nil {
		t.Fatalf("ChatWithMaxOutputTokens returned error: %v", err)
	}
	if got := sha256.Sum256(dispatchedBody); got != want {
		t.Fatalf("dispatched request SHA-256 = %x, want %x", got, want)
	}
}

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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
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

	_, err = client.ChatWithMaxOutputTokens(context.Background(), ports.ChatRequest{
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
	}, 4096)
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
	if gotBody["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %#v, want 4096", gotBody["max_tokens"])
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
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
	client := testClient(t, `{"choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`)

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
		}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 1}
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
	if _, err := New(Config{Model: "test-model"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("New returned nil error without BaseURL")
	}
	if _, err := New(Config{BaseURL: "http://example.test/v1"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("New returned nil error without Model")
	}
}

func TestClientDoesNotExposeNon2xxResponseBody(t *testing.T) {
	const sensitiveBody = "provider-secret-response-body"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, sensitiveBody, http.StatusBadRequest)
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = client.Chat(context.Background(), ports.ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), sensitiveBody) {
		t.Fatalf("public error leaked provider body: %v", err)
	}
	var responseErr *ResponseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("err type = %T, want *ResponseError", err)
	}
	if responseErr.StatusCode != http.StatusBadRequest || responseErr.Body != "" {
		t.Fatalf("ResponseError = %+v, want status only", responseErr)
	}
}

func TestClientRejectsResponseWithoutUsage(t *testing.T) {
	client := testClient(t, `{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`)

	if _, err := client.Chat(context.Background(), ports.ChatRequest{}); !errors.Is(err, ErrUsageMissing) {
		t.Fatalf("Chat error = %v, want ErrUsageMissing", err)
	}
}

func TestClientRejectsCrossOriginRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"redirected"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client, err := New(Config{BaseURL: source.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if _, err := client.Chat(context.Background(), ports.ChatRequest{}); !errors.Is(err, ErrRedirectBlocked) {
		t.Fatalf("Chat error = %v, want ErrRedirectBlocked", err)
	}
}

func TestClientAllowsSameOriginRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			http.Redirect(w, r, "/redirected/chat/completions", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"redirected"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL + "/v1", Model: "test-model"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Content != "redirected" {
		t.Fatalf("response content = %q, want redirected", resp.Content)
	}
}

func TestClientEnforcesMaxOutputTokens(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		wantSent  int
		wantErr   bool
		wantCalls int
	}{
		{name: "omitted", requested: 0, wantCalls: 1},
		{name: "at limit", requested: 4096, wantSent: 4096, wantCalls: 1},
		{name: "over limit", requested: 4097, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			gotMaxTokens := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct {
					MaxTokens int `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				gotMaxTokens = body.MaxTokens
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer server.Close()

			client, err := NewWithLimits(
				Config{BaseURL: server.URL, Model: "test-model"},
				Limits{MaxOutputTokens: 4096},
			)
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}
			_, err = client.ChatWithMaxOutputTokens(context.Background(), ports.ChatRequest{}, tt.requested)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Chat() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrMaxOutputTokensExceeded) {
				t.Fatalf("Chat() error = %v, want ErrMaxOutputTokensExceeded", err)
			}
			if calls != tt.wantCalls {
				t.Fatalf("provider calls = %d, want %d", calls, tt.wantCalls)
			}
			if gotMaxTokens != tt.wantSent {
				t.Fatalf("max_tokens = %d, want %d", gotMaxTokens, tt.wantSent)
			}
		})
	}
}

func TestNewWithLimitsRejectsNegativeValues(t *testing.T) {
	limits := []Limits{
		{MaxOutputTokens: -1},
		{MaxRequestBytes: -1},
		{MaxResponseBytes: -1},
	}
	for _, limit := range limits {
		if _, err := NewWithLimits(Config{BaseURL: "https://provider.invalid", Model: "test-model"}, limit); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NewWithLimits(%+v) error = %v, want ErrInvalidConfig", limit, err)
		}
	}
}

func TestNewDoesNotApplyRequestLimitWithoutExplicitLimits(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Model: "test-model"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	_, err = client.Chat(context.Background(), ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "user", Content: strings.Repeat("x", (128<<10)+1)}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v, want opt-in request limit", err)
	}
	if calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
}

func TestNewDoesNotApplyResponseLimitWithoutExplicitLimits(t *testing.T) {
	base := `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	client := testClient(t, base+strings.Repeat(" ", (128<<10)+1))

	if _, err := client.Chat(context.Background(), ports.ChatRequest{}); err != nil {
		t.Fatalf("Chat() error = %v, want opt-in response limit", err)
	}
}

func TestClientEnforcesRequestByteLimit(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "at limit", size: 128 << 10},
		{name: "over limit", size: (128 << 10) + 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer server.Close()

			client, err := NewWithLimits(
				Config{BaseURL: server.URL, Model: "test-model"},
				Limits{MaxRequestBytes: 128 << 10},
			)
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}
			_, err = client.Chat(context.Background(), chatRequestWithSerializedSize(t, tt.size))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Chat() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrRequestTooLarge) {
				t.Fatalf("Chat() error = %v, want ErrRequestTooLarge", err)
			}
			wantCalls := 1
			if tt.wantErr {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("provider calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestClientEnforcesResponseByteLimit(t *testing.T) {
	base := `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "at limit", size: 128 << 10},
		{name: "over limit", size: (128 << 10) + 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := base + strings.Repeat(" ", tt.size-len(base))
			client := testClientWithLimits(t, response, Limits{MaxResponseBytes: 128 << 10})

			_, err := client.Chat(context.Background(), ports.ChatRequest{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Chat() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("Chat() error = %v, want ErrResponseTooLarge", err)
			}
		})
	}
}

func TestClientReturnsErrorForMalformedJSON(t *testing.T) {
	client := testClient(t, `{"provider-secret-response-body":`)

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	if !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("Chat error = %v, want ErrInvalidJSON", err)
	}
	if strings.Contains(err.Error(), "provider-secret-response-body") {
		t.Fatalf("public error leaked malformed response body: %v", err)
	}
}

func TestClientRejectsResponseWithoutChoices(t *testing.T) {
	client := testClient(t, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)

	if _, err := client.Chat(context.Background(), ports.ChatRequest{}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("Chat error = %v, want ErrInvalidResponse", err)
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
		}],
		"usage": {"prompt_tokens": 1, "completion_tokens": 1}
	}`)

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err = %v, want ErrInvalidResponse", err)
	}
}

func testClient(t *testing.T, response string) *Client {
	return testClientWithLimits(t, response, Limits{})
}

func testClientWithLimits(t *testing.T, response string, limits Limits) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)

	client, err := NewWithLimits(Config{BaseURL: server.URL, Model: "test-model"}, limits)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	return client
}

func chatRequestWithSerializedSize(t *testing.T, target int) ports.ChatRequest {
	t.Helper()
	req := ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "user", Content: "x"}},
	}
	for {
		payload, err := json.Marshal(chatCompletionsRequest{
			Model:    "test-model",
			Messages: buildMessages(req.Messages),
			Tools:    buildTools(req.Tools),
		})
		if err != nil {
			t.Fatalf("marshal request probe: %v", err)
		}
		delta := target - len(payload)
		if delta == 0 {
			return req
		}
		contentSize := len(req.Messages[0].Content) + delta
		if contentSize <= 0 {
			t.Fatalf("target request size %d is too small", target)
		}
		req.Messages[0].Content = strings.Repeat("x", contentSize)
	}
}
