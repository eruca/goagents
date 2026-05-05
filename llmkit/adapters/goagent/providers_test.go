package goagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eruca/goagent/ports"
	"github.com/eruca/llmkit/llmkit"
)

func TestOpenAICompatibleProvidersFromConfigUsesBaseURLModelAndAPIKeyEnv(t *testing.T) {
	var gotAuthorization string
	var gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	config := llmkit.Config{
		Accounts: []llmkit.AccountConfig{{
			Alias:     "cloud-primary",
			Provider:  "openai_compatible",
			BaseURL:   server.URL + "/v1",
			APIKeyEnv: "OPENAI_API_KEY",
		}},
		Models: []llmkit.ModelConfig{{
			Alias:        "cloud-advanced",
			ModelName:    "gpt-advanced",
			Provider:     "openai_compatible",
			AccountAlias: "cloud-primary",
		}},
	}

	providers, err := OpenAICompatibleProvidersFromConfig(config, func(key string) string {
		if key == "OPENAI_API_KEY" {
			return "secret-value"
		}
		return ""
	}, server.Client())
	if err != nil {
		t.Fatalf("OpenAICompatibleProvidersFromConfig() error = %v", err)
	}

	provider := providers["cloud-advanced"]
	if provider == nil {
		t.Fatal("provider cloud-advanced missing")
	}
	resp, err := provider.Chat(context.Background(), ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("provider Chat() error = %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("response content = %q, want ok", resp.Content)
	}
	if gotModel != "gpt-advanced" {
		t.Fatalf("request model = %q, want gpt-advanced", gotModel)
	}
	if gotAuthorization != "Bearer secret-value" {
		t.Fatalf("authorization header = %q, want bearer key", gotAuthorization)
	}
}

func TestOpenAICompatibleProvidersFromConfigRejectsMissingBaseURL(t *testing.T) {
	config := llmkit.Config{
		Accounts: []llmkit.AccountConfig{{
			Alias:    "cloud-primary",
			Provider: "openai_compatible",
		}},
		Models: []llmkit.ModelConfig{{
			Alias:        "cloud-advanced",
			ModelName:    "gpt-advanced",
			Provider:     "openai_compatible",
			AccountAlias: "cloud-primary",
		}},
	}

	_, err := OpenAICompatibleProvidersFromConfig(config, func(string) string { return "" }, nil)
	if err == nil {
		t.Fatal("OpenAICompatibleProvidersFromConfig() error = nil, want missing base URL error")
	}
}
