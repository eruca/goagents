package goagent

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/eruca/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/llmkit/llmkit"
)

func TestClientImplementsGoagentLLMClient(t *testing.T) {
	var _ ports.LLMClient = (*Client)(nil)
}

func TestClientRoutesSimpleProfileToSelectedProviderAndRecordsTrace(t *testing.T) {
	ctx := context.Background()
	local := &fakeProviderClient{response: &ports.ChatResponse{Content: "local"}}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud"}}
	recorder := &fakeRecorder{}

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-simple",
			TaskID:  "task-simple",
			Attempt: 2,
		}),
		Recorder: recorder,
	})

	req := ports.ChatRequest{Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}}}
	resp, err := client.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "local" {
		t.Fatalf("Chat() response content = %q, want local", resp.Content)
	}
	if len(local.requests) != 1 {
		t.Fatalf("local provider calls = %d, want 1", len(local.requests))
	}
	if len(cloud.requests) != 0 {
		t.Fatalf("cloud provider calls = %d, want 0", len(cloud.requests))
	}
	if got := local.requests[0].Messages[0].Content; got != "hello" {
		t.Fatalf("forwarded request content = %q, want hello", got)
	}

	trace := recorder.singleRouteTrace(t)
	if trace.RouteID != "route-simple" {
		t.Fatalf("trace RouteID = %q, want route-simple", trace.RouteID)
	}
	if trace.TaskID != "task-simple" {
		t.Fatalf("trace TaskID = %q, want task-simple", trace.TaskID)
	}
	if trace.Attempt != 2 {
		t.Fatalf("trace Attempt = %d, want 2", trace.Attempt)
	}
	if trace.ModelAlias != "local-small" {
		t.Fatalf("trace ModelAlias = %q, want local-small", trace.ModelAlias)
	}
	if trace.AccountAlias != "local-account" {
		t.Fatalf("trace AccountAlias = %q, want local-account", trace.AccountAlias)
	}
	if trace.Provider != "local" {
		t.Fatalf("trace Provider = %q, want local", trace.Provider)
	}
	if !trace.Selected {
		t.Fatal("trace Selected = false, want true")
	}
	if trace.Reason == "" {
		t.Fatal("trace Reason is empty")
	}
	if trace.TaskProfile == nil {
		t.Fatal("trace TaskProfile is nil")
	}
	if trace.TaskProfile.TaskType != "simple" || trace.TaskProfile.Complexity != llmkit.ComplexitySimple || trace.TaskProfile.Privacy != llmkit.PrivacyCloudAllowed {
		t.Fatalf("trace TaskProfile = %+v, want effective simple profile", trace.TaskProfile)
	}
	if len(trace.CandidateModelAliases) != 2 {
		t.Fatalf("trace candidate aliases len = %d, want 2", len(trace.CandidateModelAliases))
	}
	if len(trace.Candidates) != 2 {
		t.Fatalf("trace candidates len = %d, want 2", len(trace.Candidates))
	}
	localScore := routeCandidateScore(t, trace.Candidates, "local-small")
	if !localScore.Available || localScore.Score == 0 || localScore.ScoreBreakdown["price"] == 0 {
		t.Fatalf("local candidate score missing explanation: %+v", localScore)
	}
	cloudScore := routeCandidateScore(t, trace.Candidates, "cloud-advanced")
	if !cloudScore.Available || cloudScore.Score == 0 || cloudScore.Reason == "" {
		t.Fatalf("cloud candidate score missing explanation: %+v", cloudScore)
	}
}

