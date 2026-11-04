package openaiapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/eruca/goagents/goagent/ports"
)

type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
	Headers    map[string]string
	Limits     Limits
}

// Limits bounds provider requests and responses. A zero field disables that bound.
type Limits struct {
	MaxOutputTokens  int
	MaxRequestBytes  int
	MaxResponseBytes int
}

type Client struct {
	config   Config
	http     *http.Client
	endpoint string
	limits   Limits
}

func New(config Config) (*Client, error) {
	limits := config.Limits
	if limits.MaxOutputTokens < 0 || limits.MaxRequestBytes < 0 || limits.MaxResponseBytes < 0 {
		return nil, newProviderError("configuration_error", false, ErrInvalidConfig)
	}
	if config.BaseURL == "" {
		return nil, newProviderError("configuration_error", false, ErrInvalidConfig)
	}
	if config.Model == "" {
		return nil, newProviderError("configuration_error", false, ErrInvalidConfig)
	}
	endpoint, err := chatCompletionsURL(config.BaseURL)
	if err != nil {
		return nil, newProviderError("configuration_error", false, ErrInvalidConfig)
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clientCopy := *httpClient
	originalCheckRedirect := clientCopy.CheckRedirect
	clientCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && !sameOrigin(req.URL, via[0].URL) {
			return newProviderError("redirect_blocked", true, ErrRedirectBlocked)
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return newProviderError("redirect_blocked", true, ErrRedirectBlocked)
		}
		return nil
	}
	return &Client{config: config, http: &clientCopy, endpoint: endpoint, limits: limits}, nil
}

func (c *Client) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	payload, err := c.providerRequestPayload(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := readResponseBody(resp, c.limits.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	return parseChatResponse(data)
}

// post sends a chat completions body and returns a successful response.
func (c *Client) post(ctx context.Context, payload []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, newProviderError("configuration_error", false, ErrInvalidConfig)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.config.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}
	for key, value := range c.config.Headers {
		httpReq.Header.Set(key, value)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &ResponseError{StatusCode: resp.StatusCode}
	}
	return resp, nil
}

func parseChatResponse(data []byte) (*ports.ChatResponse, error) {
	var decoded chatCompletionsResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, newProviderError("invalid_json", true, ErrInvalidJSON)
	}
	if decoded.Usage == nil {
		return nil, newProviderError("usage_missing", true, ErrUsageMissing)
	}
	if len(decoded.Choices) == 0 {
		return nil, newProviderError("invalid_response", true, ErrInvalidResponse)
	}
	message := decoded.Choices[0].Message
	return chatResponse(message.Content, message.ToolCalls, *decoded.Usage)
}

func chatResponse(content string, calls []chatToolCall, usage chatUsage) (*ports.ChatResponse, error) {
	result := &ports.ChatResponse{Content: content}
	for _, call := range calls {
		if call.ID == "" || call.Function.Name == "" {
			return nil, newProviderError("tool_call_invalid", true, ErrInvalidResponse)
		}
		result.ToolCalls = append(result.ToolCalls, ports.ToolCall{
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: json.RawMessage(call.Function.Arguments),
		})
	}
	result.Usage.InputTokens = usage.PromptTokens
	result.Usage.OutputTokens = usage.CompletionTokens
	return result, nil
}

// ProviderRequestSHA256 returns the digest of the exact JSON body Chat sends.
func (c *Client) ProviderRequestSHA256(req ports.ChatRequest) ([sha256.Size]byte, error) {
	payload, err := c.providerRequestPayload(req)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(payload), nil
}

func (c *Client) providerRequestPayload(req ports.ChatRequest) ([]byte, error) {
	return c.requestPayload(req, false)
}

