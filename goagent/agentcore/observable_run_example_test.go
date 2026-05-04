package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/eruca/goagent/policy"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/tools"
)

func TestObservableRunExampleAllowsReadToolAndAuditsRun(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{
			Name:       "query_accounts",
			Permission: policy.PermissionRead,
		},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{
				ForLLM: "3 active accounts found",
				Ref:    "artifact:accounts-1",
			}, nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{
			ToolCalls: []ports.ToolCall{{
				ID:    "call_query_accounts",
				Name:  "query_accounts",
				Input: json.RawMessage(`{"status":"active"}`),
			}},
			Usage: ports.Usage{InputTokens: 10, OutputTokens: 4},
		},
		{
			Content: "active accounts were found.",
			Usage:   ports.Usage{InputTokens: 12, OutputTokens: 5},
		},
	}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.RunDetailed(context.Background(), RunRequest{
		Input: "查一下 active accounts，并给我总结。",
	})
	if err != nil {
		t.Fatalf("RunDetailed returned error: %v", err)
	}

	if result.Content != "active accounts were found." {
		t.Fatalf("Content = %q", result.Content)
	}
	if result.ExecutionSummary.LLMCalls != 2 {
		t.Fatalf("LLMCalls = %d, want 2", result.ExecutionSummary.LLMCalls)
	}
	if result.ExecutionSummary.ToolCalls != 1 {
		t.Fatalf("ToolCalls = %d, want 1", result.ExecutionSummary.ToolCalls)
	}
	if len(result.ExecutionSummary.UsedTools) != 1 || result.ExecutionSummary.UsedTools[0] != "query_accounts" {
		t.Fatalf("UsedTools = %#v", result.ExecutionSummary.UsedTools)
	}
	if result.Usage.InputTokens != 22 || result.Usage.OutputTokens != 9 {
		t.Fatalf("Usage = %#v", result.Usage)
	}
	if result.ExecutionSummary.AbortReason != "" {
		t.Fatalf("AbortReason = %q, want empty", result.ExecutionSummary.AbortReason)
	}
}

func TestObservableRunExampleDeniesWriteToolBeforeExecution(t *testing.T) {
	toolRan := false
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{
			Name:       "write_file",
			Permission: policy.PermissionWrite,
		},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			toolRan = true
			return &tools.Result{ForLLM: "wrote file"}, nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{
			ToolCalls: []ports.ToolCall{{
				ID:    "call_write_file",
				Name:  "write_file",
				Input: json.RawMessage(`{"path":"notes.md"}`),
			}},
			Usage: ports.Usage{InputTokens: 8, OutputTokens: 3},
		},
	}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.RunDetailed(context.Background(), RunRequest{
		Input: "把结果写入 notes.md。",
	})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
	if toolRan {
		t.Fatal("write tool ran after policy denial")
	}
	if result == nil {
		t.Fatal("RunDetailed returned nil result")
	}
	if result.ExecutionSummary.LLMCalls != 1 {
		t.Fatalf("LLMCalls = %d, want 1", result.ExecutionSummary.LLMCalls)
	}
	if result.ExecutionSummary.ToolCalls != 0 {
		t.Fatalf("ToolCalls = %d, want 0", result.ExecutionSummary.ToolCalls)
	}
	if result.ExecutionSummary.AbortReason == "" {
		t.Fatal("AbortReason is empty")
	}
}
