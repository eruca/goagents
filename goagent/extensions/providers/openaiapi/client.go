package openaiapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/eruca/goagent/ports"
)

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

func New(config Config) (*Client, error) {
	if config.BaseURL == "" {
		return nil, fmt.Errorf("openai-compatible provider requires BaseURL")
	}
	if config.Model == "" {
		return nil, fmt.Errorf("openai-compatible provider requires Model")
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{config: config, http: httpClient}, nil
}

func (c *Client) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	body := chatCompletionsRequest{
		Model:    c.config.Model,
		Messages: buildMessages(req.Messages),
		Tools:    buildTools(req.Tools),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	endpoint, err := chatCompletionsURL(c.config.BaseURL)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
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
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &ResponseError{StatusCode: resp.StatusCode, Body: string(data)}
	}

	var decoded chatCompletionsResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	result := &ports.ChatResponse{}
	if len(decoded.Choices) > 0 {
		message := decoded.Choices[0].Message
		result.Content = message.Content
		for _, call := range message.ToolCalls {
			if call.ID == "" {
				return nil, fmt.Errorf("openai-compatible response missing tool call id")
			}
			if call.Function.Name == "" {
				return nil, fmt.Errorf("openai-compatible response missing tool call function name")
			}
			result.ToolCalls = append(result.ToolCalls, ports.ToolCall{
				ID:    call.ID,
				Name:  call.Function.Name,
				Input: json.RawMessage(call.Function.Arguments),
			})
		}
	}
	result.Usage.InputTokens = decoded.Usage.PromptTokens
	result.Usage.OutputTokens = decoded.Usage.CompletionTokens
	return result, nil
}

type chatCompletionsRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
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
	Usage   chatUsage    `json:"usage"`
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
