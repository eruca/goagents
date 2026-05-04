package agentcore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/prompt"
	"github.com/eruca/goagent/tools"
)

type mockLLM struct {
	requests  []ports.ChatRequest
	responses []*ports.ChatResponse
}

func (m *mockLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	m.requests = append(m.requests, req)
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

func TestSilentToolResultDoesNotAppendObservation(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "audit", Input: json.RawMessage(`{}`)}}},
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
	for _, message := range llm.requests[1].Messages {
		if message.Role == "tool" {
			t.Fatalf("unexpected tool observation: %#v", message)
		}
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
