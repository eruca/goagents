package llmkit

import (
	"strings"
	"testing"
	"time"
)

func TestRoutePolicySelectsLocalFreeModelForSimpleTaskWithoutLatencyRequirement(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.Complexity = ComplexitySimple
	profile.Latency = LatencyNone
	profile.FailureCost = FailureCostLow
	profile.Privacy = PrivacyLocalPreferred

	decision, err := RoutePolicy{}.Select(profile, []Candidate{
		candidate("cloud-balanced", CapabilityBalanced, PriceLow, LatencyNormalClass, false),
		candidate("local-free", CapabilitySimple, PriceFree, LatencyNormalClass, true),
	})
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}

	if decision.SelectedAlias != "local-free" {
		t.Fatalf("SelectedAlias = %q, want %q; decision=%+v", decision.SelectedAlias, "local-free", decision)
	}
	if len(decision.Candidates) != 2 {
		t.Fatalf("decision should retain candidate explanations, got %d", len(decision.Candidates))
	}
	if decision.ScoreBreakdown["price"] <= 0 || decision.ScoreBreakdown["local"] <= 0 {
		t.Fatalf("selected decision should explain price and local preference: %+v", decision.ScoreBreakdown)
	}
	if decision.Reason == "" {
		t.Fatalf("decision should include a reason")
	}
}

func TestRoutePolicySelectsAdvancedModelForHardTaskWithHighFailureCost(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.Complexity = ComplexityHard
	profile.FailureCost = FailureCostHigh

	decision, err := RoutePolicy{}.Select(profile, []Candidate{
		candidate("balanced-cheap", CapabilityBalanced, PriceLow, LatencyFastClass, false),
		candidate("advanced", CapabilityAdvanced, PriceHigh, LatencyNormalClass, false),
	})
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}

	if decision.SelectedAlias != "advanced" {
		t.Fatalf("SelectedAlias = %q, want %q; decision=%+v", decision.SelectedAlias, "advanced", decision)
	}
	if decision.ScoreBreakdown["capability"] <= 0 {
		t.Fatalf("selected decision should explain capability preference: %+v", decision.ScoreBreakdown)
	}
}

func TestRoutePolicyExcludesModelsWithoutJSONSupport(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.NeedsJSON = true

	decision, err := RoutePolicy{}.Select(profile, []Candidate{
		candidate("plain", CapabilityBalanced, PriceFree, LatencyFastClass, true),
		withJSON(candidate("json", CapabilityBalanced, PriceLow, LatencyNormalClass, false)),
	})
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}

	if decision.SelectedAlias != "json" {
		t.Fatalf("SelectedAlias = %q, want %q; decision=%+v", decision.SelectedAlias, "json", decision)
	}
	if !candidateExcluded(decision.Candidates, "plain") {
		t.Fatalf("model without JSON support should be excluded: %+v", decision.Candidates)
	}
}

func TestRoutePolicyExcludesModelsWithoutToolSupport(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.NeedsTools = true

	decision, err := RoutePolicy{}.Select(profile, []Candidate{
		candidate("plain", CapabilityBalanced, PriceFree, LatencyFastClass, true),
		withTools(candidate("tools", CapabilityBalanced, PriceLow, LatencyNormalClass, false)),
	})
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}

	if decision.SelectedAlias != "tools" {
		t.Fatalf("SelectedAlias = %q, want %q; decision=%+v", decision.SelectedAlias, "tools", decision)
	}
	if !candidateExcluded(decision.Candidates, "plain") {
		t.Fatalf("model without tool support should be excluded: %+v", decision.Candidates)
	}
}

func TestRoutePolicySkipsRateLimitedOrConcurrencyFullAccounts(t *testing.T) {
	profile := DefaultTaskProfile()

	decision, err := RoutePolicy{}.Select(profile, []Candidate{
		withRateLimitedAccount(candidate("rate-limited", CapabilityBalanced, PriceFree, LatencyFastClass, true)),
		withFullAccount(candidate("concurrency-full", CapabilityBalanced, PriceFree, LatencyFastClass, true)),
		candidate("available", CapabilityBalanced, PriceLow, LatencyNormalClass, false),
	})
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}

	if decision.SelectedAlias != "available" {
		t.Fatalf("SelectedAlias = %q, want %q; decision=%+v", decision.SelectedAlias, "available", decision)
	}
	if !candidateExcluded(decision.Candidates, "rate-limited") {
		t.Fatalf("rate-limited account should be excluded: %+v", decision.Candidates)
	}
	if !candidateExcluded(decision.Candidates, "concurrency-full") {
		t.Fatalf("concurrency-full candidate should be excluded: %+v", decision.Candidates)
	}
}

func TestRoutePolicyAppliesTaskMaxEstimatedCentsAsHardConstraint(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.MaxEstimatedCents = 5

	expensive := candidate("expensive", CapabilityBalanced, PriceHigh, LatencyNormalClass, false)
	expensive.Model.EstimatedCents = 10
	affordable := candidate("affordable", CapabilityBalanced, PriceLow, LatencyNormalClass, false)
	affordable.Model.EstimatedCents = 4

	decision, err := RoutePolicy{}.Select(profile, []Candidate{expensive, affordable})
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if decision.SelectedAlias != "affordable" {
		t.Fatalf("SelectedAlias = %q, want affordable; decision=%+v", decision.SelectedAlias, decision)
	}
	if !candidateExcludedFor(decision.Candidates, "expensive", "estimated cost exceeds task budget") {
		t.Fatalf("expensive candidate should be excluded by budget: %+v", decision.Candidates)
	}
}

