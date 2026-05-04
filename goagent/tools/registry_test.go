package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type testTool struct {
	spec Spec
	run  func(ctx context.Context, input json.RawMessage, env Env) (*Result, error)
}

func (t testTool) Spec() Spec {
	return t.spec
}

func (t testTool) Execute(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
	return t.run(ctx, input, env)
}

func TestRegistryReturnsSpecsInNameOrder(t *testing.T) {
	registry := NewRegistry()
	registry.Register(testTool{spec: Spec{Name: "zeta"}})
	registry.Register(testTool{spec: Spec{Name: "alpha"}})

	specs := registry.Specs()
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d", len(specs))
	}
	if specs[0].Name != "alpha" || specs[1].Name != "zeta" {
		t.Fatalf("spec order = %v", specs)
	}
}

func TestRegistryMissingToolErrorIsClassifiable(t *testing.T) {
	registry := NewRegistry()

	_, err := registry.MustGet("missing")
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("err = %v, want ErrToolNotFound", err)
	}
}
