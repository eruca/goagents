package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoagentRoutingExampleWritesRouteAudit(t *testing.T) {
	home := t.TempDir()
	server := startOpenAICompatibleServer(t)
	writeExampleConfig(t, home, server.URL)

	cmd := exec.Command("go", "run", ".")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(),
		"LLMKIT_HOME="+home,
		"LLMKIT_LOCAL_API_KEY=test-local-key",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run . failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Final answer: routed through llmkit.") {
		t.Fatalf("output = %s, want final answer", output)
	}

	data, err := os.ReadFile(filepath.Join(home, "route-events.jsonl"))
	if err != nil {
		t.Fatalf("read route events: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("route event lines = %d, want 1", len(lines))
	}
	var event struct {
		RouteID      string `json:"route_id"`
		TaskID       string `json:"task_id"`
		Attempt      int    `json:"attempt"`
		ModelAlias   string `json:"model_alias"`
		AccountAlias string `json:"account_alias"`
		Provider     string `json:"provider"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("decode route event: %v", err)
	}
	if event.RouteID == "" || event.TaskID == "" || event.Attempt != 1 {
		t.Fatalf("route metadata = %+v, want route/task ids and attempt 1", event)
	}
	if event.ModelAlias != "local-free" {
		t.Fatalf("event model alias = %q, want local-free", event.ModelAlias)
	}
	if event.AccountAlias != "local-dev" {
		t.Fatalf("event account alias = %q, want local-dev", event.AccountAlias)
	}
	if event.Provider != "local" {
		t.Fatalf("event provider = %q, want local", event.Provider)
	}
}

func startOpenAICompatibleServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("request path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-local-key" {
			t.Fatalf("authorization = %q, want bearer test-local-key", got)
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "local-small" {
			t.Fatalf("model = %q, want local-small", req.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Final answer: routed through llmkit."}}],"usage":{"prompt_tokens":4,"completion_tokens":5}}`))
	}))
}

func writeExampleConfig(t *testing.T, home, baseURL string) {
	t.Helper()
	config := fmt.Sprintf(`
accounts:
  - alias: local-dev
    provider: local
    base_url: %s/v1
    api_key_env: LLMKIT_LOCAL_API_KEY
    max_concurrency: 2
models:
  - alias: local-free
    model: local-small
    provider: local
    account_alias: local-dev
    is_local: true
    capability_level: simple
    context_window_class: medium
    price_class: free
    latency_class: fast
routing:
  defaults:
    complexity: simple
    latency_requirement: normal
    failure_cost: low
    privacy_level: local_preferred
`, baseURL)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
