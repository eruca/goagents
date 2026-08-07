package goagent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/llmkit/llmkit"
)

func TestClientImplementsGoagentLLMClient(t *testing.T) {
	var _ ports.LLMClient = (*Client)(nil)
}

func TestClientForwardsMaxOutputTokensToSelectedProvider(t *testing.T) {
	local := &optionsProviderClient{response: &ports.ChatResponse{Content: "local"}}
	client := NewClient(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": local},
		ProfileProvider: fixedProfile(simpleProfile()),
	})
	resp, err := client.ChatWithMaxOutputTokens(context.Background(), ports.ChatRequest{}, 4096)
	if err != nil {
		t.Fatalf("ChatWithMaxOutputTokens() error = %v", err)
	}
	if resp.Content != "local" || len(local.maxOutputTokens) != 1 || local.maxOutputTokens[0] != 4096 {
		t.Fatalf("response/max output tokens = %q/%+v, want local/[4096]", resp.Content, local.maxOutputTokens)
	}
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

func TestClientClosesProviderHealthWhenRequestContextEnds(t *testing.T) {
	tests := []struct {
		name       string
		requestErr error
	}{
		{name: "canceled", requestErr: context.Canceled},
		{name: "deadline exceeded", requestErr: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestCtx := newEndingContext()
			local := &fakeProviderClient{
				chat: func(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
					requestCtx.end(tt.requestErr)
					return nil, tt.requestErr
				},
			}
			health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
			client := NewClient(Config{
				Candidates:      testCandidates()[:1],
				Providers:       map[string]ProviderClient{"local-small": local},
				ProfileProvider: fixedProfile(simpleProfile()),
				HealthStore:     health,
			})

			_, err := client.Chat(requestCtx, ports.ChatRequest{})
			if !errors.Is(err, tt.requestErr) {
				t.Fatalf("Chat() error = %v, want %v", err, tt.requestErr)
			}
			if len(local.requests) != 1 {
				t.Fatalf("local provider calls = %d, want 1", len(local.requests))
			}

			key := llmkit.ProviderHealthKey("local-account", "local-small", "local")
			entry := health.Snapshot().Entries[key]
			if entry.InFlight != 0 {
				t.Fatalf("health in-flight = %d, want 0 after request context ended", entry.InFlight)
			}
		})
	}
}

func TestClientBoundsPostCallCleanupAndDoesNotFallbackOnFailure(t *testing.T) {
	requestCtx := newEndingContext()
	local := &fakeProviderClient{
		chat: func(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
			requestCtx.end(context.Canceled)
			return nil, context.Canceled
		},
	}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud fallback"}}
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
	postCallErr := errors.New("outcome recorder unavailable")
	recorder := &fakeRecorder{
		recordOutcomeFunc: func(ctx context.Context, _ llmkit.TaskOutcome) error {
			if err := ctx.Err(); err != nil {
				t.Fatalf("outcome cleanup context error = %v, want active context", err)
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("outcome cleanup context has no deadline")
			}
			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > 2*time.Second {
				t.Fatalf("outcome cleanup deadline remaining = %v, want within (0s, 2s]", remaining)
			}
			return postCallErr
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
			RouteID: "route-canceled-cleanup",
			TaskID:  "task-canceled-cleanup",
			Attempt: 1,
		}),
		Recorder:       recorder,
		RecordOutcomes: true,
		HealthStore:    health,
	})

	_, err := client.Chat(requestCtx, ports.ChatRequest{})
	if !errors.Is(err, postCallErr) {
		t.Fatalf("Chat() error = %v, want post-call error %v", err, postCallErr)
	}
	if len(local.requests) != 1 || len(cloud.requests) != 0 {
		t.Fatalf("provider calls local=%d cloud=%d, want local only", len(local.requests), len(cloud.requests))
	}
	if len(recorder.outcomes) != 1 {
		t.Fatalf("recorded outcomes = %d, want 1", len(recorder.outcomes))
	}
	key := llmkit.ProviderHealthKey("local-account", "local-small", "local")
	entry := health.Snapshot().Entries[key]
	if entry.InFlight != 0 {
		t.Fatalf("health in-flight = %d, want 0 after post-call failure", entry.InFlight)
	}
}

