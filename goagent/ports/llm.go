package ports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type LLMClient interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// StreamingLLMClient is an LLMClient that can deliver output while a call
// runs. ChatStream calls onDelta with each increment in order and returns the
// complete response, the same one Chat would return. Concatenating the
// deltas' Content gives the response's Content, and concatenating the
// ToolCall pieces of each Index gives its ToolCalls. An error from onDelta
// aborts the call and is returned.
type StreamingLLMClient interface {
	LLMClient
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(ChatDelta) error) (*ChatResponse, error)
}

// ChatDelta is an increment of model output: generated text, a piece of a
// tool call, or both.
type ChatDelta struct {
	Content  string
	ToolCall *ToolCallDelta
}

// ToolCallDelta is a piece of the tool call at Index, its position in the
// response's ToolCalls. ID and Name are set on the piece that first carries
// them; Arguments is the next fragment of the call's JSON arguments, which are
// valid JSON only once complete.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// ResponseDeltas splits a complete response into the deltas a streaming client
// would have sent, for clients that only have the whole response.
func ResponseDeltas(resp *ChatResponse) []ChatDelta {
	if resp == nil {
		return nil
	}
	var deltas []ChatDelta
	if resp.Content != "" {
		deltas = append(deltas, ChatDelta{Content: resp.Content})
	}
	for i, call := range resp.ToolCalls {
		deltas = append(deltas, ChatDelta{ToolCall: &ToolCallDelta{Index: i, ID: call.ID, Name: call.Name, Arguments: string(call.Input)}})
	}
	return deltas
}

type ChatRequest struct {
	Messages []ChatMessage
	Tools    []ToolSpec
	// MaxOutputTokens caps generated tokens for this call; zero leaves it to the provider.
	MaxOutputTokens int
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

// ErrToolSchemaInvalid marks a tool whose own JSON Schema does not compile, a
// host bug. ErrToolInputInvalid marks arguments that fail the schema, which the
// model can correct.
var (
	ErrToolSchemaInvalid = errors.New("invalid tool JSON schema")
	ErrToolInputInvalid  = errors.New("invalid tool input")
)

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

// Compile reports whether the JSON Schema compiles, so hosts can reject a bad
// tool when they configure it rather than on its first call.
func (s ToolSchema) Compile() error {
	if len(s.JSONSchema) == 0 {
		return nil
	}
	_, err := compileJSONSchema(s.JSONSchema)
	return err
}

// compiledSchemas caches compiled schemas by source text. Tool schemas are few
// and validated on every call; a schema that fails to compile is cached too.
var compiledSchemas sync.Map // string -> compiledSchema

type compiledSchema struct {
	schema *jsonschema.Schema
	err    error
}

func compileJSONSchema(source json.RawMessage) (*jsonschema.Schema, error) {
	key := string(source)
	if cached, ok := compiledSchemas.Load(key); ok {
		entry := cached.(compiledSchema)
		return entry.schema, entry.err
	}
	entry := compiledSchema{}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(key))
	if err == nil {
		compiler := jsonschema.NewCompiler()
		if err = compiler.AddResource("tool.schema.json", doc); err == nil {
			entry.schema, err = compiler.Compile("tool.schema.json")
		}
	}
	if err != nil {
		entry = compiledSchema{err: fmt.Errorf("%w: %w", ErrToolSchemaInvalid, err)}
	}
	compiledSchemas.Store(key, entry)
	return entry.schema, entry.err
}

func validateJSONSchema(source json.RawMessage, input json.RawMessage) error {
	compiled, err := compileJSONSchema(source)
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(string(input)))
	if err != nil {
		return fmt.Errorf("%w: arguments are not valid JSON: %w", ErrToolInputInvalid, err)
	}
	if err := compiled.Validate(instance); err != nil {
		return fmt.Errorf("%w: %w", ErrToolInputInvalid, err)
	}
	return nil
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}
