package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

type mockLLM struct {
	responses []*ports.ChatResponse
}

func (m *mockLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

// ChatStream makes mockLLM a ports.StreamingLLMClient: it streams the
// response's text word by word and each tool call as one piece before
// returning the response whole.
func (m *mockLLM) ChatStream(ctx context.Context, req ports.ChatRequest, onDelta func(ports.ChatDelta) error) (*ports.ChatResponse, error) {
	resp, err := m.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	for _, word := range strings.SplitAfter(resp.Content, " ") {
		if word == "" {
			continue
		}
		if err := onDelta(ports.ChatDelta{Content: word}); err != nil {
			return nil, err
		}
	}
	for i, call := range resp.ToolCalls {
		if err := onDelta(ports.ChatDelta{ToolCall: &ports.ToolCallDelta{Index: i, ID: call.ID, Name: call.Name, Arguments: string(call.Input)}}); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

type lookupTool struct{}

func (t lookupTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "lookup_account",
		Description: "Returns deterministic account information.",
		Permission:  policy.PermissionRead,
	}
}

func (t lookupTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	return &tools.Result{
		ForLLM:  "account acct_1 status: active",
		ForUser: "acct_1 active",
		Ref:     "account:acct_1",
	}, nil
}

func main() {
	ctx := context.Background()
	registry := tools.NewRegistry()
	registry.Register(lookupTool{})

	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{ID: "call_lookup", Name: "lookup_account", Input: json.RawMessage(`{"account_id":"acct_1"}`)}}},
			{Content: "Final answer: acct_1 is active."},
		}}),
		agentcore.WithToolRegistry(registry),
	)
	if err != nil {
		panic(err)
	}

	stream := agent.Stream(ctx, agentcore.RunRequest{Input: "Check account acct_1."})
	for event := range stream.Events {
		if delta := event.Delta; delta != nil {
			if delta.ToolCall != nil {
				fmt.Printf("stream=delta iteration=%d tool_call=%s args=%s\n", delta.Iteration, delta.ToolCall.Name, delta.ToolCall.Arguments)
			} else {
				fmt.Printf("stream=delta iteration=%d text=%q\n", delta.Iteration, delta.Content)
			}
			continue
		}
		if event.Done {
			fmt.Printf("stream=done llm=%d tools=%d\n", event.Result.ExecutionSummary.LLMCalls, event.Result.ExecutionSummary.ToolCalls)
			continue
		}
		if event.Event.Type == agentcore.EventToolCompleted {
			fmt.Printf("stream=%s tool=%v ref=%v\n", event.Event.Type, event.Event.Metadata["tool"], event.Event.Metadata["ref"])
		}
	}

	result, err := stream.Wait()
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content)
}