func TestClientDiscardsLateResponseAfterRequestCancellation(t *testing.T) {
	requestCtx := newEndingContext()
	local := &fakeProviderClient{
		chat: func(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
			requestCtx.end(context.Canceled)
			return &ports.ChatResponse{Content: "late response"}, nil
		},
	}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud fallback"}}
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
	recorder := &fakeRecorder{}
	client := NewClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-late-canceled",
			TaskID:  "task-late-canceled",
			Attempt: 1,
		}),
		Recorder:       recorder,
		RecordOutcomes: true,
		HealthStore:    health,
	})

	resp, err := client.Chat(requestCtx, ports.ChatRequest{})
	if resp != nil {
		t.Fatalf("Chat() response = %+v, want nil after cancellation", resp)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat() error = %v, want context.Canceled", err)
	}
	if len(local.requests) != 1 || len(cloud.requests) != 0 {
		t.Fatalf("provider calls local=%d cloud=%d, want local only", len(local.requests), len(cloud.requests))
	}
	if len(recorder.outcomes) != 1 {
		t.Fatalf("recorded outcomes = %d, want 1", len(recorder.outcomes))
	}
	outcome := recorder.outcomes[0]
	if outcome.Success || outcome.ErrorClass != llmkit.ErrorClassCanceled {
		t.Fatalf("outcome = %+v, want canceled failure", outcome)
	}
	key := llmkit.ProviderHealthKey("local-account", "local-small", "local")
	if entry := health.Snapshot().Entries[key]; entry.InFlight != 0 {
		t.Fatalf("health in-flight = %d, want 0", entry.InFlight)
	}
}

func TestRuntimeClientDoesNotFallbackForProviderErrorsByDefault(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "timeout", err: context.DeadlineExceeded},
		{name: "auth", err: &openaiapi.ResponseError{StatusCode: http.StatusUnauthorized}},
		{name: "rate limited", err: &openaiapi.ResponseError{StatusCode: http.StatusTooManyRequests}},
		{name: "server failure", err: &openaiapi.ResponseError{StatusCode: http.StatusServiceUnavailable}},
		{name: "unknown", err: errors.New("provider failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local := &fakeProviderClient{err: tt.err}
			cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud fallback"}}
			client := NewRuntimeClient(Config{
				Candidates: testCandidates(),
				Providers: map[string]ProviderClient{
					"local-small":    local,
					"cloud-advanced": cloud,
				},
				ProfileProvider: fixedProfile(simpleProfile()),
			})

			resp, err := client.Chat(context.Background(), ports.ChatRequest{})
			if resp != nil || err == nil {
				t.Fatalf("Chat() response/error = %+v/%v, want provider failure", resp, err)
			}
			if len(local.requests) != 1 || len(cloud.requests) != 0 {
				t.Fatalf("provider calls local=%d cloud=%d, want local only", len(local.requests), len(cloud.requests))
			}
		})
	}
}

func TestClientReturnsTypedRouteError(t *testing.T) {
	client := NewClient(Config{})

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	assertRuntimeError(t, err, ErrorStageRoute, llmkit.ErrorClassPolicyBlocked, false)
}

func TestClientReturnsTypedPreCallError(t *testing.T) {
	preCallErr := errors.New("route recorder unavailable")
	client := NewClient(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": &fakeProviderClient{}},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-pre-call-error",
			TaskID:  "task-pre-call-error",
			Attempt: 1,
		}),
		Recorder: &fakeRecorder{routeErr: preCallErr},
	})

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	assertRuntimeError(t, err, ErrorStagePreCall, llmkit.ErrorClassUnknown, false)
	if !errors.Is(err, preCallErr) {
		t.Fatalf("Chat() error = %v, want wrapped pre-call error", err)
	}
}

