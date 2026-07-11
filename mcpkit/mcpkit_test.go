package mcpkit

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/tools"
)

func TestRegisterToolsMapsDescriptorsToGoagentTools(t *testing.T) {
	client := &fakeMCPClient{
		descriptors: []ToolDescriptor{{
			Name:        "search_docs",
			Description: "Search documentation.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
			Annotations: ToolAnnotations{
				ReadOnlyHint: true,
			},
		}},
		result: &ToolCallResult{
			Content:           []ContentPart{{Type: "text", Text: "found docs"}},
			StructuredContent: json.RawMessage(`{"count":1}`),
			Metadata:          map[string]any{"source": "mcp"},
		},
	}
	registry := tools.NewRegistry()

	specs, err := RegisterTools(context.Background(), registry, client, RegisterOptions{
		MaxLLMChars:            80,
		TrustServerAnnotations: true,
	})
	if err != nil {
		t.Fatalf("RegisterTools returned error: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %#v", specs)
	}
	spec := specs[0]
	if spec.Name != "search_docs" || spec.Permission != policy.PermissionRead {
		t.Fatalf("spec = %#v", spec)
	}
	if string(spec.Schema.JSONSchema) == "" {
		t.Fatalf("schema missing: %#v", spec.Schema)
	}

	tool, ok := registry.Get("search_docs")
	if !ok {
		t.Fatal("registered tool missing")
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"agent"}`), tools.Env{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if client.calledName != "search_docs" || string(client.calledArguments) != `{"query":"agent"}` {
		t.Fatalf("call = %s %s", client.calledName, client.calledArguments)
	}
	if !reflect.DeepEqual(result.Metadata, map[string]any{"source": "mcp"}) {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
	if result.ForUser != "{\"count\":1}\nfound docs" {
		t.Fatalf("for user = %q", result.ForUser)
	}
	if result.ForLLM != "structured_content={\"count\":1}\nfound docs" {
		t.Fatalf("for llm = %q", result.ForLLM)
	}
}

func TestRegisterToolsDefaultsUnknownPermissionToPolicyDenied(t *testing.T) {
	client := &fakeMCPClient{descriptors: []ToolDescriptor{{Name: "unknown_side_effect"}}}
	registry := tools.NewRegistry()

	specs, err := RegisterTools(context.Background(), registry, client, RegisterOptions{})
	if err != nil {
		t.Fatalf("RegisterTools returned error: %v", err)
	}
	if specs[0].Permission != "" {
		t.Fatalf("permission = %q, want empty denied-by-default permission", specs[0].Permission)
	}
}

func TestRegisterToolsIgnoresAnnotationsFromUntrustedServer(t *testing.T) {
	client := &fakeMCPClient{descriptors: []ToolDescriptor{{
		Name:        "delete_everything",
		Annotations: ToolAnnotations{ReadOnlyHint: true},
	}}}
	registry := tools.NewRegistry()

	specs, err := RegisterTools(context.Background(), registry, client, RegisterOptions{})
	if err != nil {
		t.Fatalf("RegisterTools returned error: %v", err)
	}
	if specs[0].Permission != "" {
		t.Fatalf("permission = %q, want empty denied-by-default permission", specs[0].Permission)
	}
}

func TestRegisterToolsUsesExplicitPermissionForUntrustedServer(t *testing.T) {
	client := &fakeMCPClient{descriptors: []ToolDescriptor{{
		Name:        "lookup_docs",
		Annotations: ToolAnnotations{DestructiveHint: true},
	}}}
	registry := tools.NewRegistry()

	specs, err := RegisterTools(context.Background(), registry, client, RegisterOptions{Permission: policy.PermissionRead})
	if err != nil {
		t.Fatalf("RegisterTools returned error: %v", err)
	}
	if specs[0].Permission != policy.PermissionRead {
		t.Fatalf("permission = %q, want explicit read permission", specs[0].Permission)
	}
}

func TestToolResultObservationIsBounded(t *testing.T) {
	client := &fakeMCPClient{
		result: &ToolCallResult{Content: []ContentPart{{Type: "text", Text: "abcdefghijklmnopqrstuvwxyz"}}},
	}
	tool := Tool{
		Client:     client,
		Descriptor: ToolDescriptor{Name: "long_result"},
		Options:    RegisterOptions{MaxLLMChars: 10},
	}

	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`), tools.Env{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.ForLLM != "abcdefg..." {
		t.Fatalf("bounded ForLLM = %q", result.ForLLM)
	}
	if result.ForUser != "abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("ForUser = %q", result.ForUser)
	}
}

type fakeMCPClient struct {
	descriptors     []ToolDescriptor
	result          *ToolCallResult
	calledName      string
	calledArguments json.RawMessage
}

func (f *fakeMCPClient) ListTools(ctx context.Context) ([]ToolDescriptor, error) {
	return f.descriptors, nil
}

func (f *fakeMCPClient) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*ToolCallResult, error) {
	f.calledName = name
	f.calledArguments = append(json.RawMessage(nil), arguments...)
	return f.result, nil
}
