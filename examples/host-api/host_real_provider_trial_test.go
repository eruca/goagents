//go:build darwin && cgo && hostapisystemsmoke && provideracceptance

package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/llmkit/llmkit"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
)

const (
	realProviderTrialAccountAlias = "qwen-local-trial-account"
	realProviderTrialModelAlias   = "qwen-local-trial"
)

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
	redactions := []string{
		providerConfig.APIKey,
		providerConfig.BaseURL,
		strings.TrimRight(providerConfig.BaseURL, "/"),
		token,
	}

	first := startHostProcessWithEnvAndRedactions(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment, redactions)
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
	if status != http.StatusAccepted || created.Status != string(workflowkit.StatusWaitingApproval) || created.AgentApproval != nil || created.InputRef == "" || created.OutputRef == "" || created.AgentRunID == "" || created.ApprovalRef == "" || created.WaitingReason == "" || !reflect.DeepEqual(created.Completed, []string{"ingest", "agent_review"}) {
		t.Fatalf("create status=%d workflow=%#v, want persisted final approval wait", status, created)
	}
	run, status := processJSON[agentRunResponse](t, first, http.MethodGet, "/agent-runs/"+created.AgentRunID, nil, "")
	if status != http.StatusOK || !realProviderTrialRunMatches(run, created, created.OutputRef) {
		t.Fatalf("agent run status=%d run=%#v, want one successful real Provider call", status, run)
	}
	routes, status := processJSON[llmRoutesResponse](t, first, http.MethodGet, "/workflows/"+created.ID+"/llm-routes", nil, "")
	if status != http.StatusOK || !realProviderTrialRoutesMatch(routes, created, false) {
		t.Fatalf("routes status=%d routes=%#v, want successful selected real Provider route", status, routes.Routes)
	}

	_, invalidStatus := processJSON[errorResponse](t, first, http.MethodPost, "/workflows/"+created.ID+"/approve", map[string]any{
		"note": "invalid local trial approval",
	}, "Bearer invalid")
	if invalidStatus != http.StatusUnauthorized {
		t.Fatalf("invalid approval status=%d, want 401", invalidStatus)
	}
	unchanged, status := processJSON[workflowResponse](t, first, http.MethodGet, "/workflows/"+created.ID, nil, "")
	if status != http.StatusOK {
		t.Fatalf("workflow after invalid approval status=%d workflow=%#v, want unchanged approval wait", status, unchanged)
	}
	assertRealProviderTrialWorkflowEqual(t, "invalid approval", created, unchanged)
	unchangedRoutes, status := processJSON[llmRoutesResponse](t, first, http.MethodGet, "/workflows/"+created.ID+"/llm-routes", nil, "")
	if status != http.StatusOK || !realProviderTrialRoutesMatch(unchangedRoutes, created, false) {
		t.Fatalf("routes after invalid approval status=%d routes=%#v, want no human acceptance outcome", status, unchangedRoutes.Routes)
	}
	stopHostProcess(t, first)

	second := startHostProcessWithEnvAndRedactions(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment, redactions)
	second.client.Timeout = 30 * time.Second
	persisted, status := processJSON[workflowResponse](t, second, http.MethodGet, "/workflows/"+created.ID, nil, "")
	if status != http.StatusOK {
		t.Fatalf("workflow after first restart status=%d workflow=%#v, want persisted approval boundary", status, persisted)
	}
	assertRealProviderTrialWorkflowEqual(t, "first restart", created, persisted)
	completed, status := processJSON[workflowResponse](t, second, http.MethodPost, "/workflows/"+created.ID+"/approve", map[string]any{
		"note": "real Provider local trial accepted",
	}, "Bearer "+token)
	wantAuditRef := "audit:" + created.ID + ":approval"
	if status != http.StatusOK || completed.Status != string(workflowkit.StatusSucceeded) || completed.InputRef != created.InputRef || completed.OutputRef == "" || completed.OutputRef == created.OutputRef || completed.AgentRunID != created.AgentRunID || completed.AuditRef != wantAuditRef || completed.ApprovalRef != created.ApprovalRef || !reflect.DeepEqual(completed.Completed, []string{"ingest", "agent_review", "finalize"}) {
		t.Fatalf("final approval status=%d workflow=%#v, want succeeded", status, completed)
	}
	stopHostProcess(t, second)

	third := startHostProcessWithEnvAndRedactions(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment, redactions)
	third.client.Timeout = 30 * time.Second
	persistedCompleted, status := processJSON[workflowResponse](t, third, http.MethodGet, "/workflows/"+completed.ID, nil, "")
	if status != http.StatusOK {
		t.Fatalf("workflow after second restart status=%d workflow=%#v", status, persistedCompleted)
	}
	assertRealProviderTrialWorkflowEqual(t, "second restart", completed, persistedCompleted)
	persistedRun, status := processJSON[agentRunResponse](t, third, http.MethodGet, "/agent-runs/"+created.AgentRunID, nil, "")
	if status != http.StatusOK || !realProviderTrialRunMatches(persistedRun, created, created.OutputRef) {
		t.Fatalf("agent run after second restart status=%d run=%#v", status, persistedRun)
	}
	persistedRoutes, status := processJSON[llmRoutesResponse](t, third, http.MethodGet, "/workflows/"+completed.ID+"/llm-routes", nil, "")
	if status != http.StatusOK || !realProviderTrialRoutesMatch(persistedRoutes, completed, true) {
		t.Fatalf("routes after second restart status=%d routes=%#v", status, persistedRoutes.Routes)
	}
	events, status := processJSON[workflowEventsResponse](t, third, http.MethodGet, "/workflows/"+completed.ID+"/events", nil, "")
	if status != http.StatusOK || !realProviderTrialEventsMatch(events, created, completed) {
		t.Fatalf("events after second restart status=%d events=%#v", status, events)
	}
	stopHostProcess(t, third)

	for _, item := range []struct {
		label string
		value string
	}{
		{label: "Provider API key", value: providerConfig.APIKey},
		{label: "Provider endpoint", value: providerConfig.BaseURL},
		{label: "normalized Provider endpoint", value: strings.TrimRight(providerConfig.BaseURL, "/")},
		{label: "OIDC bearer token", value: token},
	} {
		if first.output.ContainsSensitive(item.value) || second.output.ContainsSensitive(item.value) || third.output.ContainsSensitive(item.value) {
			t.Fatalf("host process output leaked %s", item.label)
		}
	}
	cleanupKeychain()
}

