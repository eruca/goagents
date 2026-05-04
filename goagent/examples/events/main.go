package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/memory"
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

type printSink struct{}

func (s printSink) Emit(ctx context.Context, event agentcore.Event) error {
	fmt.Printf("event=%s stage=%s iteration=%d metadata=%s\n", event.Type, event.Stage, event.Iteration, formatMetadata(event.Metadata))
	return nil
}

func formatMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, metadata[key]))
	}
	return strings.Join(parts, ",")
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
	ctx := context.Background()
	registry := tools.NewRegistry()
	registry.Register(lookupTool{})

	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{"account":"demo"}`)}}},
			{Content: "Final answer: the account is active."},
		}}),
		agentcore.WithToolRegistry(registry),
		agentcore.WithMemoryProvider(memory.NewWindowMemory(8)),
		agentcore.WithEventSink(printSink{}),
	)
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(ctx, agentcore.RunRequest{SessionID: "demo-session", Input: "Check the demo account."})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content)
}
