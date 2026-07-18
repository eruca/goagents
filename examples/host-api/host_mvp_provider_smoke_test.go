//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type mvpProviderMode string

const (
	mvpProviderReady            mvpProviderMode = "ready"
	mvpProviderUnavailable      mvpProviderMode = "unavailable"
	mvpProviderUnregisteredTool mvpProviderMode = "unregistered_tool"
	mvpProviderAPIKey                           = "mvp-smoke-provider-key"
	mvpProviderAPIKeyEnv                        = "HOST_API_MVP_SMOKE_PROVIDER_KEY"
)

type mvpProviderRequest struct {
	Authorization      string
	ToolNames          []string
	HasToolObservation bool
}

type providerBarrier struct {
	entered chan struct{}
	release chan struct{}

	enterOnce sync.Once
	mu        sync.Mutex
	cancelled int
	changed   chan struct{}
}

func newProviderBarrier() *providerBarrier {
	return &providerBarrier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		changed: make(chan struct{}),
	}
}

func (b *providerBarrier) wait(ctx context.Context) bool {
	b.enterOnce.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return true
	case <-ctx.Done():
		b.mu.Lock()
		b.cancelled++
		close(b.changed)
		b.changed = make(chan struct{})
		b.mu.Unlock()
		return false
	}
}

func (b *providerBarrier) Cancellations() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cancelled
}

