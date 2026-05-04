package main

import (
	"context"
	"encoding/json"
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

type approveAll struct{}

func (a approveAll) ApproveTool(ctx context.Context, req agentcore.ToolApprovalRequest) agentcore.ToolApprovalDecision {
	return agentcore.ToolApprovalDecision{Allowed: true, Reason: "operator approved"}
}

type updateDraftTool struct{}

func (t updateDraftTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "update_draft",
		Description: "Updates a deterministic draft note.",
		Permission:  policy.PermissionWrite,
		Schema: tools.Schema{
			JSONSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`),
		},
	}
}

func (t updateDraftTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	var req struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}
	return &tools.Result{
		ForLLM:  fmt.Sprintf("draft updated title=%q", req.Title),
		ForUser: req.Title,
		Ref:     "draft:demo",
	}, nil
}

func main() {
	ctx := context.Background()
	registry := tools.NewRegistry()
	registry.Register(updateDraftTool{})

	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{ID: "call_update", Name: "update_draft", Input: json.RawMessage(`{"title":"Approved draft"}`)}}},
			{Content: "Final answer: draft updated after approval."},
		}}),
		agentcore.WithToolRegistry(registry),
		agentcore.WithToolApprover(approveAll{}),
	)
	if err != nil {
		panic(err)
	}

	stream := agent.Stream(ctx, agentcore.RunRequest{
		Input:              "Update the draft title.",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	for event := range stream.Events {
		switch event.Event.Type {
		case agentcore.EventApprovalRequested, agentcore.EventApprovalCompleted:
			fmt.Printf("approval=%s tool=%v reason=%v\n", event.Event.Type, event.Event.Metadata["tool"], event.Event.Metadata["reason"])
		case agentcore.EventToolCompleted:
			fmt.Printf("tool=%v ref=%v\n", event.Event.Metadata["tool"], event.Event.Metadata["ref"])
		}
		if event.Done {
			fmt.Printf("stream=done llm=%d tools=%d\n", event.Result.ExecutionSummary.LLMCalls, event.Result.ExecutionSummary.ToolCalls)
		}
	}

	result, err := stream.Wait()
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content)
}