func TestClientReturnsTypedProviderErrorWithoutLeakingCause(t *testing.T) {
	providerErr := errors.New("provider-secret-response-body")
	client := NewClient(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": &fakeProviderClient{err: providerErr}},
		ProfileProvider: fixedProfile(simpleProfile()),
	})

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	assertRuntimeError(t, err, ErrorStageProvider, llmkit.ErrorClassUnknown, true)
	if !errors.Is(err, providerErr) {
		t.Fatalf("Chat() error = %v, want wrapped provider error", err)
	}
	if strings.Contains(err.Error(), "provider-secret-response-body") {
		t.Fatalf("Chat() public error leaked provider cause: %v", err)
	}
}

func TestClientReturnsTypedPostCallError(t *testing.T) {
	postCallErr := errors.New("outcome recorder unavailable")
	client := NewClient(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": &fakeProviderClient{response: &ports.ChatResponse{Content: "done"}}},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-post-call-error",
			TaskID:  "task-post-call-error",
			Attempt: 1,
		}),
		Recorder: &fakeRecorder{
			recordOutcomeFunc: func(context.Context, llmkit.TaskOutcome) error {
				return postCallErr
			},
		},
		RecordOutcomes: true,
	})

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	assertRuntimeError(t, err, ErrorStagePostCall, llmkit.ErrorClassPostCall, true)
	if !errors.Is(err, postCallErr) {
		t.Fatalf("Chat() error = %v, want wrapped post-call error", err)
	}
}

func TestRuntimeClientFallsBackOnlyForExplicitPreDispatchClass(t *testing.T) {
	local := &fakeProviderClient{err: preDispatchProviderError{err: errors.New("dial rejected before dispatch")}}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud fallback"}}
	client := NewRuntimeClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		ErrorClassifier: func(error) llmkit.ErrorClass {
			return llmkit.ErrorClassTransient
		},
	}, WithRetryableErrorClasses(llmkit.ErrorClassTransient))

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "cloud fallback" {
		t.Fatalf("Chat() response content = %q, want cloud fallback", resp.Content)
	}
	if len(local.requests) != 1 || len(cloud.requests) != 1 {
		t.Fatalf("provider calls local=%d cloud=%d, want one each", len(local.requests), len(cloud.requests))
	}
}

func TestRuntimeClientDoesNotFallbackForDispatchedErrorEvenWhenClassAllowed(t *testing.T) {
	local := &fakeProviderClient{err: errors.New("provider failed after dispatch")}
	cloud := &fakeProviderClient{response: &ports.ChatResponse{Content: "cloud fallback"}}
	client := NewRuntimeClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		ErrorClassifier: func(error) llmkit.ErrorClass {
			return llmkit.ErrorClassTransient
		},
	}, WithRetryableErrorClasses(llmkit.ErrorClassTransient))

	_, err := client.Chat(context.Background(), ports.ChatRequest{})
	assertRuntimeError(t, err, ErrorStageProvider, llmkit.ErrorClassTransient, true)
	if len(local.requests) != 1 || len(cloud.requests) != 0 {
		t.Fatalf("provider calls local=%d cloud=%d, want local only", len(local.requests), len(cloud.requests))
	}
}

