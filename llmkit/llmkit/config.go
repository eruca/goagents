package llmkit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultConfigFile = "config.yaml"

// Config is the host-owned llmkit configuration loaded from LLMKIT_HOME.
// It intentionally stores API key environment variable names, not key values.
type Config struct {
	Home     string          `yaml:"home,omitempty" json:"home,omitempty"`
	Audit    AuditConfig     `yaml:"audit,omitempty" json:"audit,omitempty"`
	Accounts []AccountConfig `yaml:"accounts,omitempty" json:"accounts,omitempty"`
	Models   []ModelConfig   `yaml:"models,omitempty" json:"models,omitempty"`
	Routing  RoutingConfig   `yaml:"routing,omitempty" json:"routing,omitempty"`
}

// AuditConfig controls host-side audit file behavior.
type AuditConfig struct {
	Enabled         bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	RouteEventsFile string `yaml:"route_events_file,omitempty" json:"route_events_file,omitempty"`
	OutcomesFile    string `yaml:"outcomes_file,omitempty" json:"outcomes_file,omitempty"`
}

// AccountConfig names a provider account without storing credentials.
type AccountConfig struct {
	Alias          string `yaml:"alias,omitempty" json:"alias,omitempty"`
	Provider       string `yaml:"provider,omitempty" json:"provider,omitempty"`
	BaseURL        string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	APIKeyEnv      string `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	APIKey         string `yaml:"api_key,omitempty" json:"-"`
	MaxConcurrency int    `yaml:"max_concurrency,omitempty" json:"max_concurrency,omitempty"`
}

// ModelConfig describes one configured model/account route candidate.
type ModelConfig struct {
	Alias              string             `yaml:"alias,omitempty" json:"alias,omitempty"`
	ModelName          string             `yaml:"model,omitempty" json:"model,omitempty"`
	Provider           string             `yaml:"provider,omitempty" json:"provider,omitempty"`
	AccountAlias       string             `yaml:"account_alias,omitempty" json:"account_alias,omitempty"`
	IsLocal            bool               `yaml:"is_local,omitempty" json:"is_local,omitempty"`
	CapabilityLevel    CapabilityLevel    `yaml:"capability_level,omitempty" json:"capability_level,omitempty"`
	SupportsTools      bool               `yaml:"supports_tools,omitempty" json:"supports_tools,omitempty"`
	SupportsJSON       bool               `yaml:"supports_json,omitempty" json:"supports_json,omitempty"`
	ContextWindowClass ContextWindowClass `yaml:"context_window_class,omitempty" json:"context_window_class,omitempty"`
	PriceClass         PriceClass         `yaml:"price_class,omitempty" json:"price_class,omitempty"`
	LatencyClass       LatencyClass       `yaml:"latency_class,omitempty" json:"latency_class,omitempty"`
	MaxConcurrency     int                `yaml:"max_concurrency,omitempty" json:"max_concurrency,omitempty"`
	Tags               []string           `yaml:"tags,omitempty" json:"tags,omitempty"`
}

// RoutingConfig stores declarative routing defaults and host-owned rule text.
// The current library consumes Defaults; concrete rule evaluation is future work.
type RoutingConfig struct {
	Intent   map[string]string `yaml:"intent,omitempty" json:"intent,omitempty"`
	Defaults ProfileDefaults   `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Rules    []RoutingRule     `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// ProfileDefaults maps routing defaults to TaskProfile fields.
type ProfileDefaults struct {
	Complexity  Complexity         `yaml:"complexity,omitempty" json:"complexity,omitempty"`
	Latency     LatencyRequirement `yaml:"latency_requirement,omitempty" json:"latency_requirement,omitempty"`
	FailureCost FailureCost        `yaml:"failure_cost,omitempty" json:"failure_cost,omitempty"`
	Privacy     PrivacyLevel       `yaml:"privacy_level,omitempty" json:"privacy_level,omitempty"`
}

// RoutingRule preserves host-owned declarative preferences from config.yaml.
type RoutingRule struct {
	When   map[string]string `yaml:"when,omitempty" json:"when,omitempty"`
	Prefer []string          `yaml:"prefer,omitempty" json:"prefer,omitempty"`
}

// LoadConfig reads config.yaml from home and validates the host-owned config.
func LoadConfig(home string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(home, defaultConfigFile))
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

// LoadConfigFromEnv resolves LLMKIT_HOME in production mode and loads
// config.yaml from that directory.
func LoadConfigFromEnv(getenv func(string) string) (*Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	home, err := ResolveHome(cwd, getenv, HomeModeProduction)
	if err != nil {
		return nil, err
	}
	return LoadConfig(home)
}

// DefaultTaskProfile returns routing defaults from config, with library
// defaults filling omitted fields.
func (c Config) DefaultTaskProfile() TaskProfile {
	profile := DefaultTaskProfile()
	defaults := c.Routing.Defaults
	if defaults.Complexity != "" {
		profile.Complexity = defaults.Complexity
	}
	if defaults.Latency != "" {
		profile.Latency = defaults.Latency
	}
	if defaults.FailureCost != "" {
		profile.FailureCost = defaults.FailureCost
	}
	if defaults.Privacy != "" {
		profile.Privacy = defaults.Privacy
	}
	return profile
}

// Candidates converts configured models and accounts into route candidates.
func (c Config) Candidates() []Candidate {
	accounts := c.accountMap()
	candidates := make([]Candidate, 0, len(c.Models))
	for _, model := range c.Models {
		account := accounts[model.AccountAlias]
		candidates = append(candidates, Candidate{
			Model: ModelCapability{
				Alias:              model.Alias,
				Provider:           model.Provider,
				IsLocal:            model.IsLocal,
				CapabilityLevel:    model.CapabilityLevel,
				SupportsTools:      model.SupportsTools,
				SupportsJSON:       model.SupportsJSON,
				ContextWindowClass: model.ContextWindowClass,
				PriceClass:         model.PriceClass,
				LatencyClass:       model.LatencyClass,
				MaxConcurrency:     model.MaxConcurrency,
			},
			AccountAlias:          model.AccountAlias,
			AccountMaxConcurrency: account.MaxConcurrency,
		})
	}
	return candidates
}

func (c Config) validate() error {
	accounts := c.accountMap()
	for _, account := range c.Accounts {
		if strings.TrimSpace(account.Alias) == "" {
			return fmt.Errorf("account alias is required")
		}
		if strings.TrimSpace(account.APIKey) != "" {
			return fmt.Errorf("account %q must use api_key_env, not plaintext api_key", account.Alias)
		}
	}
	for _, model := range c.Models {
		if strings.TrimSpace(model.Alias) == "" {
			return fmt.Errorf("model alias is required")
		}
		if strings.TrimSpace(model.AccountAlias) == "" {
			return fmt.Errorf("model %q account_alias is required", model.Alias)
		}
		if _, ok := accounts[model.AccountAlias]; !ok {
			return fmt.Errorf("model %q references unknown account_alias %q", model.Alias, model.AccountAlias)
		}
	}
	return nil
}

func (c Config) accountMap() map[string]AccountConfig {
	accounts := make(map[string]AccountConfig, len(c.Accounts))
	for _, account := range c.Accounts {
		accounts[account.Alias] = account
	}
	return accounts
}
