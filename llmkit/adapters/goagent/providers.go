package goagent

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/eruca/goagent/extensions/providers/openaiapi"
	"github.com/eruca/llmkit/llmkit"
)

// OpenAICompatibleProvidersFromConfig builds provider clients for configured
// OpenAI-compatible models. It reads API key values from getenv at construction
// time and never stores keys in llmkit audit records.
func OpenAICompatibleProvidersFromConfig(config llmkit.Config, getenv func(string) string, httpClient *http.Client) (map[string]ProviderClient, error) {
	accounts := accountConfigsByAlias(config.Accounts)
	providers := make(map[string]ProviderClient)
	for _, model := range config.Models {
		account, ok := accounts[model.AccountAlias]
		if !ok {
			return nil, fmt.Errorf("model %q references unknown account_alias %q", model.Alias, model.AccountAlias)
		}
		if !isOpenAICompatibleProvider(model.Provider, account.Provider) {
			continue
		}
		if strings.TrimSpace(account.BaseURL) == "" {
			return nil, fmt.Errorf("account %q base_url is required for OpenAI-compatible provider", account.Alias)
		}
		modelName := strings.TrimSpace(model.ModelName)
		if modelName == "" {
			modelName = model.Alias
		}
		client, err := openaiapi.New(openaiapi.Config{
			BaseURL:    account.BaseURL,
			APIKey:     apiKeyFromEnv(getenv, account.APIKeyEnv),
			Model:      modelName,
			HTTPClient: httpClient,
		})
		if err != nil {
			return nil, fmt.Errorf("create provider for model %q: %w", model.Alias, err)
		}
		providers[model.Alias] = client
	}
	return providers, nil
}

func accountConfigsByAlias(accounts []llmkit.AccountConfig) map[string]llmkit.AccountConfig {
	byAlias := make(map[string]llmkit.AccountConfig, len(accounts))
	for _, account := range accounts {
		byAlias[account.Alias] = account
	}
	return byAlias
}

func isOpenAICompatibleProvider(modelProvider, accountProvider string) bool {
	provider := strings.ToLower(strings.TrimSpace(modelProvider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(accountProvider))
	}
	switch provider {
	case "openai", "openai_compatible", "openai-compatible", "local":
		return true
	default:
		return false
	}
}

func apiKeyFromEnv(getenv func(string) string, name string) string {
	if getenv == nil || strings.TrimSpace(name) == "" {
		return ""
	}
	return getenv(name)
}