func TestClientRunsPreDispatchHookWithoutRecorderBeforeHealthAndProvider(t *testing.T) {
	const sensitiveMessage = "provider-sensitive-prompt-sentinel"
	events := []string{}
	providerDigest := sha256.Sum256([]byte("provider request"))
	runDigest := sha256.Sum256([]byte("run request"))
	local := &digestProviderClient{
		fakeProviderClient: fakeProviderClient{response: &ports.ChatResponse{Content: "local"}},
		digest:             providerDigest,
		events:             &events,
	}
	health := &eventHealthStore{
		HealthStore: llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{}),
		events:      &events,
	}
	var got PreDispatchRequest
	client := NewClientWithPreDispatch(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": local},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-pre-dispatch",
			TaskID:  "task-pre-dispatch",
			Attempt: 2,
		}),
		HealthStore: health,
	}, PreDispatchConfig{
		MetadataProvider: fixedPreDispatchMetadata(PreDispatchMetadata{
			ProviderCallIndex: 3,
			RunRequestSHA256:  runDigest,
		}),
		Hook: func(_ context.Context, req PreDispatchRequest) error {
			events = append(events, "hook")
			got = req
			return nil
		},
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "user", Content: sensitiveMessage}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "local" {
		t.Fatalf("Chat() response content = %q, want local", resp.Content)
	}
	if got.Route.RouteID != "route-pre-dispatch" || got.Route.TaskID != "task-pre-dispatch" {
		t.Fatalf("pre-dispatch route = %+v, want configured safe identifiers", got.Route)
	}
	if got.Attempt != 2 || got.ProviderCallIndex != 3 || got.ProviderClass != "local" {
		t.Fatalf("pre-dispatch metadata = %+v, want attempt=2 call_index=3 provider_class=local", got)
	}
	if got.RunRequestSHA256 != runDigest || got.ProviderRequestSHA256 != providerDigest {
		t.Fatalf("pre-dispatch digests = %x/%x, want %x/%x", got.RunRequestSHA256, got.ProviderRequestSHA256, runDigest, providerDigest)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal pre-dispatch request: %v", err)
	}
	if strings.Contains(string(encoded), sensitiveMessage) {
		t.Fatalf("pre-dispatch request leaked ChatRequest content: %s", encoded)
	}
	if gotEvents := strings.Join(events, ","); gotEvents != "digest,hook,health_begin,provider,health_outcome" {
		t.Fatalf("call order = %q, want digest,hook,health_begin,provider,health_outcome", gotEvents)
	}
}

func TestClientPreDispatchHookFailureStopsBeforeHealthProviderAndFallback(t *testing.T) {
	events := []string{}
	providerDigest := sha256.Sum256([]byte("provider request"))
	runDigest := sha256.Sum256([]byte("run request"))
	local := &digestProviderClient{
		fakeProviderClient: fakeProviderClient{response: &ports.ChatResponse{Content: "local"}},
		digest:             providerDigest,
		events:             &events,
	}
	cloud := &digestProviderClient{
		fakeProviderClient: fakeProviderClient{response: &ports.ChatResponse{Content: "cloud"}},
		digest:             providerDigest,
		events:             &events,
	}
	health := &eventHealthStore{
		HealthStore: llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{}),
		events:      &events,
	}
	hookErr := errors.New("provider claim unavailable")
	client := NewClientWithPreDispatch(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-hook-failure",
			TaskID:  "task-hook-failure",
			Attempt: 1,
		}),
		HealthStore: health,
		ErrorClassifier: func(error) llmkit.ErrorClass {
			return llmkit.ErrorClassTransient
		},
	}, PreDispatchConfig{
		MetadataProvider: fixedPreDispatchMetadata(PreDispatchMetadata{
			ProviderCallIndex: 1,
			RunRequestSHA256:  runDigest,
		}),
		Hook: func(context.Context, PreDispatchRequest) error {
			events = append(events, "hook")
			return hookErr
		},
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if resp != nil {
		t.Fatalf("Chat() response = %+v, want nil", resp)
	}
	assertRuntimeError(t, err, ErrorStagePreCall, llmkit.ErrorClassTransient, false)
	if !errors.Is(err, hookErr) {
		t.Fatalf("Chat() error = %v, want hook error", err)
	}
	if len(local.requests) != 0 || len(cloud.requests) != 0 {
		t.Fatalf("provider calls local=%d cloud=%d, want zero", len(local.requests), len(cloud.requests))
	}
	if len(health.Snapshot().Entries) != 0 {
		t.Fatalf("health entries = %+v, want none", health.Snapshot().Entries)
	}
	if gotEvents := strings.Join(events, ","); gotEvents != "digest,hook" {
		t.Fatalf("call order = %q, want digest,hook", gotEvents)
	}
}