func TestRoutePolicyUsesStableAccountTieBreakForSameModelAlias(t *testing.T) {
	profile := DefaultTaskProfile()

	accountA := candidate("same-model", CapabilityBalanced, PriceLow, LatencyNormalClass, false)
	accountA.AccountAlias = "account-a"
	accountB := candidate("same-model", CapabilityBalanced, PriceLow, LatencyNormalClass, false)
	accountB.AccountAlias = "account-b"

	for name, candidates := range map[string][]Candidate{
		"forward": {accountA, accountB},
		"reverse": {accountB, accountA},
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := RoutePolicy{}.Select(profile, candidates)
			if err != nil {
				t.Fatalf("Select returned error: %v", err)
			}
			if decision.Selected.AccountAlias != "account-a" {
				t.Fatalf("Selected.AccountAlias = %q, want %q; decision=%+v", decision.Selected.AccountAlias, "account-a", decision)
			}
		})
	}
}

func TestRoutePolicyReturnsClearErrorWhenNoCandidateAvailable(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.NeedsJSON = true

	_, err := RoutePolicy{}.Select(profile, []Candidate{
		candidate("plain", CapabilityBalanced, PriceFree, LatencyFastClass, true),
	})
	if err == nil {
		t.Fatalf("Select should return error when no candidate is available")
	}
	if !strings.Contains(err.Error(), "no available route candidates") {
		t.Fatalf("error = %q, want clear no-candidate message", err.Error())
	}
}

func candidate(alias string, level CapabilityLevel, price PriceClass, latency LatencyClass, local bool) Candidate {
	return Candidate{
		Model: ModelCapability{
			Alias:              alias,
			Provider:           "test",
			IsLocal:            local,
			CapabilityLevel:    level,
			ContextWindowClass: ContextMedium,
			PriceClass:         price,
			LatencyClass:       latency,
			MaxConcurrency:     1,
		},
		AccountAlias: alias + "-account",
	}
}

func withJSON(c Candidate) Candidate {
	c.Model.SupportsJSON = true
	return c
}

func withTools(c Candidate) Candidate {
	c.Model.SupportsTools = true
	return c
}

func withRateLimitedAccount(c Candidate) Candidate {
	c.AccountRateLimited = true
	return c
}

func withFullAccount(c Candidate) Candidate {
	c.AccountMaxConcurrency = 1
	c.AccountCurrentConcurrency = 1
	return c
}

func candidateExcluded(candidates []CandidateScore, alias string) bool {
	for _, candidate := range candidates {
		if candidate.Alias == alias {
			return !candidate.Available
		}
	}
	return false
}

func TestReliabilityScoreIsBoundedSmoothedFailureRate(t *testing.T) {
	mature := ModelCapability{RecentOutcomeCount: 10000, RecentFailureCount: 100, RecentFailureRate: 0.01}
	fresh := ModelCapability{}
	flaky := ModelCapability{RecentOutcomeCount: 3, RecentFailureCount: 3, RecentFailureRate: 1}
	broken := ModelCapability{RecentOutcomeCount: 1000, RecentFailureCount: 1000, RecentFailureRate: 1}

	if got := reliabilityScore(mature); got != -1 {
		t.Fatalf("mature 1%% failure score = %d, want -1", got)
	}
	if got := reliabilityScore(fresh); got != -5 {
		t.Fatalf("no-history score = %d, want prior -5", got)
	}
	if reliabilityScore(mature) <= reliabilityScore(fresh) {
		t.Fatal("a model with a long clean history should beat one with no history")
	}
	if got := reliabilityScore(flaky); got >= reliabilityScore(fresh) || got < -100 {
		t.Fatalf("3/3 failures score = %d, want worse than prior but bounded", got)
	}
	if got := reliabilityScore(broken); got < -100 {
		t.Fatalf("always-failing score = %d, want at least -100", got)
	}
}

func TestLatencyScoreUsesObservedLatencyOverConfiguredClass(t *testing.T) {
	profile := DefaultTaskProfile()
	profile.Latency = LatencyUrgent
	configuredFast := ModelCapability{LatencyClass: LatencyFastClass}
	observedSlow := ModelCapability{LatencyClass: LatencyFastClass, RecentLatencyMillis: 15000}
	observedFast := ModelCapability{LatencyClass: LatencySlowClass, RecentLatencyMillis: 900}

	if latencyScore(profile, configuredFast) != 20 {
		t.Fatalf("configured fast score = %d, want 20", latencyScore(profile, configuredFast))
	}
	if latencyScore(profile, observedSlow) != 0 {
		t.Fatalf("observed slow score = %d, want 0", latencyScore(profile, observedSlow))
	}
	if latencyScore(profile, observedFast) != 20 {
		t.Fatalf("observed fast score = %d, want 20", latencyScore(profile, observedFast))
	}
}

func TestRoutePolicyAllowsOneProbeAfterCooldown(t *testing.T) {
	now := time.Date(2026, 5, 6, 3, 30, 0, 0, time.UTC)
	probing := candidate("local-free", CapabilityBalanced, PriceFree, LatencyFastClass, true)
	probing.ProviderAvailability = ProviderUnavailable
	probing.ProviderCooldownUntil = now.Add(-time.Second)
	probing.ProviderInFlight = 1

	_, err := RoutePolicy{Now: func() time.Time { return now }}.Select(DefaultTaskProfile(), []Candidate{probing})
	if err == nil {
		t.Fatal("Select returned nil error, want candidate held back while its recovery probe is in flight")
	}
}
