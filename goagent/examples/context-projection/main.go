package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
)

type mockLLM struct {
	seenMessages int
}

func (m *mockLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	m.seenMessages = len(req.Messages)
	return &ports.ChatResponse{Content: "Final answer: projected context used."}, nil
}

type boundedProjector struct {
	maxChars int
}

func (p boundedProjector) Project(ctx context.Context, req agentcore.ContextProjectionRequest) (*agentcore.ContextProjectionResult, error) {
	projected := make([]agentcore.Message, 0, len(req.Messages))
	for _, msg := range req.Messages {
		content := msg.Content
		if msg.Role == "tool" && len(content) > p.maxChars {
			content = content[:p.maxChars] + "... ref=artifact:tool-output"
		}
		projected = append(projected, agentcore.Message{
			Role:       msg.Role,
			Content:    content,
			ToolCallID: msg.ToolCallID,
			ToolCalls:  msg.ToolCalls,
		})
	}
	projected = append([]agentcore.Message{{
		Role:    "system",
		Content: fmt.Sprintf("projection budget total tokens=%d", req.Budget.MaxTotalTokens),
	}}, projected...)
	return &agentcore.ContextProjectionResult{
		Messages: projected,
		Metadata: map[string]any{"projector": "bounded"},
	}, nil
}

func main() {
	ctx := context.Background()
	llm := &mockLLM{}
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(llm),
		agentcore.WithBudget(agentcore.Budget{MaxTotalTokens: 1200}),
		agentcore.WithPromptBlocks([]prompt.Block{{Name: "identity", Mode: prompt.ModeCacheable, Content: "Use the projected context."}}),
		agentcore.WithContextProjector(boundedProjector{maxChars: 48}),
	)
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(ctx, agentcore.RunRequest{Input: strings.Repeat("tool observation ", 12)})
	if err != nil {
		panic(err)
	}
	fmt.Printf("model saw %d messages\n", llm.seenMessages)
	fmt.Println(result.Content)
}
