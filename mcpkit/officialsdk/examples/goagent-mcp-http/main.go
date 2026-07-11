package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/tools"
	"github.com/eruca/mcpkit"
	"github.com/eruca/mcpkit/officialsdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mockLLM struct {
	calls          int
	sawObservation bool
}

func (m *mockLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	m.calls++
	switch m.calls {
	case 1:
		return &ports.ChatResponse{
			ToolCalls: []ports.ToolCall{{
				Name:  "lookup_status",
				Input: json.RawMessage(`{"account":"A-123"}`),
			}},
			Usage: ports.Usage{InputTokens: 24, OutputTokens: 6},
		}, nil
	case 2:
		m.sawObservation = messagesContain(req.Messages, "A-123 active")
		if !m.sawObservation {
			return nil, fmt.Errorf("mock LLM did not receive MCP tool observation")
		}
		return &ports.ChatResponse{
			Content: "Final answer: A-123 is active via MCP HTTP.",
			Usage:   ports.Usage{InputTokens: 42, OutputTokens: 11},
		}, nil
	default:
		return nil, fmt.Errorf("unexpected LLM call %d", m.calls)
	}
}

func messagesContain(messages []ports.ChatMessage, want string) bool {
	for _, message := range messages {
		if message.Content == "" {
			continue
		}
		if strings.Contains(message.Content, want) {
			return true
		}
	}
	return false
}

type report struct {
	FinalAnswer        string   `json:"final_answer"`
	LLMCalls           int      `json:"llm_calls"`
	ToolCalls          int      `json:"tool_calls"`
	UsedTools          []string `json:"used_tools"`
	MCPObservationSeen bool     `json:"mcp_observation_seen"`
}

func main() {
	if err := run(os.Stdout); err != nil {
		panic(err)
	}
}

func run(out io.Writer) error {
	server := newServer()
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	defer httpServer.Close()

	ctx := context.Background()
	client, err := officialsdk.ConnectStreamableHTTP(ctx, officialsdk.StreamableHTTPConfig{
		Endpoint: httpServer.URL,
		Name:     "goagent-mcp-http-client",
		Version:  "v0.1.0",
	})
	if err != nil {
		return err
	}
	defer client.Close()

	registry := tools.NewRegistry()
	if _, err := mcpkit.RegisterTools(ctx, registry, client, mcpkit.RegisterOptions{
		MaxLLMChars:            200,
		TrustServerAnnotations: true,
	}); err != nil {
		return err
	}

	llm := &mockLLM{}
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(llm),
		agentcore.WithToolRegistry(registry),
	)
	if err != nil {
		return err
	}
	result, err := agent.Run(ctx, agentcore.RunRequest{Input: "Look up A-123 status through MCP and summarize it."})
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(report{
		FinalAnswer:        result.Content,
		LLMCalls:           result.ExecutionSummary.LLMCalls,
		ToolCalls:          result.ExecutionSummary.ToolCalls,
		UsedTools:          result.ExecutionSummary.UsedTools,
		MCPObservationSeen: llm.sawObservation,
	})
}

func newServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "goagent-mcp-http-server", Version: "v0.1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "lookup_status",
		Description: "Return deterministic account status for a goagent run.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["account"],
			"properties":{"account":{"type":"string"}},
			"additionalProperties":false
		}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input struct {
			Account string `json:"account"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return nil, err
		}
		status := input.Account + " active"
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: status}},
			StructuredContent: map[string]any{"status": status},
		}, nil
	})
	return server
}
