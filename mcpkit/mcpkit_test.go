package mcpkit

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/tools"
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

func TestRegisterToolsRefusesUnsafeOrCollidingNamesWithoutRegistering(t *testing.T) {
	cases := map[string][]ToolDescriptor{
		"invalid name":     {{Name: "ok_tool"}, {Name: "has space"}},
		"duplicate name":   {{Name: "lookup"}, {Name: "lookup"}},
		"host tool exists": {{Name: "ok_tool"}, {Name: "record_review"}},
	}
	for name, descriptors := range cases {
		t.Run(name, func(t *testing.T) {
			registry := tools.NewRegistry()
			host := Tool{Descriptor: ToolDescriptor{Name: "record_review"}}
			registry.Register(host)
			if _, err := RegisterTools(context.Background(), registry, &fakeMCPClient{descriptors: descriptors}, RegisterOptions{}); err == nil {
				t.Fatal("RegisterTools error = nil")
			}
			if _, registered := registry.Get("ok_tool"); registered {
				t.Fatal("a valid tool was registered from a rejected listing")
			}
			if got, _ := registry.Get("record_review"); got.(Tool).Client != nil {
				t.Fatal("the host tool was replaced")
			}
		})
	}
}

func TestRegisterToolsPrefixesNamesButCallsServerName(t *testing.T) {
	client := &fakeMCPClient{descriptors: []ToolDescriptor{{Name: "record_review"}}, result: &ToolCallResult{}}
	registry := tools.NewRegistry()
	registry.Register(Tool{Descriptor: ToolDescriptor{Name: "record_review"}})
	specs, err := RegisterTools(context.Background(), registry, client, RegisterOptions{NamePrefix: "docs__"})
	if err != nil || len(specs) != 1 || specs[0].Name != "docs__record_review" {
		t.Fatalf("specs = %+v, %v; want the prefixed name", specs, err)
	}
	tool, _ := registry.Get("docs__record_review")
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`), tools.Env{}); err != nil {
		t.Fatal(err)
	}
	if client.calledName != "record_review" {
		t.Fatalf("called %q, want the server's own name", client.calledName)
	}
}

func TestToolResultBoundsCountCharactersNotBytes(t *testing.T) {
	client := &fakeMCPClient{result: &ToolCallResult{Content: []ContentPart{{Type: "text", Text: "中文内容很长很长"}}}}
	tool := Tool{Client: client, Descriptor: ToolDescriptor{Name: "cjk"}, Options: RegisterOptions{MaxLLMChars: 5, MaxUserChars: 4}}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`), tools.Env{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ForLLM != "中文..." || result.ForUser != "中..." {
		t.Fatalf("ForLLM = %q, ForUser = %q; want cuts on character boundaries", result.ForLLM, result.ForUser)
	}
}