func TestClientPreDispatchHookRequiresProviderRequestDigester(t *testing.T) {
	local := &fakeProviderClient{response: &ports.ChatResponse{Content: "local"}}
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
	metadataCalls := 0
	hookCalls := 0
	client := NewClientWithPreDispatch(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": local},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-missing-digester",
			TaskID:  "task-missing-digester",
			Attempt: 1,
		}),
		HealthStore: health,
	}, PreDispatchConfig{
		MetadataProvider: func(context.Context, ports.ChatRequest) PreDispatchMetadata {
			metadataCalls++
			return PreDispatchMetadata{
				ProviderCallIndex: 1,
				RunRequestSHA256:  sha256.Sum256([]byte("run request")),
			}
		},
		Hook: func(context.Context, PreDispatchRequest) error {
			hookCalls++
			return nil
		},
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if resp != nil {
		t.Fatalf("Chat() response = %+v, want nil", resp)
	}
	assertRuntimeError(t, err, ErrorStageConfig, llmkit.ErrorClassConfiguration, false)
	if metadataCalls != 0 || hookCalls != 0 || len(local.requests) != 0 {
		t.Fatalf("metadata/hook/provider calls = %d/%d/%d, want zero", metadataCalls, hookCalls, len(local.requests))
	}
	if len(health.Snapshot().Entries) != 0 {
		t.Fatalf("health entries = %+v, want none", health.Snapshot().Entries)
	}
}

func TestClientPreDispatchHookRejectsInvalidHostMetadata(t *testing.T) {
	validDigest := sha256.Sum256([]byte("run request"))
	tests := []struct {
		name     string
		metadata PreDispatchMetadata
	}{
		{
			name: "missing provider call index",
			metadata: PreDispatchMetadata{
				RunRequestSHA256: validDigest,
			},
		},
		{
			name: "missing run request digest",
			metadata: PreDispatchMetadata{
				ProviderCallIndex: 1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local := &digestProviderClient{
				fakeProviderClient: fakeProviderClient{response: &ports.ChatResponse{Content: "local"}},
				digest:             sha256.Sum256([]byte("provider request")),
			}
			health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
			hookCalls := 0
			client := NewClientWithPreDispatch(Config{
				Candidates:      testCandidates()[:1],
				Providers:       map[string]ProviderClient{"local-small": local},
				ProfileProvider: fixedProfile(simpleProfile()),
				RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
					RouteID: "route-invalid-host-metadata",
					TaskID:  "task-invalid-host-metadata",
					Attempt: 1,
				}),
				HealthStore: health,
			}, PreDispatchConfig{
				MetadataProvider: fixedPreDispatchMetadata(tt.metadata),
				Hook: func(context.Context, PreDispatchRequest) error {
					hookCalls++
					return nil
				},
			})

			resp, err := client.Chat(context.Background(), ports.ChatRequest{})
			if resp != nil {
				t.Fatalf("Chat() response = %+v, want nil", resp)
			}
			assertRuntimeError(t, err, ErrorStagePreCall, llmkit.ErrorClassConfiguration, false)
			if hookCalls != 0 || len(local.requests) != 0 {
				t.Fatalf("hook/provider calls = %d/%d, want zero", hookCalls, len(local.requests))
			}
			if len(health.Snapshot().Entries) != 0 {
				t.Fatalf("health entries = %+v, want none", health.Snapshot().Entries)
			}
		})
	}
}

func TestClientPreDispatchHookRejectsMissingProviderRequestDigest(t *testing.T) {
	local := &digestProviderClient{
		fakeProviderClient: fakeProviderClient{response: &ports.ChatResponse{Content: "local"}},
	}
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
	hookCalls := 0
	client := NewClientWithPreDispatch(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": local},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-missing-provider-digest",
			TaskID:  "task-missing-provider-digest",
			Attempt: 1,
		}),
		HealthStore: health,
	}, PreDispatchConfig{
		MetadataProvider: fixedPreDispatchMetadata(PreDispatchMetadata{
			ProviderCallIndex: 1,
			RunRequestSHA256:  sha256.Sum256([]byte("run request")),
		}),
		Hook: func(context.Context, PreDispatchRequest) error {
			hookCalls++
			return nil
		},
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if resp != nil {
		t.Fatalf("Chat() response = %+v, want nil", resp)
	}
	assertRuntimeError(t, err, ErrorStagePreCall, llmkit.ErrorClassConfiguration, false)
	if hookCalls != 0 || len(local.requests) != 0 {
		t.Fatalf("hook/provider calls = %d/%d, want zero", hookCalls, len(local.requests))
	}
	if len(health.Snapshot().Entries) != 0 {
		t.Fatalf("health entries = %+v, want none", health.Snapshot().Entries)
	}
}

