package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
	"github.com/eruca/goagents/goagent/tools"
)

type mockLLM struct {
	requests        []ports.ChatRequest
	maxOutputTokens []int
	responses       []*ports.ChatResponse
}

func (m *mockLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	m.requests = append(m.requests, req)
	m.maxOutputTokens = append(m.maxOutputTokens, req.MaxOutputTokens)
	if len(m.responses) == 0 {
		return &ports.ChatResponse{}, nil
	}
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

func TestReActFinalAnswerStopsWithFinalResult(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "done"}}}
	state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
	runner := NewReActRunner(ReActConfig{
		LLM:            llm,
		PromptCompiler: prompt.NewCompiler(),
		PromptBlocks:   []prompt.Block{{Name: "system", Mode: prompt.ModeCacheable, Content: "be brief"}},
		ToolRegistry:   tools.NewRegistry(),
		PolicyEngine:   policy.NewEngine(),
		MaxIterations:  3,
	})

	result, err := runner.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result != StageBreak {
		t.Fatalf("result = %v", result)
	}
	if state.Final == nil || state.Final.Content != "done" {
		t.Fatalf("final = %#v", state.Final)
	}
}

func TestReActToolCallObservesThenReturnsFinalAnswer(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{"q":"go"}`)}}},
		{Content: "observed answer"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			if string(input) != `{"q":"go"}` {
				t.Fatalf("input = %s", input)
			}
			return &tools.Result{ForLLM: "observation text", ForUser: "shown to user"}, nil
		},
	})
	state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
	runner := NewReActRunner(ReActConfig{
		LLM:            llm,
		PromptCompiler: prompt.NewCompiler(),
		ToolRegistry:   registry,
		PolicyEngine:   policy.NewEngine(),
		MaxIterations:  3,
	})

	result, err := runner.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result != StageBreak {
		t.Fatalf("result = %v", result)
	}
	if state.Final == nil || state.Final.Content != "observed answer" {
		t.Fatalf("final = %#v", state.Final)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("LLM requests = %d", len(llm.requests))
	}
	if got := llm.requests[1].Messages[len(llm.requests[1].Messages)-1].Content; got != "observation text" {
		t.Fatalf("last LLM message = %q", got)
	}
}

func TestAgentRunAllowsRequestScopedWritePermission(t *testing.T) {
	registry := tools.NewRegistry()
	calls := 0
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write_note", Permission: policy.PermissionWrite},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			calls++
			return &tools.Result{ForLLM: "note written"}, nil
		},
	})

	deniedAgent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "write_note", Input: json.RawMessage(`{"note":"draft"}`)}}},
		}}),
		WithToolRegistry(registry),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}
	if _, err := deniedAgent.Run(context.Background(), RunRequest{Input: "write the note"}); err == nil {
		t.Fatal("Run returned nil error")
	}
	if calls != 0 {
		t.Fatalf("write tool calls = %d, want 0", calls)
	}

	allowedAgent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "write_note", Input: json.RawMessage(`{"note":"draft"}`)}}},
			{Content: "done"},
		}}),
		WithToolRegistry(registry),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}
	result, err := allowedAgent.Run(context.Background(), RunRequest{
		Input:              "write the note",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Content != "done" {
		t.Fatalf("content = %q", result.Content)
	}
	if calls != 1 {
		t.Fatalf("write tool calls = %d, want 1", calls)
	}
}

func TestReActPromptMessageOrderIncludesSystemMemoryUserAndToolObservation(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}}},
		{Content: "done"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "observation"}, nil
		},
	})
	state := NewRunState(NewRunID(), RunRequest{Input: "current"})
	state.Messages = append(state.Messages, Message{Role: "assistant", Content: "remembered"})
	runner := NewReActRunner(ReActConfig{
		LLM:            llm,
		PromptCompiler: prompt.NewCompiler(),
		PromptBlocks:   []prompt.Block{{Name: "system", Mode: prompt.ModeCacheable, Content: "system"}},
		ToolRegistry:   registry,
		PolicyEngine:   policy.NewEngine(),
		MaxIterations:  3,
	})

	_, err := runner.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	first := llm.requests[0].Messages
	if got := []string{first[0].Role, first[1].Role, first[2].Role}; got[0] != "system" || got[1] != "assistant" || got[2] != "user" {
		t.Fatalf("first request roles = %v", got)
	}
	second := llm.requests[1].Messages
	if second[len(second)-1].Role != "tool" || second[len(second)-1].Content != "observation" {
		t.Fatalf("second request messages = %#v", second)
	}
}

func TestReActPreservesToolCallIDInNextModelRequest(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{
			Content: "I will look it up.",
			ToolCalls: []ports.ToolCall{{
				ID:    "call_lookup_1",
				Name:  "lookup",
				Input: json.RawMessage(`{"q":"go"}`),
			}},
		},
		{Content: "done"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "observation"}, nil
		},
	})
	state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
	runner := NewReActRunner(ReActConfig{
		LLM:            llm,
		PromptCompiler: prompt.NewCompiler(),
		ToolRegistry:   registry,
		PolicyEngine:   policy.NewEngine(),
		MaxIterations:  3,
	})

	_, err := runner.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	second := llm.requests[1].Messages
	if len(second) < 3 {
		t.Fatalf("second request messages = %#v", second)
	}
	assistant := second[len(second)-2]
	if assistant.Role != "assistant" || assistant.Content != "I will look it up." {
		t.Fatalf("assistant tool-call message = %#v", assistant)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_lookup_1" {
		t.Fatalf("assistant tool calls = %#v", assistant.ToolCalls)
	}
	toolMessage := second[len(second)-1]
	if toolMessage.Role != "tool" || toolMessage.ToolCallID != "call_lookup_1" || toolMessage.Content != "observation" {
		t.Fatalf("tool message = %#v", toolMessage)
	}
}

func TestSelfCorrectionObservesRecoverableToolError(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{"q":1}`)}}},
		{Content: "corrected"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "schema error: q must be a string", IsError: true}, nil
		},
	})
	state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
	runner := NewReActRunner(ReActConfig{
		LLM:            llm,
		PromptCompiler: prompt.NewCompiler(),
		ToolRegistry:   registry,
		PolicyEngine:   policy.NewEngine(),
		MaxIterations:  3,
	})

	_, err := runner.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("LLM requests = %d", len(llm.requests))
	}
	got := llm.requests[1].Messages[len(llm.requests[1].Messages)-1].Content
	if got != "Tool lookup returned a recoverable error: schema error: q must be a string. Correct the arguments and try again." {
		t.Fatalf("last LLM message = %q", got)
	}
}

