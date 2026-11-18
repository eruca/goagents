// This offline example exercises the real cascade with deterministic fixture
// providers. Its test rates are fixture measurements, not model-quality claims.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/llmkit"
	adapter "github.com/eruca/goagents/llmkit/adapters/goagent"
)

type fixtureProvider struct{ rejectsEveryTenth bool }

func (p fixtureProvider) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	n, err := strconv.Atoi(req.Messages[0].Content)
	if err != nil {
		return nil, err
	}
	answer := 2 * n
	if p.rejectsEveryTenth && n%10 == 0 {
		answer++
	}
	return &ports.ChatResponse{Content: strconv.Itoa(answer)}, nil
}

func demo(ctx context.Context) (*adapter.CascadeResult, error) {
	key := llmkit.TaskKey{Type: "double-integer", Version: "v1", TestSuiteVersion: "integers-1-to-100-v1"}
	profile := llmkit.DefaultTaskProfile()
	profile.Complexity = llmkit.ComplexitySimple
	verify := func(_ context.Context, req ports.ChatRequest, response *ports.ChatResponse) (bool, error) {
		n, err := strconv.Atoi(req.Messages[0].Content)
		if err != nil {
			return false, err
		}
		got, err := strconv.Atoi(response.Content)
		return err == nil && got == 2*n, nil
	}
	models := []adapter.CascadeModel{}
	for _, m := range []struct {
		alias string
		cost  int
		weak  bool
	}{{"fixture-cheap", 0, true}, {"fixture-strong", 1, false}} {
		price := llmkit.PriceLow
		if m.cost == 0 {
			price = llmkit.PriceFree
		}
		models = append(models, adapter.CascadeModel{Candidate: llmkit.Candidate{AccountAlias: "offline", Model: llmkit.ModelCapability{Alias: m.alias, Provider: "fixture", IsLocal: true, CapabilityLevel: llmkit.CapabilitySimple, EstimatedCents: m.cost, PriceClass: price}}, Revision: "fixture-v1", Client: fixtureProvider{m.weak}})
	}
	config := adapter.CascadeConfig{Tasks: []adapter.CascadeTask{{Key: key, Profile: profile, Verify: verify}}, Models: models,
		Policy: llmkit.QualityPolicy{MinSamples: 100, MaxAttempts: 2, MaxEstimatedCents: 100}}
	evaluator, err := adapter.NewCascadeExecutor(config)
	if err != nil {
		return nil, err
	}
	cases := make([]adapter.QualityCase, 100)
	for i := range cases {
		id := strconv.Itoa(i + 1)
		cases[i] = adapter.QualityCase{ID: id, Request: ports.ChatRequest{Messages: []ports.ChatMessage{{Role: "user", Content: id}}}}
	}
	for _, model := range models {
		evidence, err := evaluator.Evaluate(ctx, key, model.Candidate.Model.Alias, cases, time.Now().Add(time.Hour))
		if err != nil {
			return nil, err
		}
		config.Evidence = append(config.Evidence, evidence)
	}
	executor, err := adapter.NewCascadeExecutor(config)
	if err != nil {
		return nil, err
	}
	return executor.Execute(ctx, key, "offline-demo", ports.ChatRequest{Messages: []ports.ChatMessage{{Role: "user", Content: "10"}}})
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := evaluationDemo(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
