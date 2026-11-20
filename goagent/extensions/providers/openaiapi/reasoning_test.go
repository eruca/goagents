package openaiapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
)

func jsonServer(t *testing.T, response string, check func(body []byte)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if check != nil {
			check(body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	return server
}

func chatJSON(message string) string {
	return `{"choices":[{"message":` + message + `}],"usage":{"prompt_tokens":3,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":6}}}`
}

func TestChatSeparatesReasoning(t *testing.T) {
	cases := []struct {
		name, message, content, reasoning string
		keep                              bool
	}{
		{"reasoning_content field", `{"content":"answer","reasoning_content":"thought"}`, "answer", "thought", false},
		{"reasoning field", `{"content":"answer","reasoning":"thought"}`, "answer", "thought", false},
		{"inline think block", `{"content":"<think>thought</think>\n\nanswer"}`, "answer", "thought", false},
		{"template-opened block", `{"content":"thought\n</think>\nanswer"}`, "answer", "thought\n", false},
		{"kept tags", `{"content":"<think>thought</think>answer"}`, "<think>thought</think>answer", "", true},
		{"field wins over tags", `{"content":"<think>x</think>answer","reasoning_content":"thought"}`, "<think>x</think>answer", "thought", false},
		{"no reasoning", `{"content":"answer"}`, "answer", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := jsonServer(t, chatJSON(tc.message), nil)
			client, err := New(Config{BaseURL: server.URL, Model: "m", KeepThinkTags: tc.keep})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Chat(context.Background(), ports.ChatRequest{Messages: []ports.ChatMessage{{Role: "user", Content: "q"}}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Content != tc.content || resp.Reasoning != tc.reasoning {
				t.Fatalf("content %q reasoning %q; want %q, %q", resp.Content, resp.Reasoning, tc.content, tc.reasoning)
			}
			if resp.Usage.ReasoningTokens != 6 || resp.Usage.OutputTokens != 9 {
				t.Fatalf("usage = %+v", resp.Usage)
			}
		})
	}
}

func TestReasoningIsSentBackOnlyWhenConfigured(t *testing.T) {
	request := ports.ChatRequest{Messages: []ports.ChatMessage{
		{Role: "user", Content: "q"},
		{Role: "assistant", Reasoning: "plan", ToolCalls: []ports.ToolCall{{ID: "c1", Name: "t", Input: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "c1", Content: "r", Reasoning: "never on a tool message"},
	}}
	for _, send := range []bool{false, true} {
		var body string
		server := jsonServer(t, chatJSON(`{"content":"ok"}`), func(b []byte) { body = string(b) })
		client, _ := New(Config{BaseURL: server.URL, Model: "m", SendReasoning: send})
		if _, err := client.Chat(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(body, `"reasoning_content"`); (send && got != 1) || (!send && got != 0) {
			t.Fatalf("send=%v: request %s", send, body)
		}
		if strings.Contains(body, "never on a tool message") {
			t.Fatal("reasoning sent on a tool message")
		}
	}
}

// streamEvents splits content into chunks of the given size.
func streamEvents(content string, size int, extra ...string) []string {
	var events []string
	for start := 0; start < len(content); start += size {
		end := min(start+size, len(content))
		chunk, _ := json.Marshal(content[start:end])
		events = append(events, `{"choices":[{"delta":{"content":`+string(chunk)+`}}]}`)
	}
	events = append(events, extra...)
	return append(events, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`, `[DONE]`)
}

func TestChatStreamSplitsThinkTagsAcrossChunks(t *testing.T) {
	for _, content := range []string{
		"<think>\nplan the steps\n</think>\n\nthe answer",
		"  <think>a</think>b",
		"<think>unfinished",
		"plain answer with </thin and <think",
		"<think></think>answer",
		"thought\n</think>\nanswer",
	} {
		for size := 1; size <= len(content); size++ {
			var reasoning, text strings.Builder
			server := sseServer(t, streamEvents(content, size), nil)
			client, _ := New(Config{BaseURL: server.URL, Model: "m"})
			resp, err := client.ChatStream(context.Background(), ports.ChatRequest{Messages: []ports.ChatMessage{{Role: "user", Content: "q"}}}, func(delta ports.ChatDelta) error {
				reasoning.WriteString(delta.Reasoning)
				text.WriteString(delta.Content)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			wantReasoning, wantContent := ports.SplitThinkTags(content)
			if resp.Content != wantContent || resp.Reasoning != wantReasoning {
				t.Fatalf("%q in %d-byte chunks: content %q reasoning %q; want %q, %q", content, size, resp.Content, resp.Reasoning, wantContent, wantReasoning)
			}
			// The template-opened form is only recognized at the end, after
			// its reasoning streamed as content.
			if !strings.HasPrefix(content, "thought") && (text.String() != resp.Content || reasoning.String() != resp.Reasoning) {
				t.Fatalf("%q in %d-byte chunks: deltas %q / %q, response %q / %q", content, size, text.String(), reasoning.String(), resp.Content, resp.Reasoning)
			}
		}
	}
}

func TestChatStreamKeepsReasoningFieldDeltasApart(t *testing.T) {
	var deltas []ports.ChatDelta
	server := sseServer(t, streamEvents("answer", 3,
		`{"choices":[{"delta":{"reasoning_content":"thou"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"ght"}}]}`), nil)
	client, _ := New(Config{BaseURL: server.URL, Model: "m"})
	resp, err := client.ChatStream(context.Background(), ports.ChatRequest{Messages: []ports.ChatMessage{{Role: "user", Content: "q"}}}, func(delta ports.ChatDelta) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "answer" || resp.Reasoning != "thought" {
		t.Fatalf("response = %+v", resp)
	}
	for _, delta := range deltas {
		if delta.Content != "" && delta.Reasoning != "" {
			t.Fatalf("delta mixes content and reasoning: %+v", delta)
		}
	}
}
