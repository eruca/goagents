package ports

import (
	"context"
	"encoding/json"
)

type ToolEnv struct {
	UserID    string
	SessionID string
	Metadata  map[string]any
}

type ToolResult struct {
	ForLLM   string
	ForUser  string
	Silent   bool
	IsError  bool
	Ref      string
	Metadata map[string]any
}

type ToolExecution struct {
	Index  int
	Call   ToolCall
	Result *ToolResult
}

type Tool interface {
	Spec() ToolSpec
	Execute(ctx context.Context, input json.RawMessage, env ToolEnv) (*ToolResult, error)
}

type ToolRegistry interface {
	Get(name string) (Tool, bool)
	MustGet(name string) (Tool, error)
	Specs() []ToolSpec
}
