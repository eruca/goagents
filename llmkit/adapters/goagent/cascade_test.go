package goagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/llmkit"
)

var cascadeTime = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
var cascadeKey = llmkit.TaskKey{Type: "classify", Version: "v1", TestSuiteVersion: "suite-v1"}

func cascadeConfig() CascadeConfig {
	config := CascadeConfig{Policy: llmkit.QualityPolicy{MinSamples: 100, MaxAttempts: 4, MaxEstimatedCents: 20}, Now: func() time.Time { return cascadeTime }}
	profile := llmkit.DefaultTaskProfile()
	profile.Complexity = llmkit.ComplexitySimple
	profile.TaskType = cascadeKey.Type
	config.Tasks = []CascadeTask{{Key: cascadeKey, Profile: profile, Verify: func(_ context.Context, _ ports.ChatRequest, r *ports.ChatResponse) (bool, error) {
		return r.Content == "accepted", nil
	}}}
	for _, m := range []struct {
		alias        string
		cost, passed int
		answer       string
	}{
		{"threshold", 0, 85, "accepted"}, {"cheap", 1, 86, "wrong"}, {"pricey-weaker", 2, 86, "accepted"}, {"middle", 3, 93, "accepted"}, {"strong", 8, 98, "accepted"},
	} {
		price := llmkit.PriceLow
		if m.cost == 0 {
			price = llmkit.PriceFree
		}
		config.Models = append(config.Models, CascadeModel{Candidate: llmkit.Candidate{AccountAlias: "account", Model: llmkit.ModelCapability{Alias: m.alias, Provider: "offline", CapabilityLevel: llmkit.CapabilitySimple, PriceClass: price, EstimatedCents: m.cost}}, Revision: "model-v1", Client: &fakeProviderClient{response: &ports.ChatResponse{Content: m.answer}}})
		config.Evidence = append(config.Evidence, llmkit.QualityEvidence{Task: cascadeKey, AccountAlias: "account", ModelAlias: m.alias, Provider: "offline", ModelRevision: "model-v1", Passed: m.passed, Samples: 100, EvaluatedAt: cascadeTime.Add(-time.Hour), ExpiresAt: cascadeTime.Add(time.Hour)})
	}
	return config
}

