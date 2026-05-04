package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

type denyAll struct{}

func (a denyAll) ApproveTool(ctx context.Context, req agentcore.ToolApprovalRequest) agentcore.ToolApprovalDecision {
	return agentcore.ToolApprovalDecision{Allowed: false, Reason: "operator rejected"}
}

type writeFileTool struct {
	ran bool
}

func (t *writeFileTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "write_file",
		Description: "Writes a deterministic file.",
		Permission:  policy.PermissionWrite,
		Schema: tools.Schema{
			JSONSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
		},
	}
}

func (t *writeFileTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	t.ran = true
	return &tools.Result{ForLLM: "file written", Ref: "file:notes.md"}, nil
}

func main() {
	ctx := context.Background()
	registry := tools.NewRegistry()
	writeTool := &writeFileTool{}
	registry.Register(writeTool)

	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{
				ToolCalls: []ports.ToolCall{{
					ID:    "call_write",
					Name:  "write_file",
					Input: json.RawMessage(`{"path":"notes.md"}`),
				}},
				Usage: ports.Usage{InputTokens: 8, OutputTokens: 3},
			},
		}}),
		agentcore.WithToolRegistry(registry),
		agentcore.WithToolApprover(denyAll{}),
	)
	if err != nil {
		panic(err)
	}

	stream := agent.Stream(ctx, agentcore.RunRequest{
		Input:              "Write notes.md.",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	for event := range stream.Events {
		switch event.Event.Type {
		case agentcore.EventApprovalRequested, agentcore.EventApprovalDenied:
			fmt.Printf("approval=%s tool=%v reason=%v\n", event.Event.Type, event.Event.Metadata["tool"], event.Event.Metadata["reason"])
		}
		if event.Done {
			fmt.Printf("stream=done llm=%d tools=%d abort=%q\n",
				event.Result.ExecutionSummary.LLMCalls,
				event.Result.ExecutionSummary.ToolCalls,
				event.Result.ExecutionSummary.AbortReason,
			)
		}
	}

	result, err := stream.Wait()
	if !errors.Is(err, agentcore.ErrApprovalDenied) {
		panic(err)
	}
	fmt.Printf("err=%v\n", agentcore.ErrApprovalDenied)
	fmt.Printf("tool_ran=%v\n", writeTool.ran)
	fmt.Printf("partial_run_id=%s\n", result.RunID)
}
