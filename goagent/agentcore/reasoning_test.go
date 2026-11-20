package agentcore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

func reasoningTool() *tools.Registry {
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "looked up"}, nil
		},
	})
	return registry
}

func reasoningResponses() []*ports.ChatResponse {
	return []*ports.ChatResponse{
		{Reasoning: "I should look it up", ToolCalls: []ports.ToolCall{{ID: "c1", Name: "lookup", Input: json.RawMessage(`{}`)}},
			Usage: ports.Usage{OutputTokens: 10, ReasoningTokens: 7}},
		{Reasoning: "the lookup answers it", Content: "final answer", Usage: ports.Usage{OutputTokens: 5, ReasoningTokens: 3}},
	}
}

func TestReasoningStaysOutOfOutputMemoryAndRequests(t *testing.T) {
	llm := &mockLLM{responses: reasoningResponses()}
	memory := &mockMemoryProvider{}
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(reasoningTool()), WithMemoryProvider(memory))
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.RunDetailed(context.Background(), RunRequest{SessionID: "s1", Input: "question"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "final answer" || result.Reasoning != "the lookup answers it" {
		t.Fatalf("result = %q / %q", result.Content, result.Reasoning)
	}
	if result.Usage.ReasoningTokens != 10 || result.Usage.OutputTokens != 15 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	for _, msg := range llm.requests[1].Messages {
		if msg.Reasoning != "" {
			t.Fatalf("reasoning replayed by default: %+v", msg)
		}
	}
	for _, msg := range memory.saved {
		if strings.Contains(msg.Content, "look it up") || strings.Contains(msg.Content, "answers it") {
			t.Fatalf("reasoning stored in memory: %+v", memory.saved)
		}
	}
}

func TestReasoningReplayCurrentTurn(t *testing.T) {
	llm := &mockLLM{responses: reasoningResponses()}
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(reasoningTool()), WithReasoningReplay(ReasoningReplayCurrentTurn))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), RunRequest{Input: "question"}); err != nil {
		t.Fatal(err)
	}
	var replayed []string
	for _, msg := range llm.requests[1].Messages {
		if msg.Reasoning != "" {
			replayed = append(replayed, msg.Role+":"+msg.Reasoning)
		}
	}
	if len(replayed) != 1 || replayed[0] != "assistant:I should look it up" {
		t.Fatalf("replayed = %v", replayed)
	}
}

func TestReplayViewKeepsOnlyTheCurrentTurn(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1", Reasoning: "old"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Reasoning: "current", ToolCalls: []ports.ToolCall{{ID: "c"}}},
		{Role: "tool", ToolCallID: "c", Content: "r", Reasoning: "not an assistant"},
	}
	view := replayView(messages, ReasoningReplayCurrentTurn)
	got := []string{}
	for _, msg := range view {
		got = append(got, msg.Reasoning)
	}
	if strings.Join(got, ",") != ",,,current," {
		t.Fatalf("current turn view = %q", got)
	}
	for _, msg := range replayView(messages, ReasoningReplayNone) {
		if msg.Reasoning != "" {
			t.Fatal("none mode kept reasoning")
		}
	}
	if messages[1].Reasoning != "old" {
		t.Fatal("replayView modified its input")
	}
}

type capturingProjector struct{ requests []ContextProjectionRequest }

func (p *capturingProjector) Project(_ context.Context, req ContextProjectionRequest) (*ContextProjectionResult, error) {
	p.requests = append(p.requests, req)
	return &ContextProjectionResult{Messages: req.Messages}, nil
}

func TestProjectorSeesOnlyReplayedReasoning(t *testing.T) {
	projector := &capturingProjector{}
	agent, err := NewAgent(WithLLM(&mockLLM{responses: reasoningResponses()}), WithToolRegistry(reasoningTool()), WithContextProjector(projector))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), RunRequest{Input: "question"}); err != nil {
		t.Fatal(err)
	}
	for _, request := range projector.requests {
		for _, msg := range request.Messages {
			if msg.Reasoning != "" {
				t.Fatalf("projector saw reasoning that is not sent: %+v", msg)
			}
		}
	}
}

func TestStreamDeliversReasoningDeltasApart(t *testing.T) {
	llm := &streamingMockLLM{mockLLM: mockLLM{responses: []*ports.ChatResponse{{Reasoning: "think", Content: "done"}}}}
	agent, err := NewAgent(WithLLM(llm))
	if err != nil {
		t.Fatal(err)
	}
	stream := agent.Stream(context.Background(), RunRequest{Input: "q"})
	var reasoning, content strings.Builder
	for event := range stream.Events {
		if event.Delta != nil {
			reasoning.WriteString(event.Delta.Reasoning)
			content.WriteString(event.Delta.Content)
		}
		if event.Done && (event.Error != nil || event.Result.Content != "done") {
			t.Fatalf("done = %+v", event)
		}
	}
	if reasoning.String() != "think" || content.String() != "done" {
		t.Fatalf("deltas = %q / %q", reasoning.String(), content.String())
	}
}

type streamingMockLLM struct{ mockLLM }

func (m *streamingMockLLM) ChatStream(ctx context.Context, req ports.ChatRequest, onDelta func(ports.ChatDelta) error) (*ports.ChatResponse, error) {
	resp, err := m.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	for _, delta := range ports.ResponseDeltas(resp) {
		if err := onDelta(delta); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// TestInlineThinkTagsNeverReachTheAnswer runs an agent against an
// OpenAI-compatible server whose model writes its thinking into the content,
// as Qwen3 served by Ollama does.
func TestInlineThinkTagsNeverReachTheAnswer(t *testing.T) {
	var bodies []string
	responses := []string{
		`{"choices":[{"message":{"content":"<think>call the tool</think>","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`{"choices":[{"message":{"content":"<think>\nall set\n</think>\n\nThe answer is 42."}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responses[len(bodies)-1])
	}))
	defer server.Close()
	client, err := openaiapi.New(openaiapi.Config{BaseURL: server.URL, Model: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(WithLLM(client), WithToolRegistry(reasoningTool()))
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.RunDetailed(context.Background(), RunRequest{Input: "what is it"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "The answer is 42." || result.Reasoning != "\nall set\n" {
		t.Fatalf("result = %q / %q", result.Content, result.Reasoning)
	}
	if strings.Contains(bodies[1], "call the tool") || strings.Contains(bodies[1], "think") {
		t.Fatalf("reasoning sent back: %s", bodies[1])
	}
}