func TestClientPreDispatchHookUsesHostCallIndexesAcrossFallback(t *testing.T) {
	providerDigest := sha256.Sum256([]byte("provider request"))
	local := &digestProviderClient{
		fakeProviderClient: fakeProviderClient{err: preDispatchProviderError{err: errors.New("local unavailable")}},
		digest:             providerDigest,
	}
	cloud := &digestProviderClient{
		fakeProviderClient: fakeProviderClient{response: &ports.ChatResponse{Content: "cloud"}},
		digest:             providerDigest,
	}
	var callIndexes []int
	var attempts []int
	routeMetadataCalls := 0
	dispatchMetadataCalls := 0
	client := NewRuntimeClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: func(context.Context, ports.ChatRequest) RouteMetadata {
			routeMetadataCalls++
			return RouteMetadata{
				RouteID: "route-hook-fallback",
				TaskID:  "task-hook-fallback",
				Attempt: 2,
			}
		},
		ErrorClassifier: func(error) llmkit.ErrorClass {
			return llmkit.ErrorClassTransient
		},
	}, WithPreDispatch(PreDispatchConfig{
		MetadataProvider: func(context.Context, ports.ChatRequest) PreDispatchMetadata {
			dispatchMetadataCalls++
			return PreDispatchMetadata{
				ProviderCallIndex: []int{7, 11}[dispatchMetadataCalls-1],
				RunRequestSHA256:  sha256.Sum256([]byte("run request")),
			}
		},
		Hook: func(_ context.Context, req PreDispatchRequest) error {
			callIndexes = append(callIndexes, req.ProviderCallIndex)
			attempts = append(attempts, req.Attempt)
			return nil
		},
	}), WithRetryableErrorClasses(llmkit.ErrorClassTransient))

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "cloud" {
		t.Fatalf("Chat() response content = %q, want cloud", resp.Content)
	}
	if got := fmt.Sprint(callIndexes); got != "[7 11]" {
		t.Fatalf("provider call indexes = %s, want host-owned [7 11]", got)
	}
	if got := fmt.Sprint(attempts); got != "[2 3]" {
		t.Fatalf("attempts = %s, want [2 3]", got)
	}
	if routeMetadataCalls != 1 || dispatchMetadataCalls != 2 {
		t.Fatalf("route/dispatch metadata provider calls = %d/%d, want 1/2", routeMetadataCalls, dispatchMetadataCalls)
	}
}

