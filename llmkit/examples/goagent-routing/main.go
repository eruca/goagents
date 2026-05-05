package main

import (
	"context"
	"errors"
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
	routeMetadata := goagentadapter.RouteMetadata{
		RouteID: "route-example-1",
		TaskID:  "task-example-1",
		Attempt: 1,
	}
	providers, err := goagentadapter.OpenAICompatibleProvidersFromConfig(*config, os.Getenv, nil)
	if err != nil {
		panic(err)
	}
	exampleProfile := config.DefaultTaskProfile()
	exampleProfile.Source = llmkit.ProfileSourceHost
	exampleProfile.TaskType = "example"
	exampleProfile.Complexity = llmkit.ComplexitySimple
	exampleProfile.FailureCost = llmkit.FailureCostLow
	exampleProfile.Privacy = llmkit.PrivacyLocalPreferred

	var modelStats *llmkit.ModelStats
	loadedStats, err := llmkit.LoadModelStats(home)
	if err == nil {
		modelStats = loadedStats
	} else if !errors.Is(err, os.ErrNotExist) {
		panic(err)
	}

	client := goagentadapter.NewClient(goagentadapter.Config{
		Candidates: config.Candidates(),
		Providers:  providers,
		ProfileProvider: func(context.Context, ports.ChatRequest) llmkit.TaskProfile {
			return exampleProfile
		},
		RouteMetadataProvider: func(context.Context, ports.ChatRequest) goagentadapter.RouteMetadata {
			return routeMetadata
		},
		Recorder:       recorder,
		RecordOutcomes: true,
		ModelStats:     modelStats,
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
