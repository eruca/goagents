package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
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

type supportModule struct{}

func (m supportModule) SystemPrompt(ctx context.Context, req agentcore.RunRequest) ([]prompt.Block, error) {
	return []prompt.Block{
		{Name: "identity", Mode: prompt.ModeCacheable, Priority: 1, Content: "You are a concise support assistant."},
	}, nil
}

func (m supportModule) Skills(ctx context.Context, req agentcore.RunRequest) ([]agentcore.Skill, error) {
	return []agentcore.Skill{
		{Name: "support-flow", Content: "Use lookup before answering account questions.", Priority: 2, Cacheable: true},
	}, nil
}

func (m supportModule) Tools(ctx context.Context, req agentcore.RunRequest) ([]tools.Tool, error) {
	return []tools.Tool{lookupTool{}}, nil
}

type lookupTool struct{}

func (t lookupTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "lookup",
		Description: "Returns deterministic account information.",
		Permission:  policy.PermissionRead,
	}
}

func (t lookupTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	return &tools.Result{ForLLM: "account status: active", ForUser: "active"}, nil
}

func main() {
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{"account":"demo"}`)}}},
			{Content: "Final answer: the account is active."},
		}}),
		agentcore.WithModule(supportModule{}),
	)
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(context.Background(), agentcore.RunRequest{Input: "Check the demo account."})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content)
}