func (c *Client) requestPayload(req ports.ChatRequest, stream bool) ([]byte, error) {
	maxTokens, err := normalizedMaxOutputTokens(req.MaxOutputTokens, c.limits.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	body := chatCompletionsRequest{
		Model:     c.config.Model,
		Messages:  buildMessages(req.Messages),
		Tools:     buildTools(req.Tools),
		MaxTokens: maxTokens,
	}
	if stream {
		body.Stream = true
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, newProviderError("invalid_json", false, ErrInvalidJSON)
	}
	if c.limits.MaxRequestBytes > 0 && len(payload) > c.limits.MaxRequestBytes {
		return nil, newProviderError("request_too_large", false, ErrRequestTooLarge)
	}
	return payload, nil
}

func normalizedMaxOutputTokens(requested int, limit int) (int, error) {
	if requested < 0 {
		return 0, newProviderError("configuration_error", false, ErrInvalidConfig)
	}
	if limit > 0 && requested > limit {
		return 0, newProviderError("budget_exceeded", false, ErrMaxOutputTokensExceeded)
	}
	return requested, nil
}

func readResponseBody(resp *http.Response, maxResponseBytes int) ([]byte, error) {
	if maxResponseBytes <= 0 {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, newProviderError("invalid_response", true, ErrInvalidResponse)
		}
		return data, nil
	}
	if resp.ContentLength > int64(maxResponseBytes) {
		return nil, newProviderError("response_too_large", true, ErrResponseTooLarge)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxResponseBytes)+1))
	if err != nil {
		return nil, newProviderError("invalid_response", true, ErrInvalidResponse)
	}
	if len(data) > maxResponseBytes {
		return nil, newProviderError("response_too_large", true, ErrResponseTooLarge)
	}
	return data, nil
}

type chatCompletionsRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Tools         []chatTool     `json:"tools,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
}

// MarshalJSON always sends content, even empty, except on an assistant
// message that only carries tool calls: OpenAI rejects a tool or user
// message without content, such as a tool result with an empty ForLLM.
func (m chatMessage) MarshalJSON() ([]byte, error) {
	wire := struct {
		Role       string         `json:"role"`
		Content    *string        `json:"content,omitempty"`
		ToolCallID string         `json:"tool_call_id,omitempty"`
		ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	}{Role: m.Role, ToolCallID: m.ToolCallID, ToolCalls: m.ToolCalls}
	if m.Content != "" || len(m.ToolCalls) == 0 {
		wire.Content = &m.Content
	}
	return json.Marshal(wire)
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Function chatToolMetadata `json:"function"`
}

type chatToolMetadata struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatCompletionsResponse struct {
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
}

type chatChoice struct {
	Message chatResponseMessage `json:"message"`
}

type chatResponseMessage struct {
	Content   string         `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func buildMessages(messages []ports.ChatMessage) []chatMessage {
	converted := make([]chatMessage, 0, len(messages))
	for _, message := range messages {
		converted = append(converted, chatMessage{
			Role:       message.Role,
			Content:    message.Content,
			ToolCallID: message.ToolCallID,
			ToolCalls:  buildMessageToolCalls(message.ToolCalls),
		})
	}
	return converted
}

func buildMessageToolCalls(calls []ports.ToolCall) []chatToolCall {
	converted := make([]chatToolCall, 0, len(calls))
	for _, call := range calls {
		converted = append(converted, chatToolCall{
			ID:   call.ID,
			Type: "function",
			Function: chatToolFunction{
				Name:      call.Name,
				Arguments: string(call.Input),
			},
		})
	}
	return converted
}

func buildTools(specs []ports.ToolSpec) []chatTool {
	if len(specs) == 0 {
		return nil
	}
	converted := make([]chatTool, 0, len(specs))
	for _, spec := range specs {
		converted = append(converted, chatTool{
			Type: "function",
			Function: chatToolMetadata{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  toolParameters(spec.Schema),
			},
		})
	}
	return converted
}

func toolParameters(schema ports.ToolSchema) json.RawMessage {
	if len(schema.JSONSchema) > 0 {
		return schema.JSONSchema
	}
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)
}

func chatCompletionsURL(baseURL string) (string, error) {
	trimmed := strings.TrimRight(baseURL, "/")
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid BaseURL %q", baseURL)
	}
	if strings.HasSuffix(parsed.Path, "/chat/completions") {
		return parsed.String(), nil
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/chat/completions"
	return parsed.String(), nil
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}
