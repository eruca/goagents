package main

import (
	"context"
	"fmt"

	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/memory"
	"github.com/eruca/goagent/ports"
)

type recordingLLM struct {
	requests  []ports.ChatRequest
	responses []*ports.ChatResponse
}

func (m *recordingLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	m.requests = append(m.requests, req)
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

func main() {
	ctx := context.Background()
	llm := &recordingLLM{responses: []*ports.ChatResponse{
		{Content: "First answer: remembered account status."},
		{Content: "Second answer: used remembered context."},
	}}
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(llm),
		agentcore.WithMemoryProvider(memory.NewWindowMemory(8)),
	)
	if err != nil {
		panic(err)
	}

	first, err := agent.Run(ctx, agentcore.RunRequest{
		SessionID: "demo-session",
		Input:     "Remember the demo account status.",
	})
	if err != nil {
		panic(err)
	}

	second, err := agent.Run(ctx, agentcore.RunRequest{
		SessionID: "demo-session",
		Input:     "Use what you remember.",
	})
	if err != nil {
		panic(err)
	}

	fmt.Println(first.Content)
	fmt.Println(second.Content)
	fmt.Printf("Second run saw messages: %d\n", len(llm.requests[1].Messages))
}
