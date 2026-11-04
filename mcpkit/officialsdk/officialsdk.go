package officialsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/eruca/goagents/mcpkit"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultClientName    = "goagents-mcpkit"
	defaultClientVersion = "v0.0.0"
	// maxToolListPages bounds how many tool list pages a server can make
	// ListTools fetch, so a server cannot keep it paging forever.
	maxToolListPages = 100
)

// StdioConfig describes a local MCP server process managed through the official
// SDK CommandTransport. Command, Args, Env, and Dir are host-owned config.
//
// The server process does not inherit the host's environment, which holds
// secrets such as LLM API keys. It gets only the variables named in
// InheritEnv, copied from the host, plus Env. A nil InheritEnv means
// DefaultInheritEnv; an empty, non-nil one inherits nothing.
type StdioConfig struct {
	Command           string
	Args              []string
	Env               []string
	InheritEnv        []string
	Dir               string
	Name              string
	Version           string
	TerminateDuration time.Duration
}

// StreamableHTTPConfig describes a remote MCP endpoint managed through the
// official SDK StreamableClientTransport. The first adapter slice defaults to
// request/response mode by disabling standalone SSE unless explicitly enabled.
type StreamableHTTPConfig struct {
	Endpoint            string
	HTTPClient          *http.Client
	MaxRetries          int
	EnableStandaloneSSE bool
	OAuthHandler        auth.OAuthHandler
	Name                string
	Version             string
}

// Client adapts an official MCP SDK client session to the transport-neutral
// mcpkit.Client interface.
type Client struct {
	session *mcp.ClientSession
}

func ConnectStdio(ctx context.Context, cfg StdioConfig) (*Client, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("stdio command is required")
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = childEnv(cfg)
	cmd.Dir = cfg.Dir

	sdkClient := newSDKClient(cfg.Name, cfg.Version)
	session, err := sdkClient.Connect(ctx, &mcp.CommandTransport{
		Command:           cmd,
		TerminateDuration: cfg.TerminateDuration,
	}, nil)
	if err != nil {
		return nil, err
	}
	return &Client{session: session}, nil
}

// DefaultInheritEnv lists the host variables a stdio server inherits when
// StdioConfig.InheritEnv is nil: what a process needs to find programs,
// a home and temporary directory, and its locale, and nothing secret.
var DefaultInheritEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TEMP", "TMP",
	"LANG", "LC_ALL", "LC_CTYPE", "TZ",
	"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "USERPROFILE",
}

func childEnv(cfg StdioConfig) []string {
	names := cfg.InheritEnv
	if names == nil {
		names = DefaultInheritEnv
	}
	env := make([]string, 0, len(names)+len(cfg.Env))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env, cfg.Env...)
}

func ConnectStreamableHTTP(ctx context.Context, cfg StreamableHTTPConfig) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("streamable http endpoint is required")
	}
	transport := &mcp.StreamableClientTransport{
		Endpoint:             cfg.Endpoint,
		HTTPClient:           cfg.HTTPClient,
		MaxRetries:           cfg.MaxRetries,
		DisableStandaloneSSE: !cfg.EnableStandaloneSSE,
		OAuthHandler:         cfg.OAuthHandler,
	}
	session, err := newSDKClient(cfg.Name, cfg.Version).Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	return &Client{session: session}, nil
}

func newSDKClient(name string, version string) *mcp.Client {
	if name == "" {
		name = defaultClientName
	}
	if version == "" {
		version = defaultClientVersion
	}
	return mcp.NewClient(&mcp.Implementation{Name: name, Version: version}, nil)
}

func (c *Client) ListTools(ctx context.Context) ([]mcpkit.ToolDescriptor, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("mcp session is required")
	}

	var out []mcpkit.ToolDescriptor
	params := &mcp.ListToolsParams{}
	seenCursors := map[string]struct{}{}
	for page := 0; ; page++ {
		if page == maxToolListPages {
			return nil, fmt.Errorf("mcp server listed more than %d pages of tools", maxToolListPages)
		}
		result, err := c.session.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		for _, tool := range result.Tools {
			descriptor, err := toolDescriptor(tool)
			if err != nil {
				return nil, err
			}
			out = append(out, descriptor)
		}
		if result.NextCursor == "" {
			return out, nil
		}
		if _, repeated := seenCursors[result.NextCursor]; repeated {
			return nil, fmt.Errorf("mcp server repeated tool list cursor %q", result.NextCursor)
		}
		seenCursors[result.NextCursor] = struct{}{}
		params.Cursor = result.NextCursor
	}
}

