package memory

import (
	"context"
	"testing"

	"github.com/eruca/goagent/ports"
)

func TestWindowMemoryDropsOlderMessagesPastLimit(t *testing.T) {
	provider := NewWindowMemory(2)
	err := provider.Save(context.Background(), "session_1", []ports.MemoryMessage{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"},
	})
	if err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	messages, err := provider.Load(context.Background(), "session_1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d", len(messages))
	}
	if messages[0].Content != "two" || messages[1].Content != "three" {
		t.Fatalf("messages = %#v", messages)
	}
}
