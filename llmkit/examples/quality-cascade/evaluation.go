package main

import (
	"context"
	"strconv"
	"time"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/llmkit"
	adapter "github.com/eruca/goagents/llmkit/adapters/goagent"
)

type evaluationFixture struct{ kind string }

func (p evaluationFixture) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	n, err := strconv.Atoi(req.Messages[0].Content)
	if err != nil {
		return nil, err
	}
	wrong := false
	switch p.kind {
	case "cheap":
		wrong = n%10 == 0
	case "higher-overall":
		wrong = n%50 == 0
	case "rescuer":
		wrong = n%25 == 1
	}
	answer := 2 * n
	if wrong {
		answer++
	}
	return &ports.ChatResponse{Content: strconv.Itoa(answer)}, nil
}

func evaluationDemo(ctx context.Context) (*adapter.CascadeEvaluationReport, error) {
	key := llmkit.TaskKey{Type: "double-integer", Version: "v1", TestSuiteVersion: "arithmetic-frozen-v2"}
	profile := llmkit.DefaultTaskProfile()
	profile.Complexity = llmkit.ComplexitySimple
	verify := func(_ context.Context, req ports.ChatRequest, r *ports.ChatResponse) (bool, error) {
		n, err := strconv.Atoi(req.Messages[0].Content)
		if err != nil {
			return false, err
		}
		got, err := strconv.Atoi(r.Content)
		return err == nil && got == 2*n, nil
	}
	config := adapter.CascadeConfig{Tasks: []adapter.CascadeTask{{Key: key, Profile: profile, Verify: verify}},
		Policy: llmkit.QualityPolicy{MinSamples: 100, MaxAttempts: 3, MaxEstimatedCents: 3}}
	for cost, kind := range []string{"cheap", "higher-overall", "rescuer"} {
		price := llmkit.PriceLow
		if cost == 0 {
			price = llmkit.PriceFree
		}
		config.Models = append(config.Models, adapter.CascadeModel{Candidate: llmkit.Candidate{AccountAlias: "offline", Model: llmkit.ModelCapability{Alias: "fixture-" + kind, Provider: "fixture", IsLocal: true, CapabilityLevel: llmkit.CapabilitySimple, PriceClass: price, EstimatedCents: cost}}, Revision: "fixture-v2", Client: evaluationFixture{kind}})
	}
	request := func(n int) ports.ChatRequest {
		return ports.ChatRequest{Messages: []ports.ChatMessage{{Role: "user", Content: strconv.Itoa(n)}}}
	}
	set := adapter.CascadeEvaluationSet{VerifierCases: []adapter.VerifierCase{
		{ID: "correct", Request: request(7), Response: &ports.ChatResponse{Content: "14"}, ExpectedPassed: true},
		{ID: "wrong-number", Request: request(7), Response: &ports.ChatResponse{Content: "15"}},
		{ID: "missing", Request: request(7), Response: &ports.ChatResponse{}},
		{ID: "malformed", Request: request(7), Response: &ports.ChatResponse{Content: "unsupported claim"}},
	}}
	for n := 1; n <= 100; n++ {
		set.Qualification = append(set.Qualification, adapter.QualityCase{ID: "train-" + strconv.Itoa(n), Request: request(n)})
	}
	for n := 101; n <= 120; n++ {
		set.Validation = append(set.Validation, adapter.QualityCase{ID: "holdout-" + strconv.Itoa(n), Request: request(n)})
	}
	executor, err := adapter.NewCascadeExecutor(config)
	if err != nil {
		return nil, err
	}
	return executor.EvaluateCascade(ctx, key, set, adapter.CascadeEvaluationOptions{ExpiresAt: time.Now().Add(time.Hour), MaxEstimatedCents: 400, RescuePolicy: llmkit.RescuePolicy{MinSamples: 5, MinSuccessRate: 0.9}})
}
