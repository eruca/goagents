package llmkit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigFromHomeBuildsCandidatesAndDefaults(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `
home: .llmkit
audit:
  enabled: true
accounts:
  - alias: local-dev
    provider: local
    api_key_env: LLMKIT_LOCAL_API_KEY
    max_concurrency: 4
  - alias: cloud-primary
    provider: openai
    api_key_env: OPENAI_API_KEY
    max_concurrency: 8
models:
  - alias: local-free
    provider: local
    account_alias: local-dev
    is_local: true
    capability_level: simple
    context_window_class: medium
    price_class: free
    latency_class: fast
    max_concurrency: 4
  - alias: cloud-advanced
    provider: openai
    account_alias: cloud-primary
    capability_level: advanced
    supports_tools: true
    supports_json: true
    context_window_class: long
    price_class: high
    latency_class: normal
    max_concurrency: 8
routing:
  defaults:
    complexity: medium
    latency_requirement: normal
    failure_cost: medium
    privacy_level: cloud_allowed
`)

	config, err := LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if !config.Audit.Enabled {
		t.Fatal("Audit.Enabled = false, want true")
	}

	profile := config.DefaultTaskProfile()
	if profile.Source != ProfileSourceDefault {
		t.Fatalf("default profile source = %q, want default", profile.Source)
	}
	if profile.Complexity != ComplexityMedium {
		t.Fatalf("default complexity = %q, want medium", profile.Complexity)
	}
	if profile.Latency != LatencyNormal {
		t.Fatalf("default latency = %q, want normal", profile.Latency)
	}
	if profile.FailureCost != FailureCostMedium {
		t.Fatalf("default failure cost = %q, want medium", profile.FailureCost)
	}
	if profile.Privacy != PrivacyCloudAllowed {
		t.Fatalf("default privacy = %q, want cloud_allowed", profile.Privacy)
	}

	candidates := config.Candidates()
	if len(candidates) != 2 {
		t.Fatalf("candidates len = %d, want 2", len(candidates))
	}
	local := candidates[0]
	if local.Model.Alias != "local-free" {
		t.Fatalf("local alias = %q, want local-free", local.Model.Alias)
	}
	if local.AccountAlias != "local-dev" {
		t.Fatalf("local account alias = %q, want local-dev", local.AccountAlias)
	}
	if local.AccountMaxConcurrency != 4 {
		t.Fatalf("local account max concurrency = %d, want 4", local.AccountMaxConcurrency)
	}
	if !local.Model.IsLocal {
		t.Fatal("local model IsLocal = false, want true")
	}
	if local.Model.PriceClass != PriceFree {
		t.Fatalf("local price class = %q, want free", local.Model.PriceClass)
	}

	cloud := candidates[1]
	if cloud.Model.Alias != "cloud-advanced" {
		t.Fatalf("cloud alias = %q, want cloud-advanced", cloud.Model.Alias)
	}
	if cloud.AccountAlias != "cloud-primary" {
		t.Fatalf("cloud account alias = %q, want cloud-primary", cloud.AccountAlias)
	}
	if !cloud.Model.SupportsTools || !cloud.Model.SupportsJSON {
		t.Fatal("cloud model should support tools and JSON")
	}
}

func TestLoadConfigFromEnvUsesLLMKITHome(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `
accounts:
  - alias: local-dev
    provider: local
models:
  - alias: local-free
    provider: local
    account_alias: local-dev
    capability_level: simple
    context_window_class: medium
    price_class: free
    latency_class: fast
`)

	config, err := LoadConfigFromEnv(func(key string) string {
		if key == "LLMKIT_HOME" {
			return home
		}
		return ""
	})
	if err != nil {
		t.Fatalf("LoadConfigFromEnv() error = %v", err)
	}
	if got := config.Candidates()[0].Model.Alias; got != "local-free" {
		t.Fatalf("candidate alias = %q, want local-free", got)
	}
}

func TestLoadConfigFromEnvRequiresLLMKITHome(t *testing.T) {
	_, err := LoadConfigFromEnv(func(string) string { return "" })
	if err == nil {
		t.Fatal("LoadConfigFromEnv() error = nil, want missing LLMKIT_HOME error")
	}
}

func TestLoadConfigRejectsPlaintextAPIKey(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `
accounts:
  - alias: unsafe
    provider: openai
    api_key: sk-this-should-not-be-here
models:
  - alias: cloud
    provider: openai
    account_alias: unsafe
    capability_level: advanced
    context_window_class: long
    price_class: high
    latency_class: normal
`)

	_, err := LoadConfig(home)
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want plaintext API key rejection")
	}
}

func TestLoadConfigRejectsUnknownAccountAlias(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, `
accounts:
  - alias: local-dev
    provider: local
models:
  - alias: local-free
    provider: local
    account_alias: missing-account
    capability_level: simple
    context_window_class: medium
    price_class: free
    latency_class: fast
`)

	_, err := LoadConfig(home)
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want unknown account alias error")
	}
}

func writeConfig(t *testing.T, home, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
