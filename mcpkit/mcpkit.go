package mcpkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

// ToolDescriptor is the subset of an MCP tool descriptor needed to expose the
// tool through goagent. Transport and session fields intentionally stay out.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	Annotations ToolAnnotations `json:"annotations,omitempty"`
	Metadata    map[string]any  `json:"metadata,omitempty"`
}

// ToolAnnotations follows MCP-style safety hints. Unknown tools remain
// permissionless so goagent policy denies them unless the host opts in.
type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint,omitempty"`
	DestructiveHint bool `json:"destructiveHint,omitempty"`
	IdempotentHint  bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   bool `json:"openWorldHint,omitempty"`
}

// ToolCallResult is a transport-neutral MCP tool result. Content is projected
// separately for model-visible and user-visible goagent tool fields.
type ToolCallResult struct {
	Content           []ContentPart   `json:"content,omitempty"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	Ref               string          `json:"ref,omitempty"`
	Metadata          map[string]any  `json:"metadata,omitempty"`
}

type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

// Client is the host-provided boundary to an MCP implementation. mcpkit does
// not own stdio, HTTP, authorization, or JSON-RPC lifecycle handling.
type Client interface {
	ListTools(context.Context) ([]ToolDescriptor, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (*ToolCallResult, error)
}

// RegisterOptions controls how MCP descriptors become goagent tool specs.
type RegisterOptions struct {
	Permission    policy.Permission
	ExecutionMode ports.ExecutionMode
	Timeout       time.Duration
	MaxLLMChars   int
	// TrustServerAnnotations must be set only after host-side server verification.
	// Untrusted MCP annotations are hints, never an authorization source.
	TrustServerAnnotations bool
}

type Registry interface {
	Register(tools.Tool)
}

type Tool struct {
	Client     Client
	Descriptor ToolDescriptor
	Options    RegisterOptions
}

// RegisterTools lists MCP tools once and registers goagent tool adapters for
// each descriptor. Hosts can refresh by calling it again with their own registry
// lifecycle policy.
func RegisterTools(ctx context.Context, registry Registry, client Client, options RegisterOptions) ([]tools.Spec, error) {
	if registry == nil {
		return nil, fmt.Errorf("registry is required")
	}
	if client == nil {
		return nil, fmt.Errorf("mcp client is required")
	}
	descriptors, err := client.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	specs := make([]tools.Spec, 0, len(descriptors))
	for _, descriptor := range descriptors {
		tool := Tool{
			Client:     client,
			Descriptor: cloneToolDescriptor(descriptor),
			Options:    options,
		}
		spec := tool.Spec()
		if strings.TrimSpace(spec.Name) == "" {
			return nil, fmt.Errorf("mcp tool name is required")
		}
		registry.Register(tool)
		specs = append(specs, spec)
	}
	return specs, nil
}

func (t Tool) Spec() tools.Spec {
	return tools.Spec{
		Name:          t.Descriptor.Name,
		Description:   t.Descriptor.Description,
		Permission:    t.permission(),
		ExecutionMode: t.Options.ExecutionMode,
		Timeout:       t.Options.Timeout,
		Schema: ports.ToolSchema{
			JSONSchema: append(json.RawMessage(nil), t.Descriptor.InputSchema...),
		},
	}
}

func (t Tool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	if t.Client == nil {
		return nil, fmt.Errorf("mcp client is required")
	}
	result, err := t.Client.CallTool(ctx, t.Descriptor.Name, input)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &tools.Result{ForLLM: "mcp tool returned no result", IsError: true}, nil
	}
	return &tools.Result{
		ForLLM:   bounded(modelObservation(*result), t.Options.MaxLLMChars),
		ForUser:  userVisibleContent(*result),
		IsError:  result.IsError,
		Ref:      result.Ref,
		Metadata: cloneMetadata(result.Metadata),
	}, nil
}

func (t Tool) permission() policy.Permission {
	if t.Options.Permission != "" {
		return t.Options.Permission
	}
	if !t.Options.TrustServerAnnotations {
		return ""
	}
	if t.Descriptor.Annotations.ReadOnlyHint {
		return policy.PermissionRead
	}
	if t.Descriptor.Annotations.DestructiveHint {
		return policy.PermissionWrite
	}
	return ""
}

func modelObservation(result ToolCallResult) string {
	lines := make([]string, 0, len(result.Content)+2)
	if result.IsError {
		lines = append(lines, "mcp_error=true")
	}
	if len(result.StructuredContent) > 0 {
		lines = append(lines, "structured_content="+string(result.StructuredContent))
	}
	for _, part := range result.Content {
		switch part.Type {
		case "text", "":
			if part.Text != "" {
				lines = append(lines, part.Text)
			}
		default:
			lines = append(lines, fmt.Sprintf("content_part type=%s mime=%s bytes=%d", part.Type, part.MIMEType, len(part.Data)))
		}
	}
	if len(lines) == 0 && result.Ref != "" {
		lines = append(lines, "ref="+result.Ref)
	}
	return strings.Join(lines, "\n")
}

func userVisibleContent(result ToolCallResult) string {
	parts := make([]string, 0, len(result.Content)+1)
	if len(result.StructuredContent) > 0 {
		parts = append(parts, string(result.StructuredContent))
	}
	for _, part := range result.Content {
		if part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func bounded(value string, maxChars int) string {
	if maxChars <= 0 || len(value) <= maxChars {
		return value
	}
	if maxChars <= 3 {
		return value[:maxChars]
	}
	return value[:maxChars-3] + "..."
}

func cloneToolDescriptor(descriptor ToolDescriptor) ToolDescriptor {
	descriptor.InputSchema = append(json.RawMessage(nil), descriptor.InputSchema...)
	descriptor.Metadata = cloneMetadata(descriptor.Metadata)
	return descriptor
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]any, len(metadata))
	for key, value := range metadata {
		out[key] = value
	}
	return out
}