func newCascade(t *testing.T, config CascadeConfig) *CascadeExecutor {
	t.Helper()
	c, err := NewCascadeExecutor(config)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCascadeStartsCheapThenEscalatesOnlyOnFailedTests(t *testing.T) {
	config := cascadeConfig()
	recorder := &fakeRecorder{}
	config.Recorder = recorder
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
	config.HealthStore = health
	result, err := newCascade(t, config).Execute(context.Background(), cascadeKey, "task-1", ports.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response.Content != "accepted" || len(result.Attempts) != 2 || result.Attempts[0].ModelAlias != "cheap" || result.Attempts[1].ModelAlias != "middle" || result.EstimatedCents != 4 {
		t.Fatalf("wrong cascade result: %+v", result)
	}
	for _, i := range []int{0, 2, 4} {
		if len(config.Models[i].Client.(*fakeProviderClient).requests) != 0 {
			t.Fatalf("unexpected model called: %s", config.Models[i].Candidate.Model.Alias)
		}
	}
	if len(recorder.outcomes) != 2 || recorder.outcomes[0].BusinessOutcome != llmkit.BusinessOutcomeFailure || recorder.outcomes[1].SuccessSignal != llmkit.SuccessSignalTestsPassed {
		t.Fatalf("quality audit missing: %+v", recorder.outcomes)
	}
	for _, entry := range health.Snapshot().Entries {
		if entry.InFlight != 0 || entry.FailureStreak != 0 {
			t.Fatalf("quality rejection corrupted transport health: %+v", entry)
		}
	}
}

func TestCascadeStopsAtFirstPassingModel(t *testing.T) {
	config := cascadeConfig()
	config.Models[1].Client = &fakeProviderClient{response: &ports.ChatResponse{Content: "accepted"}}
	result, err := newCascade(t, config).Execute(context.Background(), cascadeKey, "task-2", ports.ChatRequest{})
	if err != nil || len(result.Attempts) != 1 || result.EstimatedCents != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCascadeBudgetAndAttemptLimits(t *testing.T) {
	for _, tc := range []struct {
		name             string
		budget, attempts int
		want             error
	}{
		{"budget", 3, 4, ErrCascadeBudget}, {"attempts", 20, 1, ErrCascadeExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := cascadeConfig()
			config.Policy.MaxEstimatedCents = tc.budget
			config.Policy.MaxAttempts = tc.attempts
			result, err := newCascade(t, config).Execute(context.Background(), cascadeKey, "task", ports.ChatRequest{})
			if !errors.Is(err, tc.want) || result == nil || result.Response != nil || len(result.Attempts) != 1 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if len(config.Models[3].Client.(*fakeProviderClient).requests) != 0 {
				t.Fatal("over-limit call dispatched")
			}
		})
	}
}

func TestCascadeDoesNotEscalateProviderOrVerifierFailures(t *testing.T) {
	for _, kind := range []string{"provider", "verifier", "nil response", "tool response"} {
		t.Run(kind, func(t *testing.T) {
			config := cascadeConfig()
			want := ErrCascadeProvider
			switch kind {
			case "provider":
				config.Models[1].Client = &fakeProviderClient{err: errors.New("secret payload")}
			case "verifier":
				config.Tasks[0].Verify = func(context.Context, ports.ChatRequest, *ports.ChatResponse) (bool, error) {
					return false, errors.New("secret test payload")
				}
				want = ErrCascadeVerifier
			case "nil response":
				config.Models[1].Client = &fakeProviderClient{chat: func(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) { return nil, nil }}
			case "tool response":
				config.Models[1].Client = &fakeProviderClient{response: &ports.ChatResponse{ToolCalls: []ports.ToolCall{{Name: "write"}}}}
				want = ErrCascadeUnsupported
			}
			_, err := newCascade(t, config).Execute(context.Background(), cascadeKey, "task", ports.ChatRequest{})
			if !errors.Is(err, want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("wrong or leaking error: %v", err)
			}
			if len(config.Models[3].Client.(*fakeProviderClient).requests) != 0 {
				t.Fatal("unsafe escalation")
			}
		})
	}
}

func TestCascadeRejectsUnregisteredAndUnqualifiedTasks(t *testing.T) {
	config := cascadeConfig()
	executor := newCascade(t, config)
	wrong := cascadeKey
	wrong.Version = "unregistered"
	if _, err := executor.Execute(context.Background(), wrong, "task", ports.ChatRequest{}); err == nil {
		t.Fatal("unknown task accepted")
	}
	config.Evidence = nil
	if _, err := newCascade(t, config).Execute(context.Background(), cascadeKey, "task", ports.ChatRequest{}); !errors.Is(err, llmkit.ErrNoQualifiedModel) {
		t.Fatalf("no evidence: %v", err)
	}
	config = cascadeConfig()
	if _, err := newCascade(t, config).Execute(context.Background(), cascadeKey, "task", ports.ChatRequest{Tools: []ports.ToolSpec{{Name: "write"}}}); !errors.Is(err, ErrCascadeUnsupported) {
		t.Fatalf("tools: %v", err)
	}
}

func TestCascadeRegistrationFailsClosed(t *testing.T) {
	mutations := map[string]func(*CascadeConfig){
		"missing verifier":      func(c *CascadeConfig) { c.Tasks[0].Verify = nil },
		"duplicate task":        func(c *CascadeConfig) { c.Tasks = append(c.Tasks, c.Tasks[0]) },
		"duplicate model":       func(c *CascadeConfig) { c.Models = append(c.Models, c.Models[0]) },
		"missing model version": func(c *CascadeConfig) { c.Models[0].Revision = "" },
		"missing provider":      func(c *CascadeConfig) { c.Models[0].Client = nil },
		"no sample minimum":     func(c *CascadeConfig) { c.Policy.MinSamples = 0 },
		"no attempt limit":      func(c *CascadeConfig) { c.Policy.MaxAttempts = 0 },
		"invalid evidence":      func(c *CascadeConfig) { c.Evidence[0].Passed = 101 },
		"duplicate evidence":    func(c *CascadeConfig) { c.Evidence = append(c.Evidence, c.Evidence[0]) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c := cascadeConfig()
			mutate(&c)
			if _, err := NewCascadeExecutor(c); err == nil {
				t.Fatal("invalid registration accepted")
			}
		})
	}
}

type corpusProvider struct{}

func (corpusProvider) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	return &ports.ChatResponse{Content: strings.ToUpper(req.Messages[0].Content)}, nil
}

