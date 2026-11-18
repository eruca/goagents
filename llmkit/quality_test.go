package llmkit

import (
	"errors"
	"testing"
	"time"
)

var qualityNow = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
var qualityTask = TaskKey{Type: "extract", Version: "v1", TestSuiteVersion: "corpus-v1"}

func qualityCandidate(alias string, cost, passed int) QualityCandidate {
	model := ModelCapability{Alias: alias, Provider: "test", CapabilityLevel: CapabilityAdvanced, PriceClass: PriceLow, EstimatedCents: cost, SupportsJSON: true}
	if cost == 0 {
		model.PriceClass = PriceFree
	}
	return QualityCandidate{Candidate: Candidate{Model: model, AccountAlias: "account"}, ModelRevision: "model-v1",
		Evidence: QualityEvidence{Task: qualityTask, AccountAlias: "account", ModelAlias: alias, Provider: "test", ModelRevision: "model-v1", Passed: passed, Samples: 100, EvaluatedAt: qualityNow.Add(-time.Hour), ExpiresAt: qualityNow.Add(time.Hour)}}
}

func TestQualityPlanSelectsCheapestAbove85Percent(t *testing.T) {
	candidates := []QualityCandidate{qualityCandidate("strong", 8, 98), qualityCandidate("cheap", 1, 86), qualityCandidate("threshold", 0, 85), qualityCandidate("middle", 3, 93)}
	policy := QualityPolicy{MinSamples: 100, MaxAttempts: 3, MaxEstimatedCents: 20}
	plan, err := policy.Plan(qualityTask, DefaultTaskProfile(), candidates, qualityNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 3 || plan[0].Candidate.Model.Alias != "cheap" || plan[1].Candidate.Model.Alias != "middle" || plan[2].Candidate.Model.Alias != "strong" {
		t.Fatalf("wrong plan: %+v", plan)
	}
}

func TestQualityPlanFailsClosedForUnprovenOrIneligibleModels(t *testing.T) {
	mutations := map[string]func(*QualityCandidate){
		"85 percent":        func(c *QualityCandidate) { c.Evidence.Passed = 85 },
		"too few samples":   func(c *QualityCandidate) { c.Evidence.Samples = 99; c.Evidence.Passed = 90 },
		"task version":      func(c *QualityCandidate) { c.Evidence.Task.Version = "old" },
		"suite version":     func(c *QualityCandidate) { c.Evidence.Task.TestSuiteVersion = "old" },
		"model revision":    func(c *QualityCandidate) { c.Evidence.ModelRevision = "old" },
		"account":           func(c *QualityCandidate) { c.Evidence.AccountAlias = "other" },
		"provider":          func(c *QualityCandidate) { c.Evidence.Provider = "other" },
		"model alias":       func(c *QualityCandidate) { c.Evidence.ModelAlias = "other" },
		"expired":           func(c *QualityCandidate) { c.Evidence.ExpiresAt = qualityNow },
		"future":            func(c *QualityCandidate) { c.Evidence.EvaluatedAt = qualityNow.Add(time.Minute) },
		"impossible counts": func(c *QualityCandidate) { c.Evidence.Passed = 101 },
		"budget":            func(c *QualityCandidate) { c.Candidate.Model.EstimatedCents = 21 },
		"unknown zero cost": func(c *QualityCandidate) { c.Candidate.Model.EstimatedCents = 0 },
		"cooldown":          func(c *QualityCandidate) { c.Candidate.ProviderCooldownUntil = qualityNow.Add(time.Hour) },
		"quota":             func(c *QualityCandidate) { c.Candidate.ProviderQuotaExhausted = true },
		"busy":              func(c *QualityCandidate) { c.Candidate.ProviderMaxConcurrency = 1; c.Candidate.ProviderInFlight = 1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c := qualityCandidate("model", 1, 90)
			mutate(&c)
			_, err := (QualityPolicy{MinSamples: 100, MaxAttempts: 3, MaxEstimatedCents: 20}).Plan(qualityTask, DefaultTaskProfile(), []QualityCandidate{c}, qualityNow)
			if !errors.Is(err, ErrNoQualifiedModel) {
				t.Fatalf("unqualified model accepted: %v", err)
			}
		})
	}
	c := qualityCandidate("cloud", 1, 90)
	profile := DefaultTaskProfile()
	profile.Privacy = PrivacyLocalOnly
	if _, err := (QualityPolicy{MinSamples: 100, MaxAttempts: 3, MaxEstimatedCents: 20}).Plan(qualityTask, profile, []QualityCandidate{c}, qualityNow); !errors.Is(err, ErrNoQualifiedModel) {
		t.Fatalf("privacy: %v", err)
	}
}

func TestQualityPlanFreeOnlyBudgetAndStableTie(t *testing.T) {
	a, b := qualityCandidate("a", 0, 90), qualityCandidate("b", 0, 95)
	plan, err := (QualityPolicy{MinSamples: 100, MaxAttempts: 3, MaxEstimatedCents: 0}).Plan(qualityTask, DefaultTaskProfile(), []QualityCandidate{a, qualityCandidate("paid", 1, 99), b}, qualityNow)
	if err != nil || len(plan) != 2 || plan[0].Candidate.Model.Alias != "b" {
		t.Fatalf("free-only/tie: %+v %v", plan, err)
	}
}