func TestClientAppliesModelStatsAfterProfileSelection(t *testing.T) {
	local := &fakeProviderClient{response: &ports.ChatResponse{Content: "local"}}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud"}}
	recorder := &fakeRecorder{}
	stats := &llmkit.ModelStats{
		Models: map[string]llmkit.ModelStatsEntry{
			"simple|local-account|local-small|local": {
				TaskType:         "simple",
				AccountAlias:     "local-account",
				ModelAlias:       "local-small",
				Provider:         "local",
				OutcomeCount:     10,
				Failures:         9,
				FailureRate:      0.9,
				AvgLatencyMillis: 200,
			},
		},
	}

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-stats",
			TaskID:  "task-stats",
			Attempt: 1,
		}),
		Recorder:   recorder,
		ModelStats: stats,
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "cloud" {
		t.Fatalf("Chat() response content = %q, want cloud", resp.Content)
	}
	if len(local.requests) != 0 {
		t.Fatalf("local provider calls = %d, want 0", len(local.requests))
	}
	if len(cloud.requests) != 1 {
		t.Fatalf("cloud provider calls = %d, want 1", len(cloud.requests))
	}
	trace := recorder.singleRouteTrace(t)
	if trace.ModelAlias != "cloud-advanced" {
		t.Fatalf("trace ModelAlias = %q, want cloud-advanced", trace.ModelAlias)
	}
}

func TestClientRefreshesModelStatsForEachChat(t *testing.T) {
	local := &fakeProviderClient{response: &ports.ChatResponse{Content: "local"}}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud"}}
	calls := 0
	statsProvider := func(context.Context) (*llmkit.ModelStats, error) {
		calls++
		if calls == 1 {
			return &llmkit.ModelStats{}, nil
		}
		return &llmkit.ModelStats{Models: map[string]llmkit.ModelStatsEntry{
			"simple|local-account|local-small|local": {
				TaskType:     "simple",
				AccountAlias: "local-account",
				ModelAlias:   "local-small",
				Provider:     "local",
				OutcomeCount: 10,
				Failures:     10,
				FailureRate:  1,
			},
		}}, nil
	}

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider:    fixedProfile(simpleProfile()),
		ModelStatsProvider: statsProvider,
	})

	first, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("first Chat() error = %v", err)
	}
	if first.Content != "local" {
		t.Fatalf("first response = %q, want local", first.Content)
	}
	second, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("second Chat() error = %v", err)
	}
	if second.Content != "cloud" {
		t.Fatalf("second response = %q, want cloud after refreshed stats", second.Content)
	}
	if calls != 2 {
		t.Fatalf("stats provider calls = %d, want 2", calls)
	}
	if len(local.requests) != 1 {
		t.Fatalf("local provider calls = %d, want 1", len(local.requests))
	}
}

func TestClientRespectsFallbackMaxAttempts(t *testing.T) {
	local := &fakeProviderClient{err: errors.New("local failed")}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud"}}
	recorder := &fakeRecorder{}

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-fallback",
			TaskID:  "task-fallback",
			Attempt: 1,
		}),
		Recorder:       recorder,
		RecordOutcomes: true,
		FallbackPolicy: FallbackPolicy{MaxAttempts: 1},
	})

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err == nil {
		t.Fatal("Chat() error = nil, want provider failure without fallback")
	}
	if len(local.requests) != 1 || len(cloud.requests) != 0 {
		t.Fatalf("provider calls local=%d cloud=%d, want local only", len(local.requests), len(cloud.requests))
	}
	if len(recorder.routes) != 1 || recorder.routes[0].FallbackMaxAttempts != 1 {
		t.Fatalf("recorded routes = %+v, want one route with fallback max attempts", recorder.routes)
	}
	if len(recorder.outcomes) != 1 || recorder.outcomes[0].ErrorCode != "provider_error" {
		t.Fatalf("recorded outcomes = %+v, want provider error outcome", recorder.outcomes)
	}
}

func TestClientAppliesProviderHealthAndRecordsOutcomes(t *testing.T) {
	local := &fakeProviderClient{response: &ports.ChatResponse{Content: "local"}}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud"}}
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{
		FailureCooldownThreshold: 1,
	})
	health.Set(llmkit.ProviderHealthEntry{
		AccountAlias:   "local-account",
		ModelAlias:     "local-small",
		Provider:       "local",
		QuotaExhausted: true,
	})

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		HealthStore:     health,
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "cloud" {
		t.Fatalf("Chat() response content = %q, want cloud", resp.Content)
	}
	if len(local.requests) != 0 {
		t.Fatalf("local provider calls = %d, want 0", len(local.requests))
	}
	if len(cloud.requests) != 1 {
		t.Fatalf("cloud provider calls = %d, want 1", len(cloud.requests))
	}

	entry := health.Snapshot().Entries[llmkit.ProviderHealthKey("cloud-account", "cloud-advanced", "openai")]
	if entry.InFlight != 0 || entry.FailureStreak != 0 || entry.Availability != llmkit.ProviderAvailable {
		t.Fatalf("cloud health entry = %+v, want successful available outcome with no in-flight calls", entry)
	}
}

