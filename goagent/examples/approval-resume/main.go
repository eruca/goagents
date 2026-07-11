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

func (m *mockLLM) Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
	response := m.responses[0]
	m.responses = m.responses[1:]
	return response, nil
}

type pauseForApproval struct{}

func (pauseForApproval) ApproveTool(context.Context, agentcore.ToolApprovalRequest) agentcore.ToolApprovalDecision {
	return agentcore.ToolApprovalDecision{Pending: true, Reason: "operator review required"}
}

type updateDraftTool struct {
	runs *int
}

func (t updateDraftTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "update_draft",
		Description: "Updates a deterministic draft note.",
		Permission:  policy.PermissionWrite,
	}
}

func (t updateDraftTool) Execute(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
	*t.runs++
	return &tools.Result{ForLLM: "draft updated", Ref: "draft:demo"}, nil
}

func newAgent(llm ports.LLMClient, tool tools.Tool) (*agentcore.Agent, error) {
	registry := tools.NewRegistry()
	registry.Register(tool)
	return agentcore.NewAgent(
		agentcore.WithLLM(llm),
		agentcore.WithToolRegistry(registry),
		agentcore.WithToolApprover(pauseForApproval{}),
	)
}

func main() {
	ctx := context.Background()
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "call_update", Name: "update_draft", Input: json.RawMessage(`{"title":"Approved draft"}`)}}},
		{Content: "Final answer: draft updated after approval."},
	}}
	toolRuns := 0

	initialAgent, err := newAgent(llm, updateDraftTool{runs: &toolRuns})
	if err != nil {
		panic(err)
	}
	paused, err := initialAgent.RunDetailed(ctx, agentcore.RunRequest{
		Input:              "Update the draft title.",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	if !errors.Is(err, agentcore.ErrApprovalPending) {
		panic(err)
	}

	// The host owns encrypted storage and expiration. JSON here simulates reading it after a restart.
	stored, err := json.Marshal(paused.Interruption.Checkpoint)
	if err != nil {
		panic(err)
	}
	var checkpoint agentcore.RunCheckpoint
	if err := json.Unmarshal(stored, &checkpoint); err != nil {
		panic(err)
	}
	fmt.Printf("paused run=%s calls=%d checkpoint_bytes=%d\n", paused.RunID, len(checkpoint.PendingCalls), len(stored))

	// Rebuild the Agent and its tool registry as a new host process would.
	resumedAgent, err := newAgent(llm, updateDraftTool{runs: &toolRuns})
	if err != nil {
		panic(err)
	}
	result, err := resumedAgent.ResumeDetailed(ctx, checkpoint, []agentcore.ToolApprovalResolution{{
		Index:      0,
		ToolCallID: checkpoint.PendingCalls[0].ID,
		Tool:       checkpoint.PendingCalls[0].Name,
		Allowed:    true,
		Reason:     "operator approved",
	}})
	if err != nil {
		panic(err)
	}
	fmt.Printf("resumed llm=%d tools=%d tool_ran=%d\n", result.ExecutionSummary.LLMCalls, result.ExecutionSummary.ToolCalls, toolRuns)
	fmt.Println(result.Content)
}
