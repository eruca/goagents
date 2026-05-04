package prompt

import (
	"context"
	"testing"
)

func TestCompilerOrdersCacheableBeforeDynamicAndSortsPriority(t *testing.T) {
	compiler := NewCompiler()
	compiled, err := compiler.Compile(context.Background(), []Block{
		{Name: "dynamic-first", Mode: ModeDynamic, Priority: 1, Content: "dynamic first"},
		{Name: "cache-later", Mode: ModeCacheable, Priority: 20, Content: "cache later"},
		{Name: "cache-first-b", Mode: ModeCacheable, Priority: 1, Content: "cache first b"},
		{Name: "cache-first-a", Mode: ModeCacheable, Priority: 1, Content: "cache first a"},
	})
	if err != nil {
		t.Fatalf("Compile returned error: %v", err)
	}

	want := []string{"cache-first-a", "cache-first-b", "cache-later", "dynamic-first"}
	if len(compiled.Blocks) != len(want) {
		t.Fatalf("compiled blocks = %v", compiled.Blocks)
	}
	for i, name := range want {
		if compiled.Blocks[i].Name != name {
			t.Fatalf("block %d = %q, want %q", i, compiled.Blocks[i].Name, name)
		}
	}
}

func TestCompilerKeepsEmptyBlocksButOmitsEmptyContent(t *testing.T) {
	compiler := NewCompiler()
	compiled, err := compiler.Compile(context.Background(), []Block{
		{Name: "empty", Mode: ModeCacheable, Priority: 1},
		{Name: "filled", Mode: ModeCacheable, Priority: 2, Content: "filled content"},
	})
	if err != nil {
		t.Fatalf("Compile returned error: %v", err)
	}
	if len(compiled.Blocks) != 2 {
		t.Fatalf("blocks = %#v", compiled.Blocks)
	}
	if compiled.Content != "filled content" {
		t.Fatalf("content = %q", compiled.Content)
	}
}

func TestCompilerJoinsContentWithNewlines(t *testing.T) {
	compiler := NewCompiler()
	compiled, err := compiler.Compile(context.Background(), []Block{
		{Name: "b", Mode: ModeDynamic, Priority: 2, Content: "second"},
		{Name: "a", Mode: ModeCacheable, Priority: 1, Content: "first"},
	})
	if err != nil {
		t.Fatalf("Compile returned error: %v", err)
	}
	if compiled.Content != "first\nsecond" {
		t.Fatalf("content = %q", compiled.Content)
	}
}