func TestClientRoutesHardProfileToSelectedProviderAndRecordsTrace(t *testing.T) {
	local := &fakeProviderClient{response: &ports.ChatResponse{Content: "local"}}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud"}}
	recorder := &fakeRecorder{}

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(hardProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-hard",
			TaskID:  "task-hard",
			Attempt: 1,
		}),
		Recorder: recorder,
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "cloud" {
		t.Fatalf("Chat() response content = %q, want cloud", resp.Content)
	}
	if len(local.requests) != 0 {
		t.Fatalf("local provider calls = %d, want 0", len(local.requests))
	}
	if len(cloud.requests) != 1 {
		t.Fatalf("cloud provider calls = %d, want 1", len(cloud.requests))
	}

	trace := recorder.singleRouteTrace(t)
	if trace.RouteID != "route-hard" {
		t.Fatalf("trace RouteID = %q, want route-hard", trace.RouteID)
	}
	if trace.TaskID != "task-hard" {
		t.Fatalf("trace TaskID = %q, want task-hard", trace.TaskID)
	}
	if trace.Attempt != 1 {
		t.Fatalf("trace Attempt = %d, want 1", trace.Attempt)
	}
	if trace.ModelAlias != "cloud-advanced" {
		t.Fatalf("trace ModelAlias = %q, want cloud-advanced", trace.ModelAlias)
	}
	if trace.AccountAlias != "cloud-account" {
		t.Fatalf("trace AccountAlias = %q, want cloud-account", trace.AccountAlias)
	}
	if trace.Provider != "openai" {
		t.Fatalf("trace Provider = %q, want openai", trace.Provider)
	}
	if trace.Score == 0 {
		t.Fatal("trace Score = 0, want policy score")
	}
	if len(trace.ScoreBreakdown) == 0 {
		t.Fatal("trace ScoreBreakdown is empty")
	}
}

func TestClientSkipsCandidateWithoutProvider(t *testing.T) {
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud"}}
	recorder := &fakeRecorder{}

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-provider-filter",
			TaskID:  "task-provider-filter",
			Attempt: 1,
		}),
		Recorder: recorder,
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "cloud" {
		t.Fatalf("Chat() response content = %q, want cloud", resp.Content)
	}
	if len(cloud.requests) != 1 {
		t.Fatalf("cloud provider calls = %d, want 1", len(cloud.requests))
	}
	trace := recorder.singleRouteTrace(t)
	if trace.ModelAlias != "cloud-advanced" {
		t.Fatalf("trace ModelAlias = %q, want cloud-advanced", trace.ModelAlias)
	}
}

func TestClientFallsBackToNextCandidateWhenSelectedProviderFails(t *testing.T) {
	local := &fakeProviderClient{err: errors.New("local unavailable")}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud fallback"}}
	recorder := &fakeRecorder{}

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-fallback",
			TaskID:  "task-fallback",
			Attempt: 1,
		}),
		Recorder: recorder,
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "cloud fallback" {
		t.Fatalf("Chat() response content = %q, want cloud fallback", resp.Content)
	}
	if len(local.requests) != 1 {
		t.Fatalf("local provider calls = %d, want 1", len(local.requests))
	}
	if len(cloud.requests) != 1 {
		t.Fatalf("cloud provider calls = %d, want 1", len(cloud.requests))
	}
	if len(recorder.routes) != 2 {
		t.Fatalf("recorded route traces = %d, want 2", len(recorder.routes))
	}
	if recorder.routes[0].ModelAlias != "local-small" || recorder.routes[0].Attempt != 1 {
		t.Fatalf("first route = %+v, want local-small attempt 1", recorder.routes[0])
	}
	if recorder.routes[1].ModelAlias != "cloud-advanced" || recorder.routes[1].Attempt != 2 {
		t.Fatalf("second route = %+v, want cloud-advanced attempt 2", recorder.routes[1])
	}
}

