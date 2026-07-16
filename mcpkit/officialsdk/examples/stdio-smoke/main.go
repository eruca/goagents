package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/eruca/goagents/goagent/tools"
	"github.com/eruca/goagents/mcpkit"
	"github.com/eruca/goagents/mcpkit/officialsdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type smokeReport struct {
	ToolName   string `json:"tool_name"`
	Permission string `json:"permission"`
	ForLLM     string `json:"for_llm"`
	ForUser    string `json:"for_user"`
}

func main() {
	serverMode := flag.Bool("server", false, "run the fake MCP server over stdio")
	flag.Parse()

	if *serverMode {
		if err := runServer(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := runClient(os.Stdout); err != nil {
		panic(err)
	}
}

func runClient(out *os.File) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx := context.Background()
	client, err := officialsdk.ConnectStdio(ctx, officialsdk.StdioConfig{
		Command: exe,
		Args:    []string{"--server"},
		Name:    "stdio-smoke-client",
		Version: "v0.1.0",
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
		ForLLM:     result.ForLLM,
		ForUser:    result.ForUser,
	})
}

func runServer() error {
	server := mcp.NewServer(&mcp.Implementation{Name: "stdio-smoke-server", Version: "v0.1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "lookup_status",
		Description: "Return deterministic account status.",
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
	return server.Run(context.Background(), &mcp.StdioTransport{})
}
