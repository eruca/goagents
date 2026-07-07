package officialsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/tools"
	"github.com/eruca/mcpkit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioClientListsCallsAndRegistersTools(t *testing.T) {
	client, err := ConnectStdio(context.Background(), StdioConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperMCPServer"},
		Env:     []string{"MCPKIT_OFFICIALSDK_HELPER=1"},
		Name:    "mcpkit-test",
		Version: "v0.1.0",
	})
	if err != nil {
		t.Fatalf("ConnectStdio returned error: %v", err)
	}
	defer client.Close()

	descriptors, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools returned error: %v", err)
	}
	if len(descriptors) != 1 {
		t.Fatalf("descriptors = %#v", descriptors)
	}
	descriptor := descriptors[0]
	if descriptor.Name != "greet" || !descriptor.Annotations.ReadOnlyHint {
		t.Fatalf("descriptor = %#v", descriptor)
	}
	if string(descriptor.InputSchema) == "" {
		t.Fatalf("input schema missing: %#v", descriptor)
	}

	result, err := client.CallTool(context.Background(), "greet", json.RawMessage(`{"name":"Ada"}`))
	if err != nil {
		t.Fatalf("CallTool returned error: %v", err)
	}
	if firstText(result) != "hello Ada" {
		t.Fatalf("tool result = %#v", result)
	}
	if string(result.StructuredContent) != `{"greeting":"hello Ada"}` {
		t.Fatalf("structured content = %s", result.StructuredContent)
	}

	registry := tools.NewRegistry()
	specs, err := mcpkit.RegisterTools(context.Background(), registry, client, mcpkit.RegisterOptions{MaxLLMChars: 100})
	if err != nil {
		t.Fatalf("RegisterTools returned error: %v", err)
	}
	if len(specs) != 1 || specs[0].Permission != policy.PermissionRead {
		t.Fatalf("specs = %#v", specs)
	}
	registered, ok := registry.Get("greet")
	if !ok {
		t.Fatal("registered tool missing")
	}
	toolResult, err := registered.Execute(context.Background(), json.RawMessage(`{"name":"Grace"}`), tools.Env{})
	if err != nil {
		t.Fatalf("registered tool Execute returned error: %v", err)
	}
	if toolResult.ForLLM != "structured_content={\"greeting\":\"hello Grace\"}\nhello Grace" {
		t.Fatalf("ForLLM = %q", toolResult.ForLLM)
	}
}

func TestDescriptorMappingPreservesExplicitAnnotations(t *testing.T) {
	destructive := true
	openWorld := false
	descriptor, err := toolDescriptor(&mcp.Tool{
		Name:        "write_note",
		Description: "Write a note.",
		InputSchema: map[string]any{"type": "object"},
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: &destructive,
			IdempotentHint:  true,
			OpenWorldHint:   &openWorld,
		},
	})
	if err != nil {
		t.Fatalf("toolDescriptor returned error: %v", err)
	}
	if !descriptor.Annotations.DestructiveHint || !descriptor.Annotations.IdempotentHint {
		t.Fatalf("annotations = %#v", descriptor.Annotations)
	}
	if descriptor.Annotations.OpenWorldHint {
		t.Fatalf("open world hint = true, want false")
	}
}

func TestToolCallResultMapsContentAndMetadata(t *testing.T) {
	result, err := toolCallResult(&mcp.CallToolResult{
		Meta:              mcp.Meta{"request_id": "req-1"},
		Content:           []mcp.Content{&mcp.TextContent{Text: "ok"}, &mcp.ImageContent{MIMEType: "image/png", Data: []byte("abc")}},
		StructuredContent: map[string]any{"ok": true},
		IsError:           true,
	})
	if err != nil {
		t.Fatalf("toolCallResult returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("IsError = false")
	}
	if string(result.StructuredContent) != `{"ok":true}` {
		t.Fatalf("structured content = %s", result.StructuredContent)
	}
	want := []mcpkit.ContentPart{
		{Type: "text", Text: "ok"},
		{Type: "image", MIMEType: "image/png", Data: []byte("abc")},
	}
	if !reflect.DeepEqual(result.Content, want) {
		t.Fatalf("content = %#v", result.Content)
	}
	if !reflect.DeepEqual(result.Metadata, map[string]any{"request_id": "req-1"}) {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
}

func TestConnectStdioRequiresCommand(t *testing.T) {
	_, err := ConnectStdio(context.Background(), StdioConfig{})
	if err == nil {
		t.Fatal("expected missing command error")
	}
}

func TestHelperMCPServer(t *testing.T) {
	if os.Getenv("MCPKIT_OFFICIALSDK_HELPER") != "1" {
		return
	}
	if err := runHelperMCPServer(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runHelperMCPServer() error {
	server := mcp.NewServer(&mcp.Implementation{Name: "helper", Version: "v0.1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "greet",
		Description: "Return a deterministic greeting.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"required":["name"],
			"properties":{"name":{"type":"string"}},
			"additionalProperties":false
		}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return nil, err
		}
		greeting := "hello " + input.Name
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: greeting}},
			StructuredContent: map[string]any{"greeting": greeting},
		}, nil
	})
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

func TestCloseNilClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (&Client{}).Close(); err != nil {
		t.Fatalf("Close empty client returned error: %v", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("context unexpectedly done: %v", err)
	}
}

func firstText(result *mcpkit.ToolCallResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	return result.Content[0].Text
}