func (b *providerBarrier) WaitForCancellations(ctx context.Context, count int) error {
	for {
		b.mu.Lock()
		if b.cancelled >= count {
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type mvpProviderStub struct {
	server *httptest.Server

	mu              sync.Mutex
	mode            mvpProviderMode
	barrier         *providerBarrier
	requests        []mvpProviderRequest
	requestsChanged chan struct{}
}

type mvpChatRequest struct {
	Messages []struct {
		Role string `json:"role"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func TestMVPProviderBarrierReleasesConcurrentRequestsAndSnapshotsExactCount(t *testing.T) {
	barrier := newProviderBarrier()
	provider := newMVPProviderStub(t, mvpProviderReady)
	provider.SetBarrier(barrier)

	results := make(chan mvpProviderCallResult, 2)
	for range 2 {
		go func() {
			results <- callMVPProvider(t.Context(), provider.URL())
		}()
	}

	waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	requests, err := provider.WaitForRequests(waitCtx, 2)
	if err != nil {
		t.Fatalf("wait for provider requests: %v", err)
	}
	select {
	case <-barrier.entered:
	case <-waitCtx.Done():
		t.Fatalf("wait for provider barrier entry: %v", waitCtx.Err())
	}
	if len(requests) != 2 || len(provider.Requests()) != 2 {
		t.Fatalf("provider request count = %d/%d, want exact 2", len(requests), len(provider.Requests()))
	}

	close(barrier.release)
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil || result.status != http.StatusOK {
				t.Fatalf("provider call = (%d, %v), want (200, nil)", result.status, result.err)
			}
		case <-waitCtx.Done():
			t.Fatalf("wait for released provider call: %v", waitCtx.Err())
		}
	}
}

func TestMVPProviderBarrierRecordsRequestContextCancellation(t *testing.T) {
	barrier := newProviderBarrier()
	provider := newMVPProviderStub(t, mvpProviderReady)
	provider.SetBarrier(barrier)

	requestCtx, cancelRequest := context.WithCancel(t.Context())
	result := make(chan mvpProviderCallResult, 1)
	go func() {
		result <- callMVPProvider(requestCtx, provider.URL())
	}()

	waitCtx, cancelWait := context.WithTimeout(t.Context(), time.Second)
	defer cancelWait()
	if _, err := provider.WaitForRequests(waitCtx, 1); err != nil {
		t.Fatalf("wait for provider request: %v", err)
	}
	select {
	case <-barrier.entered:
	case <-waitCtx.Done():
		t.Fatalf("wait for provider barrier entry: %v", waitCtx.Err())
	}
	cancelRequest()
	if err := barrier.WaitForCancellations(waitCtx, 1); err != nil {
		t.Fatalf("wait for provider request cancellation: %v", err)
	}
	if got := barrier.Cancellations(); got != 1 {
		t.Fatalf("provider cancellations = %d, want 1", got)
	}
	if got := len(provider.Requests()); got != 1 {
		t.Fatalf("provider requests = %d, want exact 1", got)
	}

	select {
	case call := <-result:
		if !errors.Is(call.err, context.Canceled) {
			t.Fatalf("cancelled provider call error = %v, want context canceled", call.err)
		}
	case <-waitCtx.Done():
		t.Fatalf("wait for cancelled provider call: %v", waitCtx.Err())
	}
}

type mvpProviderCallResult struct {
	status int
	err    error
}

func callMVPProvider(ctx context.Context, providerURL string) mvpProviderCallResult {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		providerURL+"/v1/chat/completions",
		strings.NewReader(`{"messages":[],"tools":[]}`),
	)
	if err != nil {
		return mvpProviderCallResult{err: err}
	}
	request.Header.Set("Authorization", "Bearer "+mvpProviderAPIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return mvpProviderCallResult{err: err}
	}
	defer response.Body.Close()
	return mvpProviderCallResult{status: response.StatusCode}
}

func newMVPProviderStub(t *testing.T, mode mvpProviderMode) *mvpProviderStub {
	t.Helper()
	stub := &mvpProviderStub{
		mode:            mode,
		requestsChanged: make(chan struct{}),
	}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *mvpProviderStub) URL() string {
	return s.server.URL
}

func (s *mvpProviderStub) SetMode(mode mvpProviderMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
}

func (s *mvpProviderStub) SetBarrier(barrier *providerBarrier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.barrier = barrier
}

func (s *mvpProviderStub) Requests() []mvpProviderRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneMVPProviderRequests(s.requests)
}

func (s *mvpProviderStub) WaitForRequests(ctx context.Context, count int) ([]mvpProviderRequest, error) {
	for {
		s.mu.Lock()
		if len(s.requests) >= count {
			requests := cloneMVPProviderRequests(s.requests)
			s.mu.Unlock()
			return requests, nil
		}
		changed := s.requestsChanged
		s.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func cloneMVPProviderRequests(recorded []mvpProviderRequest) []mvpProviderRequest {
	requests := make([]mvpProviderRequest, len(recorded))
	copy(requests, recorded)
	for index, request := range recorded {
		requests[index].ToolNames = append([]string(nil), request.ToolNames...)
	}
	return requests
}

func (s *mvpProviderStub) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+mvpProviderAPIKey {
		writeMVPProviderError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var payload mvpChatRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeMVPProviderError(w, http.StatusBadRequest, "invalid request")
		return
	}
	toolNames := make([]string, 0, len(payload.Tools))
	for _, tool := range payload.Tools {
		toolNames = append(toolNames, tool.Function.Name)
	}
	hasToolObservation := false
	for _, message := range payload.Messages {
		if message.Role == "tool" {
			hasToolObservation = true
			break
		}
	}

	s.mu.Lock()
	mode := s.mode
	barrier := s.barrier
	s.requests = append(s.requests, mvpProviderRequest{
		Authorization:      r.Header.Get("Authorization"),
		ToolNames:          toolNames,
		HasToolObservation: hasToolObservation,
	})
	close(s.requestsChanged)
	s.requestsChanged = make(chan struct{})
	s.mu.Unlock()
	if barrier != nil && !barrier.wait(r.Context()) {
		return
	}

	switch mode {
	case mvpProviderUnavailable:
		writeMVPProviderError(w, http.StatusServiceUnavailable, "mvp smoke unavailable")
	case mvpProviderUnregisteredTool:
		writeMVPProviderToolCall(w, "call-unregistered", "unregistered_tool")
	default:
		if len(toolNames) > 0 && !hasToolObservation {
			writeMVPProviderToolCall(w, "call-record-review", recordReviewToolName)
			return
		}
		writeMVPProviderText(w, "mvp smoke response")
	}
}

func writeMVPProviderError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}})
}

func writeMVPProviderText(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}},
		"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
	})
}

func writeMVPProviderToolCall(w http.ResponseWriter, id, name string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id":       id,
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": `{}`},
			}},
		}}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
	})
}

func writeMVPLLMKitConfig(t *testing.T, runtimeHome, providerURL string) {
	t.Helper()
	home := filepath.Join(runtimeHome, ".llmkit")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create MVP llmkit home: %v", err)
	}
	config := fmt.Sprintf(`
audit:
  enabled: true
  route_events_file: route-events.jsonl
  outcomes_file: outcomes.jsonl
accounts:
  - alias: mvp-smoke-account
    provider: openai_compatible
    base_url: %q
    api_key_env: %s
    max_concurrency: 2
models:
  - alias: mvp-smoke-model
    model: mvp-smoke-model
    provider: openai_compatible
    account_alias: mvp-smoke-account
    capability_level: advanced
    supports_tools: true
    supports_json: true
    context_window_class: long
    price_class: free
    latency_class: fast
    max_concurrency: 2
routing:
  defaults:
    complexity: simple
    latency_requirement: normal
    failure_cost: low
    privacy_level: cloud_allowed
`, strings.TrimRight(providerURL, "/")+"/v1", mvpProviderAPIKeyEnv)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(strings.TrimSpace(config)+"\n"), 0o600); err != nil {
		t.Fatalf("write MVP llmkit config: %v", err)
	}
}

func mvpHostEnvironment(skillRoot string) map[string]string {
	return map[string]string{
		hostAPISkillRootEnv:  skillRoot,
		mvpProviderAPIKeyEnv: mvpProviderAPIKey,
	}
}
