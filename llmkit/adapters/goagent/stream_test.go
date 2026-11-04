package goagent

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/llmkit"
)

// streamingProviderClient streams its chunks, then fails with err if set.
type streamingProviderClient struct {
	chunks       []string
	err          error
	streamCalls  int
	streamDigest [sha256.Size]byte
}

func (p *streamingProviderClient) Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
	return nil, errors.New("Chat called on a streaming route")
}

func (p *streamingProviderClient) ChatStream(_ context.Context, _ ports.ChatRequest, onDelta func(ports.ChatDelta) error) (*ports.ChatResponse, error) {
	p.streamCalls++
	for _, chunk := range p.chunks {
		if err := onDelta(ports.ChatDelta{Content: chunk}); err != nil {
			return nil, err
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	return &ports.ChatResponse{Content: strings.Join(p.chunks, "")}, nil
}

func (p *streamingProviderClient) ProviderStreamRequestSHA256(ports.ChatRequest) ([sha256.Size]byte, error) {
	return p.streamDigest, nil
}

func collect(deltas *[]string) func(ports.ChatDelta) error {
	return func(delta ports.ChatDelta) error {
		*deltas = append(*deltas, delta.Content)
		return nil
	}
}

func TestClientChatStreamForwardsProviderDeltasAndRecordsOutcome(t *testing.T) {
	local := &streamingProviderClient{chunks: []string{"loc", "al"}}
	recorder := &fakeRecorder{}
	client := NewClient(Config{
		Candidates:            testCandidates()[:1],
		Providers:             map[string]ProviderClient{"local-small": local},
		ProfileProvider:       fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{RouteID: "route-stream", TaskID: "task-stream", Attempt: 1}),
		Recorder:              recorder,
		RecordOutcomes:        true,
	})

	var deltas []string
	resp, err := client.ChatStream(context.Background(), ports.ChatRequest{}, collect(&deltas))
	if err != nil || resp.Content != "local" {
		t.Fatalf("ChatStream() = %+v, %v", resp, err)
	}
	if strings.Join(deltas, "|") != "loc|al" {
		t.Fatalf("deltas = %q", deltas)
	}
	if len(recorder.outcomes) != 1 || !recorder.outcomes[0].Success {
		t.Fatalf("outcomes = %+v, want one success", recorder.outcomes)
	}
}

func TestClientChatStreamDeliversNonStreamingProviderAsOneDelta(t *testing.T) {
	local := &fakeProviderClient{response: &ports.ChatResponse{Content: "whole answer"}}
	client := NewClient(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": local},
		ProfileProvider: fixedProfile(simpleProfile()),
	})

	var deltas []string
	resp, err := client.ChatStream(context.Background(), ports.ChatRequest{}, collect(&deltas))
	if err != nil || resp.Content != "whole answer" || strings.Join(deltas, "|") != "whole answer" {
		t.Fatalf("response = %+v, deltas = %q, err = %v", resp, deltas, err)
	}
}

func TestClientChatStreamDeliversNonStreamingProviderToolCalls(t *testing.T) {
	local := &fakeProviderClient{response: &ports.ChatResponse{ToolCalls: []ports.ToolCall{{ID: "call_1", Name: "lookup", Input: []byte(`{"q":"go"}`)}}}}
	client := NewClient(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": local},
		ProfileProvider: fixedProfile(simpleProfile()),
	})

	var pieces []ports.ToolCallDelta
	_, err := client.ChatStream(context.Background(), ports.ChatRequest{}, func(delta ports.ChatDelta) error {
		if delta.ToolCall != nil {
			pieces = append(pieces, *delta.ToolCall)
		}
		return nil
	})
	if err != nil || len(pieces) != 1 || pieces[0] != (ports.ToolCallDelta{ID: "call_1", Name: "lookup", Arguments: `{"q":"go"}`}) {
		t.Fatalf("tool call pieces = %+v, err = %v; want the whole call as one piece", pieces, err)
	}
}

