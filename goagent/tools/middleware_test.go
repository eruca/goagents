package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestMiddlewareRunsSchemaValidationBeforeExecutionAndOutputMaskingAfter(t *testing.T) {
	events := make([]string, 0, 3)
	schema := Schema{
		Validate: func(input json.RawMessage) error {
			events = append(events, "schema")
			return nil
		},
	}
	mask := func(result *Result) *Result {
		events = append(events, "mask")
		result.ForLLM = "masked"
		return result
	}
	handler := func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
		events = append(events, "execute")
		return &Result{ForLLM: "raw", ForUser: "raw"}, nil
	}

	result, err := Chain(SchemaValidator(schema), OutputMask(mask))(handler)(
		context.Background(),
		json.RawMessage(`{"ok":true}`),
		Env{},
	)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result.ForLLM != "masked" {
		t.Fatalf("ForLLM = %q", result.ForLLM)
	}

	want := []string{"schema", "execute", "mask"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v", events)
		}
	}
}

func TestToolSchemaKeepsJSONSchemaAndValidateBehavior(t *testing.T) {
	schema := Schema{
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		Validate: func(input json.RawMessage) error {
			if string(input) != `{"ok":true}` {
				return fmt.Errorf("invalid")
			}
			return nil
		},
	}
	if string(schema.JSONSchema) != `{"type":"object"}` {
		t.Fatalf("JSONSchema = %s", schema.JSONSchema)
	}
	if err := schema.ValidateInput(json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("ValidateInput returned error: %v", err)
	}
	if err := schema.ValidateInput(json.RawMessage(`{"ok":false}`)); err == nil {
		t.Fatal("ValidateInput returned nil error")
	}
}