func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*mcpkit.ToolCallResult, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("mcp session is required")
	}
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: normalizedArguments(arguments),
	})
	if err != nil {
		return nil, err
	}
	return toolCallResult(result)
}

func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.Close()
}

func toolDescriptor(tool *mcp.Tool) (mcpkit.ToolDescriptor, error) {
	if tool == nil {
		return mcpkit.ToolDescriptor{}, fmt.Errorf("mcp tool descriptor is nil")
	}
	inputSchema, err := rawJSON(tool.InputSchema)
	if err != nil {
		return mcpkit.ToolDescriptor{}, fmt.Errorf("mcp tool %q input schema: %w", tool.Name, err)
	}
	return mcpkit.ToolDescriptor{
		Name:        tool.Name,
		Description: tool.Description,
		InputSchema: inputSchema,
		Annotations: toolAnnotations(tool.Annotations),
		Metadata:    metadata(tool.Meta),
	}, nil
}

func toolAnnotations(annotations *mcp.ToolAnnotations) mcpkit.ToolAnnotations {
	if annotations == nil {
		return mcpkit.ToolAnnotations{}
	}
	return mcpkit.ToolAnnotations{
		ReadOnlyHint:    annotations.ReadOnlyHint,
		DestructiveHint: annotations.DestructiveHint != nil && *annotations.DestructiveHint,
		IdempotentHint:  annotations.IdempotentHint,
		OpenWorldHint:   annotations.OpenWorldHint != nil && *annotations.OpenWorldHint,
	}
}

func toolCallResult(result *mcp.CallToolResult) (*mcpkit.ToolCallResult, error) {
	if result == nil {
		return nil, nil
	}
	structured, err := rawJSON(result.StructuredContent)
	if err != nil {
		return nil, fmt.Errorf("mcp structured content: %w", err)
	}
	parts, err := contentParts(result.Content)
	if err != nil {
		return nil, err
	}
	return &mcpkit.ToolCallResult{
		Content:           parts,
		StructuredContent: structured,
		IsError:           result.IsError,
		Metadata:          metadata(result.Meta),
	}, nil
}

func contentParts(contents []mcp.Content) ([]mcpkit.ContentPart, error) {
	if len(contents) == 0 {
		return nil, nil
	}
	out := make([]mcpkit.ContentPart, 0, len(contents))
	for _, content := range contents {
		part, err := contentPart(content)
		if err != nil {
			return nil, err
		}
		out = append(out, part)
	}
	return out, nil
}

func contentPart(content mcp.Content) (mcpkit.ContentPart, error) {
	switch c := content.(type) {
	case *mcp.TextContent:
		return mcpkit.ContentPart{Type: "text", Text: c.Text}, nil
	case *mcp.ImageContent:
		return mcpkit.ContentPart{Type: "image", MIMEType: c.MIMEType, Data: append([]byte(nil), c.Data...)}, nil
	case *mcp.AudioContent:
		return mcpkit.ContentPart{Type: "audio", MIMEType: c.MIMEType, Data: append([]byte(nil), c.Data...)}, nil
	default:
		return wireContentPart(content)
	}
}

func wireContentPart(content mcp.Content) (mcpkit.ContentPart, error) {
	raw, err := json.Marshal(content)
	if err != nil {
		return mcpkit.ContentPart{}, err
	}
	var wire struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		MIMEType string `json:"mimeType,omitempty"`
		Data     []byte `json:"data,omitempty"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return mcpkit.ContentPart{}, err
	}
	if wire.Type == "" {
		wire.Type = fmt.Sprintf("%T", content)
	}
	return mcpkit.ContentPart{
		Type:     wire.Type,
		Text:     wire.Text,
		MIMEType: wire.MIMEType,
		Data:     append([]byte(nil), wire.Data...),
	}, nil
}

func rawJSON(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	switch v := value.(type) {
	case json.RawMessage:
		return cloneRaw(v), nil
	case []byte:
		return cloneRaw(v), nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	return raw, nil
}

func normalizedArguments(arguments json.RawMessage) any {
	trimmed := bytes.TrimSpace(arguments)
	if len(trimmed) == 0 {
		return map[string]any{}
	}
	return cloneRaw(trimmed)
}

func metadata(meta mcp.Meta) map[string]any {
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]any, len(meta))
	for key, value := range meta {
		out[key] = value
	}
	return out
}

func cloneRaw(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
