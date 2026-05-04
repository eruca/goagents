package goagent

import (
	"context"
	"errors"
	"testing"

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
	if len(trace.CandidateModelAliases) != 2 {
		t.Fatalf("trace candidate aliases len = %d, want 2", len(trace.CandidateModelAliases))
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

func TestClientReturnsErrorWhenSelectedProviderIsMissing(t *testing.T) {
	client := NewClient(Config{
		Candidates:      testCandidates(),
		Providers:       map[string]ProviderClient{"local-small": &fakeProviderClient{}},
		ProfileProvider: fixedProfile(hardProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-missing",
		}),
	})

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err == nil {
		t.Fatal("Chat() error = nil, want missing provider error")
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
}

func (f *fakeRecorder) RecordRoute(_ context.Context, trace llmkit.RouteTrace) error {
	if f.routeErr != nil {
		return f.routeErr
	}
	f.routes = append(f.routes, trace)
	return nil
}

func (f *fakeRecorder) RecordOutcome(context.Context, llmkit.TaskOutcome) error {
	return errors.New("outcome recording is not used by goagent adapter")
}

func (f *fakeRecorder) singleRouteTrace(t *testing.T) llmkit.RouteTrace {
	t.Helper()
	if len(f.routes) != 1 {
		t.Fatalf("recorded route traces = %d, want 1", len(f.routes))
	}
	return f.routes[0]
}