func TestClientPreDispatchHookRejectsNonMonotonicHostCallIndex(t *testing.T) {
	providerDigest := sha256.Sum256([]byte("provider request"))
	local := &digestProviderClient{
		fakeProviderClient: fakeProviderClient{err: preDispatchProviderError{err: errors.New("local unavailable")}},
		digest:             providerDigest,
	}
	cloud := &digestProviderClient{
		fakeProviderClient: fakeProviderClient{response: &ports.ChatResponse{Content: "cloud"}},
		digest:             providerDigest,
	}
	metadataCalls := 0
	hookCalls := 0
	client := NewRuntimeClient(Config{
		Candidates: testCandidates(),
		Providers: map[string]ProviderClient{
			"local-small":    local,
			"cloud-advanced": cloud,
		},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-non-monotonic-index",
			TaskID:  "task-non-monotonic-index",
			Attempt: 1,
		}),
		ErrorClassifier: func(error) llmkit.ErrorClass {
			return llmkit.ErrorClassTransient
		},
	}, WithPreDispatch(PreDispatchConfig{
		MetadataProvider: func(context.Context, ports.ChatRequest) PreDispatchMetadata {
			metadataCalls++
			return PreDispatchMetadata{
				ProviderCallIndex: 7,
				RunRequestSHA256:  sha256.Sum256([]byte("run request")),
			}
		},
		Hook: func(context.Context, PreDispatchRequest) error {
			hookCalls++
			return nil
		},
	}), WithRetryableErrorClasses(llmkit.ErrorClassTransient))

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if resp != nil {
		t.Fatalf("Chat() response = %+v, want nil", resp)
	}
	assertRuntimeError(t, err, ErrorStagePreCall, llmkit.ErrorClassConfiguration, false)
	if metadataCalls != 2 || hookCalls != 1 {
		t.Fatalf("metadata/hook calls = %d/%d, want 2/1", metadataCalls, hookCalls)
	}
	if len(local.requests) != 1 || len(cloud.requests) != 0 {
		t.Fatalf("local/cloud provider calls = %d/%d, want 1/0", len(local.requests), len(cloud.requests))
	}
}

func TestClientPreDispatchDigestFailureStopsBeforeHookHealthAndProvider(t *testing.T) {
	digestErr := classifiedProviderError{class: string(llmkit.ErrorClassRequestTooLarge)}
	local := &digestProviderClient{
		fakeProviderClient: fakeProviderClient{response: &ports.ChatResponse{Content: "local"}},
		digestErr:          digestErr,
	}
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
	metadataCalls := 0
	hookCalls := 0
	client := NewClientWithPreDispatch(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": local},
		ProfileProvider: fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{
			RouteID: "route-digest-failure",
			TaskID:  "task-digest-failure",
			Attempt: 1,
		}),
		HealthStore: health,
	}, PreDispatchConfig{
		MetadataProvider: func(context.Context, ports.ChatRequest) PreDispatchMetadata {
			metadataCalls++
			return PreDispatchMetadata{
				ProviderCallIndex: 1,
				RunRequestSHA256:  sha256.Sum256([]byte("run request")),
			}
		},
		Hook: func(context.Context, PreDispatchRequest) error {
			hookCalls++
			return nil
		},
	})

	resp, err := client.Chat(context.Background(), ports.ChatRequest{})
	if resp != nil {
		t.Fatalf("Chat() response = %+v, want nil", resp)
	}
	assertRuntimeError(t, err, ErrorStagePreCall, llmkit.ErrorClassRequestTooLarge, false)
	if !errors.Is(err, digestErr) {
		t.Fatalf("Chat() error = %v, want digest error", err)
	}
	if metadataCalls != 0 || hookCalls != 0 || len(local.requests) != 0 {
		t.Fatalf("metadata/hook/provider calls = %d/%d/%d, want zero", metadataCalls, hookCalls, len(local.requests))
	}
	if len(health.Snapshot().Entries) != 0 {
		t.Fatalf("health entries = %+v, want none", health.Snapshot().Entries)
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
		Recorder:       recorder,
		FallbackPolicy: FallbackPolicy{MaxAttempts: 2},
		ErrorClassifier: func(error) llmkit.ErrorClass {
			return llmkit.ErrorClassTransient
		},
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
	local := &fakeProviderClient{err: preDispatchProviderError{err: errors.New("local unavailable")}}
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
		FallbackPolicy: FallbackPolicy{MaxAttempts: 2},
		ErrorClassifier: func(error) llmkit.ErrorClass {
			return llmkit.ErrorClassTransient
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
	local := &fakeProviderClient{err: preDispatchProviderError{err: errors.New("deadline exceeded")}}
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
		FallbackPolicy: FallbackPolicy{MaxAttempts: 2},
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

func fixedPreDispatchMetadata(metadata PreDispatchMetadata) PreDispatchMetadataProvider {
	return func(context.Context, ports.ChatRequest) PreDispatchMetadata {
		return metadata
	}
}

type fakeProviderClient struct {
	response *ports.ChatResponse
	err      error
	requests []ports.ChatRequest
	chat     func(context.Context, ports.ChatRequest) (*ports.ChatResponse, error)
}

type optionsProviderClient struct {
	response        *ports.ChatResponse
	maxOutputTokens []int
}

func (c *optionsProviderClient) Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
	return c.response, nil
}

func (c *optionsProviderClient) ChatWithMaxOutputTokens(_ context.Context, _ ports.ChatRequest, maxOutputTokens int) (*ports.ChatResponse, error) {
	c.maxOutputTokens = append(c.maxOutputTokens, maxOutputTokens)
	return c.response, nil
}

type digestProviderClient struct {
	fakeProviderClient
	digest    [sha256.Size]byte
	digestErr error
	events    *[]string
}

func (f *digestProviderClient) ProviderRequestSHA256(ports.ChatRequest) ([sha256.Size]byte, error) {
	if f.events != nil {
		*f.events = append(*f.events, "digest")
	}
	return f.digest, f.digestErr
}

func (f *digestProviderClient) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	if f.events != nil {
		*f.events = append(*f.events, "provider")
	}
	return f.fakeProviderClient.Chat(ctx, req)
}