func TestClientChatStreamFallsBackOnlyBeforeOutput(t *testing.T) {
	for _, tc := range []struct {
		name         string
		local        *streamingProviderClient
		wantFallback bool
	}{
		{"failure before output falls back", &streamingProviderClient{err: errors.New("local down")}, true},
		{"failure after output does not", &streamingProviderClient{chunks: []string{"half an "}, err: errors.New("local dropped")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &streamingProviderClient{chunks: []string{"cloud answer"}}
			client := NewClient(Config{
				Candidates:      testCandidates(),
				Providers:       map[string]ProviderClient{"local-small": tc.local, "cloud-advanced": cloud},
				ProfileProvider: fixedProfile(simpleProfile()),
			})

			var deltas []string
			resp, err := client.ChatStream(context.Background(), ports.ChatRequest{}, collect(&deltas))
			if tc.wantFallback {
				if err != nil || resp.Content != "cloud answer" || strings.Join(deltas, "|") != "cloud answer" {
					t.Fatalf("response = %+v, deltas = %q, err = %v; want cloud fallback", resp, deltas, err)
				}
				return
			}
			if err == nil || cloud.streamCalls != 0 || strings.Join(deltas, "|") != "half an " {
				t.Fatalf("err = %v, cloud calls = %d, deltas = %q; want failure without fallback", err, cloud.streamCalls, deltas)
			}
		})
	}
}

func TestClientChatStreamPreDispatchUsesStreamRequestDigest(t *testing.T) {
	local := &streamingProviderClient{chunks: []string{"ok"}, streamDigest: sha256.Sum256([]byte("stream body"))}
	var got PreDispatchRequest
	client := NewClient(Config{
		Candidates:            testCandidates()[:1],
		Providers:             map[string]ProviderClient{"local-small": local},
		ProfileProvider:       fixedProfile(simpleProfile()),
		RouteMetadataProvider: fixedRouteMetadata(RouteMetadata{RouteID: "route-stream", TaskID: "task-stream", Attempt: 1}),
		PreDispatch: PreDispatchConfig{
			MetadataProvider: fixedPreDispatchMetadata(PreDispatchMetadata{
				ProviderCallIndex: 1,
				RunRequestSHA256:  sha256.Sum256([]byte("run")),
			}),
			Hook: func(_ context.Context, req PreDispatchRequest) error {
				got = req
				return nil
			},
		},
	})

	var deltas []string
	if _, err := client.ChatStream(context.Background(), ports.ChatRequest{}, collect(&deltas)); err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if got.ProviderRequestSHA256 != local.streamDigest {
		t.Fatalf("pre-dispatch digest = %x, want the stream body digest", got.ProviderRequestSHA256)
	}
}

func TestClientChatStreamConsumerFailureLeavesProviderHealthy(t *testing.T) {
	local := &streamingProviderClient{chunks: []string{"a", "b"}}
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{FailureCooldownThreshold: 1})
	client := NewClient(Config{
		Candidates:      testCandidates()[:1],
		Providers:       map[string]ProviderClient{"local-small": local},
		ProfileProvider: fixedProfile(simpleProfile()),
		HealthStore:     health,
	})
	disconnected := errors.New("client disconnected")
	for range 3 {
		_, err := client.ChatStream(context.Background(), ports.ChatRequest{}, func(ports.ChatDelta) error { return disconnected })
		if !errors.Is(err, disconnected) {
			t.Fatalf("ChatStream error = %v, want the consumer's error", err)
		}
		var runtimeErr *RuntimeError
		if !errors.As(err, &runtimeErr) || runtimeErr.Class != llmkit.ErrorClassStreamConsumer {
			t.Fatalf("ChatStream error = %v, want class %s", err, llmkit.ErrorClassStreamConsumer)
		}
	}
	for _, entry := range health.Snapshot().Entries {
		if entry.FailureStreak != 0 || entry.Availability == llmkit.ProviderUnavailable || entry.InFlight != 0 {
			t.Fatalf("health entry = %+v, want a healthy provider with no slot held", entry)
		}
	}
	if _, err := client.ChatStream(context.Background(), ports.ChatRequest{}, func(ports.ChatDelta) error { return nil }); err != nil {
		t.Fatalf("ChatStream after consumer failures = %v, want success", err)
	}
}

func TestClientReleasesSlotWhenProviderPanics(t *testing.T) {
	local := &fakeProviderClient{chat: func(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) { panic("provider bug") }}
	health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{})
	candidates := testCandidates()[:1]
	candidates[0].ProviderMaxConcurrency = 1
	client := NewClient(Config{
		Candidates:      candidates,
		Providers:       map[string]ProviderClient{"local-small": local},
		ProfileProvider: fixedProfile(simpleProfile()),
		HealthStore:     health,
	})
	for range 2 {
		func() {
			defer func() { _ = recover() }()
			_, _ = client.Chat(context.Background(), ports.ChatRequest{})
		}()
	}
	for _, entry := range health.Snapshot().Entries {
		if entry.InFlight != 0 {
			t.Fatalf("in flight after recovered panics = %d, want 0", entry.InFlight)
		}
	}
}