func TestClientRecordsOutcomesForFallbackAttemptsWhenEnabled(t *testing.T) {
	local := &fakeProviderClient{err: errors.New("local unavailable")}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{
		Content: "cloud fallback",
		Usage: ports.Usage{
			InputTokens:  7,
			OutputTokens: 11,
		},
	}}
	recorder := &fakeRecorder{}

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-outcome-fallback",
			TaskID:  "task-outcome-fallback",
			Attempt: 1,
		}),
		Recorder:       recorder,
		RecordOutcomes: true,
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "cloud fallback" {
		t.Fatalf("Chat() response content = %q, want cloud fallback", resp.Content)
	}
	if len(recorder.outcomes) != 2 {
		t.Fatalf("recorded outcomes = %d, want 2", len(recorder.outcomes))
	}
	first := recorder.outcomes[0]
	if first.Success || first.ModelAlias != "local-small" || first.Attempt != 1 || first.ErrorCode == "" {
		t.Fatalf("first outcome = %+v, want failed local attempt", first)
	}
	second := recorder.outcomes[1]
	if !second.Success || second.ModelAlias != "cloud-advanced" || second.Attempt != 2 {
		t.Fatalf("second outcome = %+v, want successful cloud attempt", second)
	}
	if second.InputTokens != 7 || second.OutputTokens != 11 {
		t.Fatalf("second outcome usage = %d/%d, want 7/11", second.InputTokens, second.OutputTokens)
	}
}

func TestClientRecordsClassifiedProviderErrors(t *testing.T) {
	local := &fakeProviderClient{err: errors.New("deadline exceeded")}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud fallback"}}
	recorder := &fakeRecorder{}

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-classified-fallback",
			TaskID:  "task-classified-fallback",
			Attempt: 1,
		}),
		Recorder:       recorder,
		RecordOutcomes: true,
		ErrorClassifier: func(error) llmkit.ErrorClass {
			return llmkit.ErrorClassTimeout
		},
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "cloud fallback" {
		t.Fatalf("Chat() response content = %q, want cloud fallback", resp.Content)
	}
	if len(recorder.outcomes) != 2 {
		t.Fatalf("recorded outcomes = %d, want 2", len(recorder.outcomes))
	}
	first := recorder.outcomes[0]
	if first.Success || first.ErrorCode != "provider_error" || first.ErrorClass != llmkit.ErrorClassTimeout {
		t.Fatalf("first outcome = %+v, want classified timeout provider failure", first)
	}
	second := recorder.outcomes[1]
	if !second.Success || second.ErrorClass != "" {
		t.Fatalf("second outcome = %+v, want successful unclassified outcome", second)
	}
}

func TestClientUsesDefaultErrorClassifier(t *testing.T) {
	local := &fakeProviderClient{err: &openaiapi.ResponseError{StatusCode: http.StatusUnauthorized}}
	recorder := &fakeRecorder{}

	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small": local,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-default-classifier",
			TaskID:  "task-default-classifier",
			Attempt: 1,
		}),
		Recorder:       recorder,
		RecordOutcomes: true,
	})

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err == nil {
		t.Fatal("Chat() error = nil, want provider failure")
	}
	if len(recorder.outcomes) != 1 {
		t.Fatalf("recorded outcomes = %d, want 1", len(recorder.outcomes))
	}
	outcome := recorder.outcomes[0]
	if outcome.ErrorCode != "provider_error" || outcome.ErrorClass != llmkit.ErrorClassAuth {
		t.Fatalf("outcome = %+v, want provider_error/auth_error", outcome)
	}
}

func TestClientReturnsErrorWhenNoProviderBackedCandidateCanHandleTask(t *testing.T) {
	client := NewClient(Config{
		Candidates:      testCandidates(),
		Providers:       map[string]ProviderClient{"local-small": &fakeProviderClient{}},
		ProfileProvider: fixedProfile(hardProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-missing",
			TaskID:  "task-missing",
			Attempt: 1,
		}),
	})

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err == nil {
		t.Fatal("Chat() error = nil, want no available candidates error")
	}
}

