package ports

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type LLMClient interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

type ChatRequest struct {
	Messages []ChatMessage
	Tools    []ToolSpec
}

type ChatMessage struct {
	Role       string
	Content    string
	ToolCallID string
	ToolCalls  []ToolCall
}

type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
}

type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type ToolSpec struct {
	Name          string
	Description   string
	Permission    Permission
	ExecutionMode ExecutionMode
	Timeout       time.Duration
	Schema        ToolSchema
}

type ExecutionMode string

const (
	ExecutionModeAuto       ExecutionMode = ""
	ExecutionModeParallel   ExecutionMode = "parallel"
	ExecutionModeSequential ExecutionMode = "sequential"
	ExecutionModeExclusive  ExecutionMode = "exclusive"
)

type ToolSchema struct {
	JSONSchema json.RawMessage
	Validate   func(json.RawMessage) error
}

func (s ToolSchema) ValidateInput(input json.RawMessage) error {
	if len(s.JSONSchema) > 0 {
		if err := validateJSONSchema(s.JSONSchema, input); err != nil {
			return err
		}
	}
	if s.Validate == nil {
		return nil
	}
	return s.Validate(input)
}

func validateJSONSchema(schema json.RawMessage, input json.RawMessage) error {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schema)))
	if err != nil {
		return fmt.Errorf("invalid tool JSON schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("tool.schema.json", doc); err != nil {
		return fmt.Errorf("invalid tool JSON schema: %w", err)
	}
	compiled, err := compiler.Compile("tool.schema.json")
	if err != nil {
		return fmt.Errorf("invalid tool JSON schema: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(string(input)))
	if err != nil {
		return fmt.Errorf("invalid tool input JSON: %w", err)
	}
	if err := compiled.Validate(instance); err != nil {
		return fmt.Errorf("tool input failed JSON schema validation: %w", err)
	}
	return nil
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}
