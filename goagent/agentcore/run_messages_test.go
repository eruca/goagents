package agentcore

import (
	"context"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
)

func TestRunResultCarriesThisRunsMessagesOnly(t *testing.T) {
	llm := &mockLLM{responses: reasoningResponses()}
	memory := &mockMemoryProvider{loaded: []ports.MemoryMessage{{Role: "user", Content: "old question"}, {Role: "assistant", Content: "old answer"}}}
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(reasoningTool()), WithMemoryProvider(memory))
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.RunDetailed(context.Background(), RunRequest{SessionID: "s", Input: "question"})
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, msg := range result.Messages {
		roles = append(roles, msg.Role+":"+msg.Content)
	}
	want := []string{"user:question", "assistant:", "tool:looked up", "assistant:final answer"}
	if len(roles) != len(want) {
		t.Fatalf("messages = %q", roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("messages = %q", roles)
		}
	}
	if result.Messages[1].ToolCalls[0].Name != "lookup" || result.Messages[3].Reasoning != "the lookup answers it" {
		t.Fatalf("messages = %+v", result.Messages)
	}
}
