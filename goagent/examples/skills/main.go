package main

import (
	"context"
	"encoding/json"
	"fmt"

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

type skillProvider struct{}

func (p skillProvider) Skills(ctx context.Context, req agentcore.RunRequest) ([]agentcore.Skill, error) {
	return []agentcore.Skill{
		{
			Name:        "tool-use-guide",
			Description: "Guidance for using read-only tools.",
			Content:     "Use lookup before answering factual questions, then summarize the result plainly.",
			Priority:    10,
			Cacheable:   true,
		},
	}, nil
}

type lookupTool struct{}

func (t lookupTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "lookup",
		Description: "Returns a deterministic lookup result.",
		Permission:  policy.PermissionRead,
	}
}

func (t lookupTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	return &tools.Result{
		ForLLM:  "lookup result: skills are prompt instructions and tools are executable actions.",
		ForUser: "Skills guide the model; tools perform actions.",
	}, nil
}

func main() {
	ctx := context.Background()
	registry := tools.NewRegistry()
	registry.Register(lookupTool{})

	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{"topic":"skills"}`)}}},
			{Content: "Final answer: Skills guide the model; tools perform actions."},
		}}),
		agentcore.WithSkillProvider(skillProvider{}),
		agentcore.WithToolRegistry(registry),
	)
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(ctx, agentcore.RunRequest{Input: "Explain skills and tools."})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content)
}
