package ports

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestToolSchemaCompilesOncePerSource(t *testing.T) {
	schema := ToolSchema{JSONSchema: json.RawMessage(`{"type":"object","required":["q"],"properties":{"q":{"type":"string"}}}`)}
	if err := schema.ValidateInput(json.RawMessage(`{"q":"x"}`)); err != nil {
		t.Fatalf("ValidateInput(valid) = %v", err)
	}
	first, _ := compileJSONSchema(schema.JSONSchema)
	second, _ := compileJSONSchema(schema.JSONSchema)
	if first == nil || first != second {
		t.Fatal("schema compiled twice, want the cached compilation")
	}
	if err := schema.ValidateInput(json.RawMessage(`{}`)); !errors.Is(err, ErrToolInputInvalid) {
		t.Fatalf("ValidateInput(missing field) = %v, want ErrToolInputInvalid", err)
	}
	if err := (ToolSchema{JSONSchema: json.RawMessage(`{"type": 42}`)}).Compile(); !errors.Is(err, ErrToolSchemaInvalid) {
		t.Fatalf("Compile(broken) = %v, want ErrToolSchemaInvalid", err)
	}
}
