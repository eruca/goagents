//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

type mvpProviderStub struct {
	server *httptest.Server

	mu       sync.Mutex
	mode     mvpProviderMode
	requests []mvpProviderRequest
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

func newMVPProviderStub(t *testing.T, mode mvpProviderMode) *mvpProviderStub {
	t.Helper()
	stub := &mvpProviderStub{mode: mode}
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

func (s *mvpProviderStub) Requests() []mvpProviderRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	requests := make([]mvpProviderRequest, len(s.requests))
	for index, request := range s.requests {
		requests[index] = request
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
	s.requests = append(s.requests, mvpProviderRequest{
		Authorization:      r.Header.Get("Authorization"),
		ToolNames:          toolNames,
		HasToolObservation: hasToolObservation,
	})
	s.mu.Unlock()

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
