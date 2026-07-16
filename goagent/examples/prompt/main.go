package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
	"github.com/eruca/goagents/goagent/tools"
)

type recordingLLM struct {
	requests []ports.ChatRequest
}

func (l *recordingLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	l.requests = append(l.requests, req)
	return &ports.ChatResponse{Content: "Final answer: prompt assembled."}, nil
}

type promptModule struct{}

func (m promptModule) SystemPrompt(ctx context.Context, req agentcore.RunRequest) ([]prompt.Block, error) {
	return []prompt.Block{
		{Name: "module", Mode: prompt.ModeCacheable, Priority: 2, Content: "Module prompt label."},
	}, nil
}

func (m promptModule) Skills(ctx context.Context, req agentcore.RunRequest) ([]agentcore.Skill, error) {
	return []agentcore.Skill{
		{Name: "prompt-skill", Content: "Skill prompt label.", Priority: 3, Cacheable: true},
	}, nil
}

func (m promptModule) Tools(ctx context.Context, req agentcore.RunRequest) ([]tools.Tool, error) {
	return nil, nil
}

func main() {
	llm := &recordingLLM{}
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(llm),
		agentcore.WithPromptBlocks([]prompt.Block{
			{Name: "static", Mode: prompt.ModeCacheable, Priority: 1, Content: "Static prompt label."},
		}),
		agentcore.WithModule(promptModule{}),
	)
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(context.Background(), agentcore.RunRequest{Input: "Show prompt assembly."})
	if err != nil {
		panic(err)
	}
	if len(llm.requests) == 0 {
		panic("LLM request was not recorded")
	}

	messages := llm.requests[0].Messages
	for i, message := range messages {
		fmt.Printf("message[%d]=%s\n", i, message.Role)
	}

	system := ""
	if len(messages) > 0 && messages[0].Role == "system" {
		system = messages[0].Content
	}
	fmt.Printf(
		"system contains static=%t module=%t skill=%t\n",
		strings.Contains(system, "Static prompt label."),
		strings.Contains(system, "Module prompt label."),
		strings.Contains(system, "Skill prompt label."),
	)
	fmt.Println(result.Content)
}