func TestClientRequiresRouteMetadataWhenRecording(t *testing.T) {
	recorder := &fakeRecorder{}
	client := NewClient(Config{
		Candidates:      testCandidates(),
		Providers:       map[string]ProviderClient{"local-small": &fakeProviderClient{}},
		ProfileProvider: fixedProfile(simpleProfile()),
		Recorder:        recorder,
	})

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err == nil {
		t.Fatal("Chat() error = nil, want metadata validation error")
	}
	if len(recorder.routes) != 0 {
		t.Fatalf("recorded route traces = %d, want 0", len(recorder.routes))
	}
}

func simpleProfile() llmkit.TaskProfile {
	profile := llmkit.DefaultTaskProfile()
	profile.Source = llmkit.ProfileSourceHost
	profile.TaskType = "simple"
	profile.Complexity = llmkit.ComplexitySimple
	profile.FailureCost = llmkit.FailureCostLow
	profile.Latency = llmkit.LatencyNormal
	return profile
}

func hardProfile() llmkit.TaskProfile {
	profile := llmkit.DefaultTaskProfile()
	profile.Source = llmkit.ProfileSourceHost
	profile.TaskType = "hard"
	profile.Complexity = llmkit.ComplexityHard
	profile.FailureCost = llmkit.FailureCostHigh
	profile.NeedsReasoning = true
	return profile
}

func testCandidates() []llmkit.Candidate {
	return []llmkit.Candidate{
		{
			Model: llmkit.ModelCapability{
				Alias:              "local-small",
				Provider:           "local",
				IsLocal:            true,
				CapabilityLevel:    llmkit.CapabilitySimple,
				ContextWindowClass: llmkit.ContextMedium,
				PriceClass:         llmkit.PriceFree,
				LatencyClass:       llmkit.LatencyFastClass,
			},
			AccountAlias: "local-account",
		},
		{
			Model: llmkit.ModelCapability{
				Alias:              "cloud-advanced",
				Provider:           "openai",
				CapabilityLevel:    llmkit.CapabilityAdvanced,
				ContextWindowClass: llmkit.ContextLong,
				PriceClass:         llmkit.PriceHigh,
				LatencyClass:       llmkit.LatencyNormalClass,
			},
			AccountAlias: "cloud-account",
		},
	}
}

func fixedProfile(profile llmkit.TaskProfile) ProfileProvider {
	return func(context.Context, ports.ChatRequest) llmkit.TaskProfile {
		return profile
	}
}

func fixedRouteMetadata(metadata RouteMetadata) RouteMetadataProvider {
	return func(context.Context, ports.ChatRequest) RouteMetadata {
		return metadata
	}
}

type fakeProviderClient struct {
	response *ports.ChatResponse
	err      error
	requests []ports.ChatRequest
}

func (f *fakeProviderClient) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.response == nil {
		return &ports.ChatResponse{}, nil
	}
	return f.response, nil
}

type fakeRecorder struct {
	routeErr error
	routes   []llmkit.RouteTrace
	outcomes []llmkit.TaskOutcome
}

func (f *fakeRecorder) RecordRoute(_ context.Context, trace llmkit.RouteTrace) error {
	if f.routeErr != nil {
		return f.routeErr
	}
	f.routes = append(f.routes, trace)
	return nil
}

func (f *fakeRecorder) RecordOutcome(_ context.Context, outcome llmkit.TaskOutcome) error {
	f.outcomes = append(f.outcomes, outcome)
	return nil
}

func (f *fakeRecorder) singleRouteTrace(t *testing.T) llmkit.RouteTrace {
	t.Helper()
	if len(f.routes) != 1 {
		t.Fatalf("recorded route traces = %d, want 1", len(f.routes))
	}
	return f.routes[0]
}

func routeCandidateScore(t *testing.T, scores []llmkit.CandidateScore, alias string) llmkit.CandidateScore {
	t.Helper()
	for _, score := range scores {
		if score.Alias == alias {
			return score
		}
	}
	t.Fatalf("candidate score %q not found: %+v", alias, scores)
	return llmkit.CandidateScore{}
}