func TestQualityEvaluationCountsAutomaticTestResults(t *testing.T) {
	config := cascadeConfig()
	config.Models[1].Client = corpusProvider{}
	config.Tasks[0].Verify = func(_ context.Context, req ports.ChatRequest, r *ports.ChatResponse) (bool, error) {
		return r.Content == strings.ToUpper(req.Messages[0].Content) && req.Messages[0].Content != "reject", nil
	}
	cases := []QualityCase{
		{ID: "a", Request: ports.ChatRequest{Messages: []ports.ChatMessage{{Content: "alpha"}}}},
		{ID: "b", Request: ports.ChatRequest{Messages: []ports.ChatMessage{{Content: "beta"}}}},
		{ID: "c", Request: ports.ChatRequest{Messages: []ports.ChatMessage{{Content: "reject"}}}},
	}
	evidence, err := newCascade(t, config).Evaluate(context.Background(), cascadeKey, "cheap", cases, cascadeTime.Add(time.Hour))
	if err != nil || evidence.Passed != 2 || evidence.Samples != 3 || evidence.Task != cascadeKey || evidence.ModelRevision != "model-v1" {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
}

func TestCascadeCanceledBeforeDispatch(t *testing.T) {
	config := cascadeConfig()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newCascade(t, config).Execute(ctx, cascadeKey, "task", ports.ChatRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	if len(config.Models[1].Client.(*fakeProviderClient).requests) != 0 {
		t.Fatal("canceled dispatch")
	}
}

func TestQualityEvaluationChecksCorpusAndBudgetBeforeDispatch(t *testing.T) {
	for _, kind := range []string{"budget", "duplicate", "verifier"} {
		t.Run(kind, func(t *testing.T) {
			config := cascadeConfig()
			cases := []QualityCase{{ID: "a"}, {ID: "b"}}
			want := ErrCascadeVerifier
			switch kind {
			case "budget":
				config.Policy.MaxEstimatedCents = 1
				want = ErrCascadeBudget
			case "duplicate":
				cases[1].ID = "a"
				want = llmkit.ErrInvalidQualityConfig
			case "verifier":
				config.Tasks[0].Verify = func(context.Context, ports.ChatRequest, *ports.ChatResponse) (bool, error) {
					return false, errors.New("broken tests")
				}
			}
			e, err := newCascade(t, config).Evaluate(context.Background(), cascadeKey, "cheap", cases, cascadeTime.Add(time.Hour))
			if !errors.Is(err, want) || e.Samples != 0 {
				t.Fatalf("partial or over-budget evidence: %+v %v", e, err)
			}
			if kind != "verifier" && len(config.Models[1].Client.(*fakeProviderClient).requests) > 0 {
				t.Fatal("invalid evaluation dispatched")
			}
		})
	}
}

func TestCascadeAuditFailuresStopBeforeEscalation(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "route", false: "outcome"}[before], func(t *testing.T) {
			config := cascadeConfig()
			recorder := &fakeRecorder{}
			if before {
				recorder.routeErr = errors.New("private audit failure")
			} else {
				recorder.recordOutcomeFunc = func(context.Context, llmkit.TaskOutcome) error { return errors.New("private audit failure") }
			}
			config.Recorder = recorder
			_, err := newCascade(t, config).Execute(context.Background(), cascadeKey, "task", ports.ChatRequest{})
			if !errors.Is(err, ErrCascadeAudit) || strings.Contains(err.Error(), "private") {
				t.Fatalf("audit: %v", err)
			}
			if before && len(config.Models[1].Client.(*fakeProviderClient).requests) > 0 {
				t.Fatal("route failure dispatched")
			}
			if len(config.Models[3].Client.(*fakeProviderClient).requests) > 0 {
				t.Fatal("audit failure escalated")
			}
		})
	}
}