type eventHealthStore struct {
	llmkit.HealthStore
	events *[]string
}

func (s *eventHealthStore) Begin(ctx context.Context, candidate llmkit.Candidate) error {
	*s.events = append(*s.events, "health_begin")
	return s.HealthStore.Begin(ctx, candidate)
}

func (s *eventHealthStore) RecordOutcome(ctx context.Context, outcome llmkit.TaskOutcome) error {
	*s.events = append(*s.events, "health_outcome")
	return s.HealthStore.RecordOutcome(ctx, outcome)
}

func (f *fakeProviderClient) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	f.requests = append(f.requests, req)
	if f.chat != nil {
		return f.chat(ctx, req)
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.response == nil {
		return &ports.ChatResponse{}, nil
	}
	return f.response, nil
}

type fakeRecorder struct {
	routeErr          error
	routes            []llmkit.RouteTrace
	outcomes          []llmkit.TaskOutcome
	recordOutcomeFunc func(context.Context, llmkit.TaskOutcome) error
}

func (f *fakeRecorder) RecordRoute(_ context.Context, trace llmkit.RouteTrace) error {
	if f.routeErr != nil {
		return f.routeErr
	}
	f.routes = append(f.routes, trace)
	return nil
}

func (f *fakeRecorder) RecordOutcome(ctx context.Context, outcome llmkit.TaskOutcome) error {
	f.outcomes = append(f.outcomes, outcome)
	if f.recordOutcomeFunc != nil {
		return f.recordOutcomeFunc(ctx, outcome)
	}
	return nil
}

type endingContext struct {
	context.Context
	mu   sync.RWMutex
	done chan struct{}
	err  error
	once sync.Once
}

func newEndingContext() *endingContext {
	return &endingContext{
		Context: context.Background(),
		done:    make(chan struct{}),
	}
}

func (c *endingContext) Done() <-chan struct{} {
	return c.done
}

func (c *endingContext) Err() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.err
}

func (c *endingContext) end(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	})
}

type preDispatchProviderError struct {
	err error
}

func (e preDispatchProviderError) Error() string {
	return e.err.Error()
}

func (e preDispatchProviderError) Unwrap() error {
	return e.err
}

func (preDispatchProviderError) ProviderDispatched() bool {
	return false
}

func assertRuntimeError(t *testing.T, err error, stage ErrorStage, class llmkit.ErrorClass, dispatched bool) {
	t.Helper()
	var runtimeErr *RuntimeError
	if !errors.As(err, &runtimeErr) {
		t.Fatalf("error type = %T, want *RuntimeError: %v", err, err)
	}
	if runtimeErr.Stage != stage || runtimeErr.Class != class || runtimeErr.ProviderDispatched != dispatched {
		t.Fatalf("RuntimeError = %+v, want stage=%q class=%q dispatched=%t", runtimeErr, stage, class, dispatched)
	}
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
