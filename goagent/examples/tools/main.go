package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/tools"
)

type mockLLM struct {
	responses []*ports.ChatResponse
}

func (m *mockLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

type userOutputs struct {
	mu     sync.Mutex
	values []string
}

func (o *userOutputs) append(value string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.values = append(o.values, value)
}

func (o *userOutputs) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	values := make([]string, len(o.values))
	copy(values, o.values)
	return values
}

type accountLookupTool struct {
	outputs *userOutputs
}

func (t accountLookupTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "account_lookup",
		Description: "Looks up deterministic account status.",
		Permission:  policy.PermissionRead,
		Timeout:     100 * time.Millisecond,
		Schema: tools.Schema{
			Validate: requireAccount,
		},
	}
}

func (t accountLookupTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	t.outputs.append("active")
	return &tools.Result{
		ForLLM:  "account status: active",
		ForUser: "active",
	}, nil
}

type auditTool struct{}

func (t auditTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "audit",
		Description: "Records a deterministic audit event.",
		Permission:  policy.PermissionRead,
		Timeout:     100 * time.Millisecond,
	}
}

func (t auditTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	return &tools.Result{
		ForLLM:  "audit recorded",
		ForUser: "audit recorded",
		Silent:  true,
	}, nil
}

func requireAccount(input json.RawMessage) error {
	var payload struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return err
	}
	if payload.Account == "" {
		return fmt.Errorf("account is required")
	}
	return nil
}

func main() {
	ctx := context.Background()
	outputs := &userOutputs{}
	registry := tools.NewRegistry()
	registry.Register(accountLookupTool{outputs: outputs})
	registry.Register(auditTool{})

	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "account_lookup", Input: json.RawMessage(`{"account":"demo"}`)}}},
			{ToolCalls: []ports.ToolCall{{Name: "audit", Input: json.RawMessage(`{}`)}}},
			{Content: "Final answer: the account is active."},
		}}),
		agentcore.WithToolRegistry(registry),
	)
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(ctx, agentcore.RunRequest{Input: "Check the demo account."})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content)
	for _, output := range outputs.snapshot() {
		fmt.Printf("User-visible tool output: %s\n", output)
	}
}
