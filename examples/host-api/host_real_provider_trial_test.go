//go:build darwin && cgo && hostapisystemsmoke && provideracceptance

package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eruca/runkit"
	"github.com/eruca/workflowkit"
)

const realProviderTrialModelAlias = "qwen-local-trial"

func TestHostAPIProcessRealProviderLocalTrial(t *testing.T) {
	providerConfig := requireRealProviderConfig(t)
	requireInteractiveLoginKeychain(t)
	oidc := newOIDCTestProvider(t)
	binary := buildHostBinary(t)
	runtimeHome := t.TempDir()
	writeHostRealProviderTrialConfig(t, runtimeHome, providerConfig)
	token := oidc.mintToken(t, "operator-local-trial", "host-api", time.Now().Add(time.Hour))
	keychainService := fmt.Sprintf("%s.smoke.real-provider.%d", localApprovalKeychainService, time.Now().UnixNano())
	cleanupKeychain := smokeKeychainCleanup(t, keychainService, localApprovalKeyID)
	t.Cleanup(cleanupKeychain)
	environment := map[string]string{
		hostAPISkillRootEnv:     "",
		"OPENAI_COMPAT_API_KEY": providerConfig.APIKey,
	}

	first := startHostProcessWithEnv(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment)
	first.client.Timeout = 90 * time.Second
	models, status := processJSON[modelsResponse](t, first, http.MethodGet, "/llmkit/models", nil, "")
	if status != http.StatusOK || len(models.Models) != 1 || models.Models[0].Alias != realProviderTrialModelAlias || models.Models[0].Provider != "openai_compatible" {
		t.Fatalf("models status=%d models=%#v, want one configured OpenAI-compatible model", status, models.Models)
	}

	created, status := processJSON[workflowResponse](t, first, http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-real-provider-local-trial",
		"input": "Return a concise operator trial summary without calling tools.",
		"task_profile": map[string]any{
			"complexity":      "hard",
			"failure_cost":    "high",
			"privacy":         "cloud_allowed",
			"needs_reasoning": true,
		},
	}, "")
	if status != http.StatusAccepted || created.Status != string(workflowkit.StatusWaitingApproval) || created.AgentApproval != nil || created.OutputRef == "" || created.AgentRunID == "" || created.ApprovalRef == "" {
		t.Fatalf("create status=%d workflow=%#v, want persisted final approval wait", status, created)
	}
	run, status := processJSON[agentRunResponse](t, first, http.MethodGet, "/agent-runs/"+created.AgentRunID, nil, "")
	if status != http.StatusOK || run.Status != string(runkit.StatusSucceeded) || run.Summary.LLMCalls != 1 || run.Summary.ToolCalls != 0 {
		t.Fatalf("agent run status=%d run=%#v, want one successful real Provider call", status, run)
	}
	routes, status := processJSON[llmRoutesResponse](t, first, http.MethodGet, "/workflows/"+created.ID+"/llm-routes", nil, "")
	if status != http.StatusOK || !realProviderTrialRouteSucceeded(routes.Routes) {
		t.Fatalf("routes status=%d routes=%#v, want successful selected real Provider route", status, routes.Routes)
	}

	_, invalidStatus := processJSON[errorResponse](t, first, http.MethodPost, "/workflows/"+created.ID+"/approve", map[string]any{
		"note": "invalid local trial approval",
	}, "Bearer invalid")
	if invalidStatus != http.StatusUnauthorized {
		t.Fatalf("invalid approval status=%d, want 401", invalidStatus)
	}
	unchanged, status := processJSON[workflowResponse](t, first, http.MethodGet, "/workflows/"+created.ID, nil, "")
	if status != http.StatusOK || unchanged.Status != created.Status || unchanged.OutputRef != created.OutputRef || unchanged.AgentRunID != created.AgentRunID || unchanged.ApprovalRef != created.ApprovalRef {
		t.Fatalf("workflow after invalid approval status=%d workflow=%#v, want unchanged approval wait", status, unchanged)
	}
	stopHostProcess(t, first)

	second := startHostProcessWithEnv(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment)
	second.client.Timeout = 30 * time.Second
	persisted, status := processJSON[workflowResponse](t, second, http.MethodGet, "/workflows/"+created.ID, nil, "")
	if status != http.StatusOK || persisted.Status != created.Status || persisted.OutputRef != created.OutputRef || persisted.AgentRunID != created.AgentRunID || persisted.ApprovalRef != created.ApprovalRef {
		t.Fatalf("workflow after first restart status=%d workflow=%#v, want persisted approval boundary", status, persisted)
	}
	completed, status := processJSON[workflowResponse](t, second, http.MethodPost, "/workflows/"+created.ID+"/approve", map[string]any{
		"note": "real Provider local trial accepted",
	}, "Bearer "+token)
	if status != http.StatusOK || completed.Status != string(workflowkit.StatusSucceeded) || completed.OutputRef == "" || completed.OutputRef == created.OutputRef || completed.AgentRunID != created.AgentRunID {
		t.Fatalf("final approval status=%d workflow=%#v, want succeeded", status, completed)
	}
	stopHostProcess(t, second)

	third := startHostProcessWithEnv(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment)
	third.client.Timeout = 30 * time.Second
	assertMVPCompletedWorkflow(t, third, completed, 0)
	stopHostProcess(t, third)

	processOutput := first.output.String() + second.output.String() + third.output.String()
	for label, sensitive := range map[string]string{
		"Provider API key":  providerConfig.APIKey,
		"Provider endpoint": strings.TrimRight(providerConfig.BaseURL, "/"),
		"OIDC bearer token": token,
	} {
		if strings.Contains(processOutput, sensitive) {
			t.Fatalf("host process output leaked %s", label)
		}
	}
	cleanupKeychain()
}

func realProviderTrialRouteSucceeded(routes []llmRouteResponse) bool {
	for _, route := range routes {
		if route.Selected && route.ModelAlias == realProviderTrialModelAlias && route.Provider == "openai_compatible" && route.Outcome != nil && route.Outcome.Success {
			return true
		}
	}
	return false
}

func writeHostRealProviderTrialConfig(t *testing.T, runtimeHome string, config realProviderConfig) {
	t.Helper()
	home := filepath.Join(runtimeHome, ".llmkit")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create real Provider trial llmkit home: %v", err)
	}
	content := fmt.Sprintf(`
audit:
  enabled: true
  route_events_file: route-events.jsonl
  outcomes_file: outcomes.jsonl
accounts:
  - alias: qwen-local-trial-account
    provider: openai_compatible
    base_url: %q
    api_key_env: OPENAI_COMPAT_API_KEY
    max_concurrency: 1
models:
  - alias: %s
    model: %q
    provider: openai_compatible
    account_alias: qwen-local-trial-account
    capability_level: advanced
    supports_tools: true
    supports_json: true
    context_window_class: long
    price_class: medium
    latency_class: normal
    max_concurrency: 1
routing:
  defaults:
    complexity: hard
    latency_requirement: normal
    failure_cost: high
    privacy_level: cloud_allowed
`, strings.TrimRight(config.BaseURL, "/"), realProviderTrialModelAlias, config.Model)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatalf("write real Provider trial llmkit config: %v", err)
	}
}
