package agentadapter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
)

func toolRun(results ...string) []agentcore.Message {
	messages := []agentcore.Message{{Role: "user", Content: "Find the staging database port."}}
	for i, result := range results {
		id := fmt.Sprint("c", i)
		messages = append(messages,
			agentcore.Message{Role: "assistant", Content: "let me look", Reasoning: "maybe 5432?", ToolCalls: []ports.ToolCall{{ID: id, Name: "read_file", Input: json.RawMessage(fmt.Sprintf(`{"path":"f%d"}`, i))}}},
			agentcore.Message{Role: "tool", ToolCallID: id, Content: result})
	}
	return append(messages, agentcore.Message{Role: "assistant", Content: "The staging database listens on 55432.", Reasoning: "done"})
}

func TestRunSourceKeepsToolFindingsAndLeavesOutGuesses(t *testing.T) {
	source := RunSource(toolRun("port: 55432 (staging)", "owner: platform team"), 4096)
	for _, want := range []string{
		"Request:\nFind the staging database port.",
		`- read_file {"path":"f0"} → port: 55432 (staging)`,
		`- read_file {"path":"f1"} → owner: platform team`,
		"Answer:\nThe staging database listens on 55432.",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("source lacks %q:\n%s", want, source)
		}
	}
	if strings.Contains(source, "maybe 5432") || strings.Contains(source, "let me look") {
		t.Fatalf("source keeps reasoning or interim text:\n%s", source)
	}
}

func TestRunSourceFitsTheLimit(t *testing.T) {
	big := strings.Repeat("值", 3000)
	for _, limit := range []int{50, 300, 1000, 4096} {
		for _, results := range [][]string{{big}, {big, big, big, big, big, big, big, big, big, big, big, big}, nil} {
			source := RunSource(toolRun(results...), limit)
			if n := utf8.RuneCountInString(source); n > limit || n == 0 {
				t.Fatalf("limit %d, %d results: %d runes", limit, len(results), n)
			}
			if limit >= 300 && !strings.Contains(source, "Answer:\nThe staging database listens on 55432.") {
				t.Fatalf("limit %d lost the answer:\n%s", limit, source)
			}
		}
	}
	many := make([]string, 40)
	for i := range many {
		many[i] = strings.Repeat("r", 500)
	}
	source := RunSource(toolRun(many...), 2000)
	if !strings.Contains(source, "earlier tool calls left out") || !strings.Contains(source, `{"path":"f39"}`) {
		t.Fatalf("many calls:\n%s", source)
	}
	if RunSource(nil, 0) != "" {
		t.Fatal("zero limit produced text")
	}
}
