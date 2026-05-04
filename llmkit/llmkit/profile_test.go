package llmkit

import "testing"

func TestTaskProfileDefault(t *testing.T) {
	profile := DefaultTaskProfile()

	if profile.Source != ProfileSourceDefault {
		t.Fatalf("Source = %q, want %q", profile.Source, ProfileSourceDefault)
	}
	if profile.Complexity != ComplexityMedium {
		t.Fatalf("Complexity = %q, want %q", profile.Complexity, ComplexityMedium)
	}
	if profile.Latency != LatencyNormal {
		t.Fatalf("Latency = %q, want %q", profile.Latency, LatencyNormal)
	}
	if profile.FailureCost != FailureCostMedium {
		t.Fatalf("FailureCost = %q, want %q", profile.FailureCost, FailureCostMedium)
	}
	if profile.Privacy != PrivacyCloudAllowed {
		t.Fatalf("Privacy = %q, want %q", profile.Privacy, PrivacyCloudAllowed)
	}
	if profile.NeedsJSON || profile.NeedsTools || profile.NeedsLongContext || profile.NeedsReasoning {
		t.Fatalf("default profile should not opt into JSON/tools/long-context/reasoning requirements: %+v", profile)
	}
}

func TestModelCapabilityMatchesRequiredFeatures(t *testing.T) {
	jsonToolProfile := DefaultTaskProfile()
	jsonToolProfile.NeedsJSON = true
	jsonToolProfile.NeedsTools = true
	jsonToolProfile.NeedsLongContext = true

	capability := ModelCapability{
		Alias:              "cloud-advanced",
		CapabilityLevel:    CapabilityAdvanced,
		SupportsJSON:       true,
		SupportsTools:      true,
		ContextWindowClass: ContextLong,
		PriceClass:         PriceHigh,
		LatencyClass:       LatencyNormalClass,
		MaxConcurrency:     8,
	}

	if !capability.Matches(jsonToolProfile) {
		t.Fatalf("advanced JSON/tool/long-context model should match profile")
	}

	capability.SupportsJSON = false
	if capability.Matches(jsonToolProfile) {
		t.Fatalf("model without JSON support should not match JSON profile")
	}

	capability.SupportsJSON = true
	capability.SupportsTools = false
	if capability.Matches(jsonToolProfile) {
		t.Fatalf("model without tool support should not match tool profile")
	}

	capability.SupportsTools = true
	capability.ContextWindowClass = ContextMedium
	if capability.Matches(jsonToolProfile) {
		t.Fatalf("medium-context model should not match long-context profile")
	}
}

func TestModelCapabilityMatchesComplexityAndConcurrency(t *testing.T) {
	hardProfile := DefaultTaskProfile()
	hardProfile.Complexity = ComplexityHard

	capability := ModelCapability{
		Alias:              "local-small",
		CapabilityLevel:    CapabilitySimple,
		ContextWindowClass: ContextMedium,
		PriceClass:         PriceFree,
		LatencyClass:       LatencyFastClass,
		MaxConcurrency:     1,
	}

	if capability.Matches(hardProfile) {
		t.Fatalf("simple model should not match hard profile")
	}

	capability.CapabilityLevel = CapabilityAdvanced
	if !capability.Matches(hardProfile) {
		t.Fatalf("advanced model should match hard profile")
	}

	capability.CurrentConcurrency = 1
	if capability.Matches(hardProfile) {
		t.Fatalf("model at max concurrency should not match")
	}
}

func TestModelCapabilityLocalOnlyFilteringSemantics(t *testing.T) {
	localOnly := DefaultTaskProfile()
	localOnly.Privacy = PrivacyLocalOnly

	localPreferred := DefaultTaskProfile()
	localPreferred.Privacy = PrivacyLocalPreferred

	cloudModel := ModelCapability{
		Alias:              "cloud-balanced",
		CapabilityLevel:    CapabilityBalanced,
		ContextWindowClass: ContextMedium,
		PriceClass:         PriceLow,
		LatencyClass:       LatencyNormalClass,
		MaxConcurrency:     4,
		IsLocal:            false,
	}
	localModel := cloudModel
	localModel.Alias = "local-balanced"
	localModel.PriceClass = PriceFree
	localModel.IsLocal = true

	if cloudModel.Matches(localOnly) {
		t.Fatalf("cloud model should not match local-only profile")
	}
	if !localModel.Matches(localOnly) {
		t.Fatalf("local model should match local-only profile")
	}
	if !cloudModel.Matches(localPreferred) {
		t.Fatalf("local-preferred profile should not filter out cloud models")
	}
}