func assertRealProviderTrialWorkflowEqual(t *testing.T, phase string, expected, actual workflowResponse) {
	t.Helper()
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("workflow after %s = %#v, want %#v", phase, actual, expected)
	}
}

func realProviderTrialRunMatches(run agentRunResponse, workflow workflowResponse, contentRef string) bool {
	return run.RunID == workflow.AgentRunID &&
		run.WorkflowID == workflow.ID &&
		run.TaskID == workflow.ID &&
		run.Status == string(runkit.StatusSucceeded) &&
		run.Summary.Status == runkit.StatusSucceeded &&
		run.Summary.ContentRef == contentRef &&
		run.Summary.AbortReason == "" &&
		run.Summary.LLMCalls == 1 &&
		run.Summary.ToolCalls == 0 &&
		len(run.Summary.UsedTools) == 0
}

func realProviderTrialRoutesMatch(response llmRoutesResponse, workflow workflowResponse, wantHumanAccepted bool) bool {
	if response.WorkflowID != workflow.ID || len(response.Routes) != 1 {
		return false
	}
	route := response.Routes[0]
	wantRouteID := fmt.Sprintf("route:%s:%s:1", workflow.ID, workflow.AgentRunID)
	if route.RouteID != wantRouteID || route.TaskID != workflow.ID || route.Attempt != 1 || route.AccountAlias != realProviderTrialAccountAlias || !route.Selected || route.ModelAlias != realProviderTrialModelAlias || route.Provider != "openai_compatible" || route.Outcome == nil || !route.Outcome.Success {
		return false
	}
	if !wantHumanAccepted {
		return route.Outcome.BusinessOutcome == "" && route.Outcome.SuccessSignal == ""
	}
	return route.Outcome.BusinessOutcome == string(llmkit.BusinessOutcomeSuccess) && route.Outcome.SuccessSignal == string(llmkit.SuccessSignalHumanAccepted)
}

func realProviderTrialEventsMatch(events workflowEventsResponse, created, completed workflowResponse) bool {
	if events.WorkflowID != completed.ID || events.Status != string(workflowkit.StatusSucceeded) || events.RunMode != completed.RunMode || events.CurrentStep != "finalize" || !reflect.DeepEqual(events.Completed, completed.Completed) || len(events.Events) != 3 {
		return false
	}
	ingest := events.Events[0]
	agent := events.Events[1]
	finalize := events.Events[2]
	return ingest.Type == "step" && ingest.Name == "ingest" && ingest.Status == string(workflowkit.StatusSucceeded) && ingest.OutputRef == created.InputRef &&
		agent.Type == "step" && agent.Name == "agent_review" && agent.Status == string(workflowkit.StatusWaitingApproval) && agent.OutputRef == created.OutputRef && agent.AgentRunID == created.AgentRunID && agent.ApprovalRef == created.ApprovalRef && agent.WaitingReason == created.WaitingReason &&
		finalize.Type == "step" && finalize.Name == "finalize" && finalize.Status == string(workflowkit.StatusSucceeded) && finalize.OutputRef == completed.OutputRef && finalize.AuditRef == completed.AuditRef
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
  - alias: %s
    provider: openai_compatible
    base_url: %q
    api_key_env: OPENAI_COMPAT_API_KEY
    max_concurrency: 1
models:
  - alias: %s
    model: %q
    provider: openai_compatible
    account_alias: %s
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
`, realProviderTrialAccountAlias, strings.TrimRight(config.BaseURL, "/"), realProviderTrialModelAlias, config.Model, realProviderTrialAccountAlias)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatalf("write real Provider trial llmkit config: %v", err)
	}
}