func TestCascadeCancellationWinsOverLateProviderAndVerifier(t *testing.T) {
	for _, inVerifier := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider", true: "verifier"}[inVerifier], func(t *testing.T) {
			config := cascadeConfig()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if inVerifier {
				config.Tasks[0].Verify = func(context.Context, ports.ChatRequest, *ports.ChatResponse) (bool, error) {
					cancel()
					return true, nil
				}
			} else {
				config.Models[1].Client = &fakeProviderClient{chat: func(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
					cancel()
					return &ports.ChatResponse{Content: "accepted"}, nil
				}}
			}
			health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
			config.HealthStore = health
			result, err := newCascade(t, config).Execute(ctx, cascadeKey, "task", ports.ChatRequest{})
			if !errors.Is(err, context.Canceled) || result.Response != nil {
				t.Fatalf("late success: %+v %v", result, err)
			}
			for _, entry := range health.Snapshot().Entries {
				if entry.InFlight != 0 {
					t.Fatalf("health leak: %+v", entry)
				}
			}
		})
	}
}

func TestCascadeIsolatesRequestAndRegistrationSnapshots(t *testing.T) {
	config := cascadeConfig()
	config.Tasks[0].Verify = func(_ context.Context, req ports.ChatRequest, _ *ports.ChatResponse) (bool, error) {
		return req.Messages[0].Content == "original", nil
	}
	config.Models[1].Client = &fakeProviderClient{chat: func(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
		req.Messages[0].Content = "mutated"
		return &ports.ChatResponse{Content: "answer"}, nil
	}}
	executor := newCascade(t, config)
	config.Tasks[0].Verify = nil
	config.Evidence[1].Passed = 0
	config.Models[1].Revision = "changed"
	req := ports.ChatRequest{Messages: []ports.ChatMessage{{Content: "original"}}}
	result, err := executor.Execute(context.Background(), cascadeKey, "task", req)
	if err != nil || result.Response == nil || req.Messages[0].Content != "original" {
		t.Fatalf("mutable registration/request: %+v %v", result, err)
	}
}

func TestCascadeSupportsConcurrentExecutions(t *testing.T) {
	config := cascadeConfig()
	for i := range config.Models {
		config.Models[i].Client = corpusProvider{}
	}
	config.Tasks[0].Verify = func(_ context.Context, req ports.ChatRequest, r *ports.ChatResponse) (bool, error) {
		return r.Content == strings.ToUpper(req.Messages[0].Content), nil
	}
	executor := newCascade(t, config)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := executor.Execute(context.Background(), cascadeKey, "parallel", ports.ChatRequest{Messages: []ports.ChatMessage{{Content: "alpha"}}})
			if err != nil || r.Response == nil {
				t.Errorf("parallel: %+v %v", r, err)
			}
		}()
	}
	wg.Wait()
}

func TestCascadeCancellationDuringOutcomeAuditDiscardsAcceptedResponse(t *testing.T) {
	config := cascadeConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config.Models[1].Client = &fakeProviderClient{response: &ports.ChatResponse{Content: "accepted"}}
	config.Recorder = &fakeRecorder{recordOutcomeFunc: func(context.Context, llmkit.TaskOutcome) error { cancel(); return nil }}
	result, err := newCascade(t, config).Execute(ctx, cascadeKey, "task", ports.ChatRequest{})
	if !errors.Is(err, context.Canceled) || result.Response != nil {
		t.Fatalf("audit cancellation leaked response: %+v %v", result, err)
	}
}
