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

type lookupTool struct{}

func (t lookupTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "lookup",
		Description: "Returns a deterministic read result.",
		Permission:  policy.PermissionRead,
	}
}

func (t lookupTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	return &tools.Result{ForLLM: "read allowed"}, nil
}

type writeTool struct {
	called *bool
}

func (t writeTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "write_file",
		Description: "Demonstrates a write action denied by default policy.",
		Permission:  policy.PermissionWrite,
	}
}

func (t writeTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	*t.called = true
	return &tools.Result{ForLLM: "wrote"}, nil
}

func main() {
	ctx := context.Background()
	readRegistry := tools.NewRegistry()
	readRegistry.Register(lookupTool{})
	readAgent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}}},
			{Content: "read allowed"},
		}}),
		agentcore.WithToolRegistry(readRegistry),
	)
	if err != nil {
		panic(err)
	}
	readResult, err := readAgent.Run(ctx, agentcore.RunRequest{Input: "Read account status."})
	if err != nil {
		panic(err)
	}
	fmt.Println(readResult.Content)

	writeCalled := false
	writeRegistry := tools.NewRegistry()
	writeRegistry.Register(writeTool{called: &writeCalled})
	writeAgent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "write_file", Input: json.RawMessage(`{}`)}}},
		}}),
		agentcore.WithToolRegistry(writeRegistry),
	)
	if err != nil {
		panic(err)
	}
	_, err = writeAgent.Run(ctx, agentcore.RunRequest{Input: "Write a file."})
	if err != nil && !writeCalled {
		fmt.Println("write denied")
	} else {
		panic("write tool was not denied")
	}

	allowedWriteCalled := false
	allowedWriteRegistry := tools.NewRegistry()
	allowedWriteRegistry.Register(writeTool{called: &allowedWriteCalled})
	allowedWriteAgent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "write_file", Input: json.RawMessage(`{}`)}}},
			{Content: "write allowed"},
		}}),
		agentcore.WithToolRegistry(allowedWriteRegistry),
	)
	if err != nil {
		panic(err)
	}
	allowedWriteResult, err := allowedWriteAgent.Run(ctx, agentcore.RunRequest{
		Input:              "Write a file.",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
		PolicyContext: ports.PolicyContext{
			RequestID: "example-request",
		},
	})
	if err != nil {
		panic(err)
	}
	if !allowedWriteCalled {
		panic("write tool was not called")
	}
	fmt.Println(allowedWriteResult.Content)
}
