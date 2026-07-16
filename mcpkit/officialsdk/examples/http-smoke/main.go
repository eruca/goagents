package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/eruca/goagents/goagent/tools"
	"github.com/eruca/goagents/mcpkit"
	"github.com/eruca/goagents/mcpkit/officialsdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type smokeReport struct {
	ToolName   string `json:"tool_name"`
	Permission string `json:"permission"`
	Endpoint   string `json:"endpoint"`
	ForLLM     string `json:"for_llm"`
	ForUser    string `json:"for_user"`
}

func main() {
	if err := run(os.Stdout); err != nil {
		panic(err)
	}
}

func run(out *os.File) error {
	server := newServer()
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	defer httpServer.Close()

	ctx := context.Background()
	client, err := officialsdk.ConnectStreamableHTTP(ctx, officialsdk.StreamableHTTPConfig{
		Endpoint: httpServer.URL,
		Name:     "http-smoke-client",
		Version:  "v0.1.0",
	})
	if err != nil {
		return err
	}
	defer client.Close()

	registry := tools.NewRegistry()
	specs, err := mcpkit.RegisterTools(ctx, registry, client, mcpkit.RegisterOptions{
		MaxLLMChars:            200,
		TrustServerAnnotations: true,
	})
	if err != nil {
		return err
	}
	tool, ok := registry.Get("lookup_status")
	if !ok {
		return fmt.Errorf("lookup_status tool was not registered")
	}
	result, err := tool.Execute(ctx, json.RawMessage(`{"account":"A-123"}`), tools.Env{})
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(smokeReport{
		ToolName:   specs[0].Name,
		Permission: string(specs[0].Permission),
		Endpoint:   httpServer.URL,
		ForLLM:     result.ForLLM,
		ForUser:    result.ForUser,
	})
}

func newServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "http-smoke-server", Version: "v0.1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "lookup_status",
		Description: "Return deterministic account status over Streamable HTTP.",
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
