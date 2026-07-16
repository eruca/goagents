package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
	"github.com/eruca/goagents/goagent/tools"
)

type accountLookupTool struct{}

func (t accountLookupTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "account_lookup",
		Description: "Looks up account status by account name.",
		Permission:  policy.PermissionRead,
		Timeout:     5 * time.Second,
		Schema: tools.Schema{
			JSONSchema: json.RawMessage(`{"type":"object","properties":{"account":{"type":"string"}},"required":["account"],"additionalProperties":false}`),
			Validate:   requireAccount,
		},
	}
}

func (t accountLookupTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	return &tools.Result{
		ForLLM:  "account status: active",
		ForUser: "active",
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
	baseURL := os.Getenv("OPENAI_COMPAT_BASE_URL")
	model := os.Getenv("OPENAI_COMPAT_MODEL")
	apiKey := os.Getenv("OPENAI_COMPAT_API_KEY")
	if baseURL == "" || model == "" {
		fmt.Println("Skipping OpenAI-compatible example: set OPENAI_COMPAT_BASE_URL and OPENAI_COMPAT_MODEL to run it.")
		return
	}

	client, err := openaiapi.New(openaiapi.Config{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
	})
	if err != nil {
		panic(err)
	}

	registry := tools.NewRegistry()
	registry.Register(accountLookupTool{})

	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(client),
		agentcore.WithPromptBlocks([]prompt.Block{
			{Name: "identity", Mode: prompt.ModeCacheable, Content: "Use available tools before answering account questions."},
		}),
		agentcore.WithToolRegistry(registry),
	)
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(context.Background(), agentcore.RunRequest{Input: "Check the demo account."})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content)
}

var _ ports.Tool = accountLookupTool{}
