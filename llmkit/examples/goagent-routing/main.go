package main

import (
	"context"
	"fmt"
	"os"

	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/prompt"
	goagentadapter "github.com/eruca/llmkit/adapters/goagent"
	"github.com/eruca/llmkit/llmkit"
)

func main() {
	ctx := context.Background()

	home, err := llmkit.ResolveHome(".", os.Getenv, llmkit.HomeModeProduction)
	if err != nil {
		panic(err)
	}
	config, err := llmkit.LoadConfig(home)
	if err != nil {
		panic(err)
	}
	recorder, err := llmkit.NewJSONLRecorder(home)
	if err != nil {
		panic(err)
	}
	providers, err := goagentadapter.OpenAICompatibleProvidersFromConfig(*config, os.Getenv, nil)
	if err != nil {
		panic(err)
	}

	client := goagentadapter.NewClient(goagentadapter.Config{
		Candidates: config.Candidates(),
		Providers:  providers,
		ProfileProvider: func(context.Context, ports.ChatRequest) llmkit.TaskProfile {
			profile := config.DefaultTaskProfile()
			profile.Source = llmkit.ProfileSourceHost
			profile.TaskType = "example"
			profile.Complexity = llmkit.ComplexitySimple
			profile.FailureCost = llmkit.FailureCostLow
			profile.Privacy = llmkit.PrivacyLocalPreferred
			return profile
		},
		RouteMetadataProvider: func(context.Context, ports.ChatRequest) goagentadapter.RouteMetadata {
			return goagentadapter.RouteMetadata{
				RouteID: "route-example-1",
				TaskID:  "task-example-1",
				Attempt: 1,
			}
		},
		Recorder: recorder,
	})

	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(client),
		agentcore.WithPromptCompiler(prompt.NewCompiler()),
	)
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(ctx, agentcore.RunRequest{Input: "Run the llmkit routing example."})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content)
}