func TestSilentToolResultKeepsToolCallPairingWithoutContent(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call_audit", Name: "audit", Input: json.RawMessage(`{}`)}}},
		{Content: "done"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "audit", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "internal audit trail", Silent: true}, nil
		},
	})
	state := NewRunState(NewRunID(), RunRequest{Input: "hello"})
	runner := NewReActRunner(ReActConfig{
		LLM:            llm,
		PromptCompiler: prompt.NewCompiler(),
		ToolRegistry:   registry,
		PolicyEngine:   policy.NewEngine(),
		MaxIterations:  3,
	})

	_, err := runner.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("LLM requests = %d", len(llm.requests))
	}
	var toolMessages []ports.ChatMessage
	for _, message := range llm.requests[1].Messages {
		if message.Role == "tool" {
			toolMessages = append(toolMessages, message)
		}
	}
	if len(toolMessages) != 1 || toolMessages[0].ToolCallID != "call_audit" {
		t.Fatalf("tool messages = %#v, want one reply to call_audit", toolMessages)
	}
	if strings.Contains(toolMessages[0].Content, "internal audit trail") {
		t.Fatalf("silent result leaked into model context: %q", toolMessages[0].Content)
	}
}

type testAgentTool struct {
	spec tools.Spec
	run  func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error)
}

func (t testAgentTool) Spec() tools.Spec {
	return t.spec
}

func (t testAgentTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	return t.run(ctx, input, env)
}

func lastToolReply(t *testing.T, req ports.ChatRequest, callID string) ports.ChatMessage {
	t.Helper()
	for _, message := range req.Messages {
		if message.Role == "tool" && message.ToolCallID == callID {
			return message
		}
	}
	t.Fatalf("no tool reply for %s in %#v", callID, req.Messages)
	return ports.ChatMessage{}
}

func TestReActAnswersUnknownToolCallSoModelCanRecover(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call_bad", Name: "serach", Input: json.RawMessage(`{}`)}}},
		{Content: "done"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "search", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "results"}, nil
		},
	})
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry), WithEventSink(sink))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Content != "done" || len(llm.requests) != 2 {
		t.Fatalf("result = %#v, LLM requests = %d", result, len(llm.requests))
	}
	reply := lastToolReply(t, llm.requests[1], "call_bad")
	if !strings.Contains(reply.Content, `"serach" is not available`) || !strings.Contains(reply.Content, "search") {
		t.Fatalf("reply = %q, want unknown tool with available names", reply.Content)
	}
	if !sink.hasEvent(EventToolRejected, result.RunID) {
		t.Fatalf("events = %#v, want tool.rejected", sink.events)
	}
}

func TestReActAnswersSchemaInvalidArgumentsWithoutRunningTool(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{
			{ID: "call_bad", Name: "lookup", Input: json.RawMessage(`{"q":1}`)},
			{ID: "call_ok", Name: "lookup", Input: json.RawMessage(`{"q":"go"}`)},
		}},
		{Content: "done"},
	}}
	var executed []string
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{
			Name:       "lookup",
			Permission: policy.PermissionRead,
			Schema:     tools.Schema{JSONSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)},
		},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			executed = append(executed, string(input))
			return &tools.Result{ForLLM: "found"}, nil
		},
	})
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	if _, err := agent.Run(context.Background(), RunRequest{Input: "hello"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(executed) != 1 || executed[0] != `{"q":"go"}` {
		t.Fatalf("executed = %v, want only the valid call", executed)
	}
	bad := lastToolReply(t, llm.requests[1], "call_bad")
	if !strings.Contains(bad.Content, "Tool lookup returned a recoverable error") || !strings.Contains(bad.Content, "Correct the arguments") {
		t.Fatalf("bad reply = %q", bad.Content)
	}
	if ok := lastToolReply(t, llm.requests[1], "call_ok"); ok.Content != "found" {
		t.Fatalf("ok reply = %q", ok.Content)
	}
}

func TestReActStillAbortsOnBrokenToolSchema(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call_1", Name: "broken", Input: json.RawMessage(`{}`)}}},
		{Content: "done"},
	}}
	// A configured tool's schema is checked by NewAgent; a request-scoped tool
	// is only seen at run time.
	broken := testAgentTool{
		spec: tools.Spec{
			Name:       "broken",
			Permission: policy.PermissionRead,
			Schema:     tools.Schema{JSONSchema: json.RawMessage(`{"type":"nope"}`)},
		},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			t.Fatal("tool with broken schema ran")
			return nil, nil
		},
	}
	agent, err := NewAgent(WithLLM(llm), WithToolProvider(staticToolProvider{tools: []tools.Tool{broken}}))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	if _, err := agent.Run(context.Background(), RunRequest{Input: "hello"}); !errors.Is(err, tools.ErrToolSchemaInvalid) {
		t.Fatalf("Run err = %v, want ErrToolSchemaInvalid", err)
	}
}
