package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eruca/artifactkit"
	"github.com/eruca/llmkit/llmkit"
	"github.com/eruca/runkit"
	"github.com/eruca/workflowkit"
)

func TestHostAPIWorkflowApprovalRunAndModelEndpoints(t *testing.T) {
	server, err := NewServer(Config{LLMKitHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	handler := server.Handler()

	create := doJSON[workflowResponse](t, handler, http.MethodPost, "/workflows", map[string]string{
		"id":    "wf-api-1",
		"input": "Review the draft through the host API.",
	})
	if create.ID != "wf-api-1" || create.Status != string(workflowkit.StatusWaitingApproval) {
		t.Fatalf("create response = %+v, want waiting workflow", create)
	}
	if create.RunMode != string(RunModeSync) {
		t.Fatalf("create run mode = %q, want sync", create.RunMode)
	}
	if create.InputRef == "" || create.OutputRef == "" || create.AgentRunID == "" || create.ApprovalRef == "" {
		t.Fatalf("create response refs should be populated: %+v", create)
	}

	loaded := doJSON[workflowResponse](t, handler, http.MethodGet, "/workflows/wf-api-1", nil)
	if loaded.ID != create.ID || loaded.AgentRunID != create.AgentRunID {
		t.Fatalf("loaded workflow = %+v, want created workflow %+v", loaded, create)
	}

	run := doJSON[agentRunResponse](t, handler, http.MethodGet, "/agent-runs/"+create.AgentRunID, nil)
	if run.RunID != create.AgentRunID || run.WorkflowID != create.ID {
		t.Fatalf("agent run = %+v, want correlated run", run)
	}
	if run.Summary.ContentRef != create.OutputRef {
		t.Fatalf("agent run summary = %+v, want content ref %q", run.Summary, create.OutputRef)
	}
	if len(run.Events) == 0 {
		t.Fatalf("agent run events should be returned")
	}

	models := doJSON[modelsResponse](t, handler, http.MethodGet, "/llmkit/models", nil)
	if len(models.Models) != 2 {
		t.Fatalf("models len = %d, want 2: %+v", len(models.Models), models)
	}
	if !hasModel(models.Models, "local-free") || !hasModel(models.Models, "cloud-advanced") {
		t.Fatalf("models = %+v, want local-free and cloud-advanced", models.Models)
	}
	if len(models.Health.Entries) == 0 {
		t.Fatalf("health snapshot should include selected provider after workflow run: %+v", models.Health)
	}

	approved := doJSON[workflowResponse](t, handler, http.MethodPost, "/workflows/wf-api-1/approve", map[string]string{
		"approved_by": "operator-api",
		"note":        "accepted",
	})
	if approved.Status != string(workflowkit.StatusSucceeded) {
		t.Fatalf("approved response = %+v, want succeeded", approved)
	}
	if approved.OutputRef == "" || approved.AuditRef == "" {
		t.Fatalf("approved refs should be populated: %+v", approved)
	}
}

func TestHostAPIDurableRuntimeResumesWorkflowAfterReopen(t *testing.T) {
	runtimeHome := t.TempDir()
	server, err := NewServer(Config{RuntimeHome: runtimeHome})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	create := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{
		"id":    "wf-durable-1",
		"input": "Review the durable draft through the host API.",
	})
	if create.Status != string(workflowkit.StatusWaitingApproval) {
		t.Fatalf("create status = %q, want waiting_approval", create.Status)
	}

	reopened, err := NewServer(Config{RuntimeHome: runtimeHome})
	if err != nil {
		t.Fatalf("reopen NewServer returned error: %v", err)
	}
	loaded := doJSON[workflowResponse](t, reopened.Handler(), http.MethodGet, "/workflows/wf-durable-1", nil)
	if loaded.ID != create.ID || loaded.AgentRunID != create.AgentRunID || loaded.OutputRef != create.OutputRef {
		t.Fatalf("loaded after reopen = %+v, want created %+v", loaded, create)
	}

	run := doJSON[agentRunResponse](t, reopened.Handler(), http.MethodGet, "/agent-runs/"+create.AgentRunID, nil)
	if run.RunID != create.AgentRunID || run.Summary.ContentRef != create.OutputRef || len(run.Events) == 0 {
		t.Fatalf("agent run after reopen = %+v, want durable run with events", run)
	}

	approved := doJSON[workflowResponse](t, reopened.Handler(), http.MethodPost, "/workflows/wf-durable-1/approve", map[string]string{
		"approved_by": "operator-durable",
		"note":        "resume after reopen",
	})
	if approved.Status != string(workflowkit.StatusSucceeded) {
		t.Fatalf("approved after reopen = %+v, want succeeded", approved)
	}
	if approved.OutputRef == create.OutputRef {
		t.Fatalf("approved output ref = %q, should point to final artifact", approved.OutputRef)
	}

	if !fileExists(filepath.Join(runtimeHome, "workflow.db")) {
		t.Fatalf("workflow db was not created under runtime home")
	}
	if !fileExists(filepath.Join(runtimeHome, "agent-runs.db")) {
		t.Fatalf("agent run db was not created under runtime home")
	}
	if !fileExists(filepath.Join(runtimeHome, ".llmkit", "route-events.jsonl")) {
		t.Fatalf("llmkit route audit was not created under runtime home")
	}
}

func TestHostAPIReturnsWorkflowLLMRouteAudit(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	create := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{
		"id":    "wf-routes-1",
		"input": "Review routing visibility through the host API.",
	})

	routes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-routes-1/llm-routes", nil)
	if routes.WorkflowID != create.ID {
		t.Fatalf("workflow id = %q, want %q", routes.WorkflowID, create.ID)
	}
	if len(routes.Routes) != 1 {
		t.Fatalf("routes len = %d, want 1: %+v", len(routes.Routes), routes.Routes)
	}
	got := routes.Routes[0]
	if got.RouteID != "route:wf-routes-1:1" || got.ModelAlias != "local-free" || got.Provider != "local" || got.AccountAlias != "local-dev" {
		t.Fatalf("route audit = %+v, want selected local-free route", got)
	}
	if got.Score == 0 || got.ScoreBreakdown["price"] == 0 || len(got.CandidateModelAliases) != 2 {
		t.Fatalf("route explainability fields missing: %+v", got)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("route candidates len = %d, want 2: %+v", len(got.Candidates), got.Candidates)
	}
	localScore := routeCandidate(t, got.Candidates, "local-free")
	if !localScore.Available || localScore.Score == 0 || localScore.ScoreBreakdown["price"] == 0 {
		t.Fatalf("local candidate explanation missing: %+v", localScore)
	}
	cloudScore := routeCandidate(t, got.Candidates, "cloud-advanced")
	if !cloudScore.Available || cloudScore.Score == 0 || cloudScore.Reason == "" {
		t.Fatalf("cloud candidate explanation missing: %+v", cloudScore)
	}
	if got.Outcome == nil || !got.Outcome.Success || got.Outcome.InputTokens != 5 || got.Outcome.OutputTokens != 7 {
		t.Fatalf("route outcome = %+v, want successful token outcome", got.Outcome)
	}

	approved := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows/wf-routes-1/approve", map[string]string{
		"approved_by": "operator-routes",
		"note":        "accepted",
	})
	if approved.Status != string(workflowkit.StatusSucceeded) {
		t.Fatalf("approved response = %+v, want succeeded", approved)
	}
	approvedRoutes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-routes-1/llm-routes", nil)
	approvedOutcome := selectedRoute(t, approvedRoutes).Outcome
	if approvedOutcome == nil || approvedOutcome.BusinessOutcome != string(llmkit.BusinessOutcomeSuccess) || approvedOutcome.SuccessSignal != string(llmkit.SuccessSignalHumanAccepted) {
		t.Fatalf("approved route outcome = %+v, want human accepted business outcome", approvedOutcome)
	}
}

func TestHostAPIRouteOutcomeResponseIncludesErrorClass(t *testing.T) {
	response := llmRouteToResponse(llmkit.RouteAuditRecord{
		Route: llmkit.RouteTrace{
			RouteID:      "route-error-class",
			TaskID:       "task-error-class",
			Attempt:      1,
			AccountAlias: "local-dev",
			ModelAlias:   "local-free",
			Provider:     "local",
			Selected:     true,
		},
		Outcome: &llmkit.TaskOutcome{
			RouteID:    "route-error-class",
			TaskID:     "task-error-class",
			Attempt:    1,
			ModelAlias: "local-free",
			Provider:   "local",
			Success:    false,
			ErrorCode:  "provider_error",
			ErrorClass: llmkit.ErrorClassTimeout,
		},
	})
	if response.Outcome == nil {
		t.Fatalf("response outcome is nil: %+v", response)
	}
	if response.Outcome.ErrorClass != string(llmkit.ErrorClassTimeout) {
		t.Fatalf("response outcome = %+v, want timeout error_class", response.Outcome)
	}
}

func TestHostAPIAgentStepFailsWhenOutputArtifactCannotBeWritten(t *testing.T) {
	server := &Server{
		artifacts: failingArtifactStore{err: fmt.Errorf("artifact store unavailable")},
		runs:      runkit.NewMemoryStore(),
		health:    llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{}),
		llmHome:   t.TempDir(),
		models:    defaultCandidates(),
		providers: defaultProviders(),
	}

	result, err := server.agentStep().Run(context.Background(), workflowkit.WorkflowRun{
		ID: "wf-strict-artifact",
		Metadata: map[string]any{
			"task_profile": defaultHostTaskProfile(),
		},
	})

	if err == nil {
		t.Fatalf("agent step returned nil error, result=%+v", result)
	}
	if result.Status != workflowkit.StatusFailed {
		t.Fatalf("agent step status = %q, want failed", result.Status)
	}
	if !strings.Contains(err.Error(), "artifact store unavailable") {
		t.Fatalf("agent step error = %v, want artifact failure", err)
	}
}

func TestHostAPIAgentStepFailsWhenTerminalSummaryCannotBeWritten(t *testing.T) {
	server := &Server{
		artifacts: artifactkit.NewMemoryStore(),
		runs:      failingRunStore{err: fmt.Errorf("run store unavailable")},
		health:    llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{}),
		llmHome:   t.TempDir(),
		models:    defaultCandidates(),
		providers: defaultProviders(),
	}

	result, err := server.agentStep().Run(context.Background(), workflowkit.WorkflowRun{
		ID: "wf-strict-run",
		Metadata: map[string]any{
			"task_profile": defaultHostTaskProfile(),
		},
	})

	if err == nil {
		t.Fatalf("agent step returned nil error, result=%+v", result)
	}
	if result.Status != workflowkit.StatusFailed {
		t.Fatalf("agent step status = %q, want failed", result.Status)
	}
	if !strings.Contains(err.Error(), "run store unavailable") {
		t.Fatalf("agent step error = %v, want run store failure", err)
	}
}

func TestHostAPIRoutesLLMByRequestTaskProfile(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-profile-simple",
		"input": "Format a short note.",
		"task_profile": map[string]any{
			"task_type":    "format_note",
			"complexity":   "simple",
			"failure_cost": "low",
			"privacy":      "local_preferred",
		},
	})
	simpleRoutes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-profile-simple/llm-routes", nil)
	if got := selectedModelAlias(t, simpleRoutes); got != "local-free" {
		t.Fatalf("simple profile selected %q, want local-free; routes=%+v", got, simpleRoutes.Routes)
	}

	doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-profile-hard",
		"input": "Review a long, high-risk clinical policy decision.",
		"task_profile": map[string]any{
			"task_type":       "clinical_policy_review",
			"complexity":      "hard",
			"failure_cost":    "high",
			"privacy":         "cloud_allowed",
			"needs_reasoning": true,
		},
	})
	hardRoutes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-profile-hard/llm-routes", nil)
	if got := selectedModelAlias(t, hardRoutes); got != "cloud-advanced" {
		t.Fatalf("hard profile selected %q, want cloud-advanced; routes=%+v", got, hardRoutes.Routes)
	}
}

func TestHostAPIRoutesLLMByTaskProfilePreset(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":                  "wf-preset-simple",
		"input":               "Format a short note.",
		"task_profile_preset": "simple_local",
	})
	simpleRoutes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-preset-simple/llm-routes", nil)
	if got := selectedModelAlias(t, simpleRoutes); got != "local-free" {
		t.Fatalf("simple_local preset selected %q, want local-free; routes=%+v", got, simpleRoutes.Routes)
	}
	if got := selectedTaskType(t, simpleRoutes); got != "simple_local" {
		t.Fatalf("simple_local task type = %q, want simple_local", got)
	}

	doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":                  "wf-preset-high-success",
		"input":               "Review a high-risk policy.",
		"task_profile_preset": "high_success",
	})
	highRoutes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-preset-high-success/llm-routes", nil)
	if got := selectedModelAlias(t, highRoutes); got != "cloud-advanced" {
		t.Fatalf("high_success preset selected %q, want cloud-advanced; routes=%+v", got, highRoutes.Routes)
	}
	if got := selectedTaskType(t, highRoutes); got != "high_success" {
		t.Fatalf("high_success task type = %q, want high_success", got)
	}
	highRoute := selectedRoute(t, highRoutes)
	if highRoute.TaskProfile == nil {
		t.Fatalf("high_success route profile is nil: %+v", highRoute)
	}
	if highRoute.TaskProfile.TaskType != "high_success" ||
		highRoute.TaskProfile.Complexity != "hard" ||
		highRoute.TaskProfile.FailureCost != "high" ||
		highRoute.TaskProfile.Privacy != "cloud_allowed" ||
		!highRoute.TaskProfile.NeedsReasoning {
		t.Fatalf("high_success route profile = %+v, want effective high_success profile", highRoute.TaskProfile)
	}
}

func TestHostAPIRoutesLLMUsingHistoricalOutcomes(t *testing.T) {
	llmHome := t.TempDir()
	recorder, err := llmkit.NewJSONLRecorder(llmHome)
	if err != nil {
		t.Fatalf("NewJSONLRecorder returned error: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := recorder.RecordOutcome(context.Background(), llmkit.TaskOutcome{
			RouteID:       fmt.Sprintf("route-history-%d", i),
			TaskID:        fmt.Sprintf("history-%d", i),
			Attempt:       1,
			RecordedAt:    time.Date(2026, 5, 6, 8, i, 0, 0, time.UTC),
			TaskType:      "simple_local",
			AccountAlias:  "local-dev",
			ModelAlias:    "local-free",
			Provider:      "local",
			Success:       false,
			ErrorCode:     "timeout",
			LatencyMillis: 3000,
		}); err != nil {
			t.Fatalf("RecordOutcome returned error: %v", err)
		}
	}

	server, err := NewServer(Config{RuntimeHome: t.TempDir(), LLMKitHome: llmHome})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":                  "wf-history-routing",
		"input":               "Format a short note, but avoid recently failing models.",
		"task_profile_preset": "simple_local",
	})
	routes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-history-routing/llm-routes", nil)
	if got := selectedModelAlias(t, routes); got != "cloud-advanced" {
		t.Fatalf("history-aware route selected %q, want cloud-advanced; routes=%+v", got, routes.Routes)
	}
	selected := selectedRoute(t, routes)
	if !containsString(selected.CandidateModelAliases, "local-free") {
		t.Fatalf("candidate aliases = %+v, want local-free included", selected.CandidateModelAliases)
	}
	stats, err := llmkit.LoadModelStats(llmHome)
	if err != nil {
		t.Fatalf("LoadModelStats returned error: %v", err)
	}
	local := stats.Models["simple_local|local-dev|local-free|local"]
	if local.Failures != 10 || local.FailureRate != 1 {
		t.Fatalf("local-free stats = %+v, want 10 historical failures", local)
	}
	models := doJSON[modelsResponse](t, server.Handler(), http.MethodGet, "/llmkit/models", nil)
	localModelStats := modelStatsFor(t, models.Stats, "simple_local", "local-free")
	if localModelStats.Failures != 10 || localModelStats.FailureRate != 1 {
		t.Fatalf("models stats = %+v, want local-free historical failures", localModelStats)
	}
}

func TestHostAPITaskProfilePresetAllowsOverrides(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":                  "wf-preset-override",
		"input":               "Review a high-risk policy locally.",
		"task_profile_preset": "high_success",
		"task_profile": map[string]any{
			"complexity": "simple",
			"privacy":    "local_only",
		},
	})
	routes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-preset-override/llm-routes", nil)
	if got := selectedModelAlias(t, routes); got != "local-free" {
		t.Fatalf("override selected %q, want local-free; routes=%+v", got, routes.Routes)
	}
}

func TestHostAPITaskProfilePatchKeepsPresetBooleansWhenOmitted(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":                  "wf-preset-patch-keep-bool",
		"input":               "Review a high-risk policy.",
		"task_profile_preset": "high_success",
		"task_profile": map[string]any{
			"task_type": "policy_review_custom",
		},
	})
	routes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-preset-patch-keep-bool/llm-routes", nil)
	route := selectedRoute(t, routes)
	if route.TaskProfile == nil {
		t.Fatalf("route profile is nil: %+v", route)
	}
	if !route.TaskProfile.NeedsReasoning {
		t.Fatalf("route profile = %+v, want omitted needs_reasoning to inherit high_success preset", route.TaskProfile)
	}
}

func TestHostAPITaskProfilePatchCanExplicitlyDisablePresetBoolean(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":                  "wf-preset-patch-disable-bool",
		"input":               "Review a high-risk policy without reasoning.",
		"task_profile_preset": "high_success",
		"task_profile": map[string]any{
			"needs_reasoning": false,
		},
	})
	routes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-preset-patch-disable-bool/llm-routes", nil)
	route := selectedRoute(t, routes)
	if route.TaskProfile == nil {
		t.Fatalf("route profile is nil: %+v", route)
	}
	if route.TaskProfile.NeedsReasoning {
		t.Fatalf("route profile = %+v, want explicit needs_reasoning=false to override preset", route.TaskProfile)
	}
}

func TestHostAPIRejectsEmptyTaskProfilePatchStrings(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/workflows", bytes.NewBufferString(`{
		"id": "wf-empty-profile-string",
		"input": "Review this.",
		"task_profile": {
			"complexity": ""
		}
	}`))
	req.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "invalid_task_profile") || !strings.Contains(resp.Body.String(), "complexity") {
		t.Fatalf("body = %s, want invalid_task_profile complexity error", resp.Body.String())
	}
}

func TestHostAPIRejectsInvalidTaskProfilePresetCombination(t *testing.T) {
	server, err := NewServer(Config{LLMKitHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/workflows", bytes.NewBufferString(`{
		"id": "wf-invalid-profile",
		"input": "Review this locally with advanced reasoning.",
		"task_profile_preset": "local_only",
		"task_profile": {
			"complexity": "hard",
			"needs_reasoning": true
		}
	}`))
	req.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "invalid_task_profile") || !strings.Contains(resp.Body.String(), "local_only") {
		t.Fatalf("body = %s, want invalid_task_profile local_only error", resp.Body.String())
	}
}

func TestHostAPIReturnsJSONErrors(t *testing.T) {
	server, err := NewServer(Config{LLMKitHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/workflows/missing", nil)
	server.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "not_found") {
		t.Fatalf("body = %s, want not_found error", resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/workflows", bytes.NewBufferString("{"))
	server.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
}

func TestHostAPIRunModeSyncAndQueuedSemantics(t *testing.T) {
	server, err := NewServer(Config{LLMKitHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	syncRun := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{
		"id":       "wf-sync",
		"input":    "Review the sync draft.",
		"run_mode": "sync",
	})
	if syncRun.RunMode != string(RunModeSync) || syncRun.Status != string(workflowkit.StatusWaitingApproval) {
		t.Fatalf("sync run response = %+v, want sync waiting workflow", syncRun)
	}

	queued := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{
		"id":       "wf-queued",
		"input":    "Review later.",
		"run_mode": "queued",
	})
	if queued.RunMode != string(RunModeQueued) || queued.Status != string(workflowkit.StatusPending) {
		t.Fatalf("queued response = %+v, want queued pending workflow", queued)
	}
	if queued.InputRef != "artifact:wf-queued:input" || queued.AgentRunID != "" || queued.OutputRef != "" {
		t.Fatalf("queued response refs = %+v, want only input ref before background run", queued)
	}

	loaded := waitForWorkflowStatus(t, server.Handler(), "wf-queued", workflowkit.StatusWaitingApproval)
	if loaded.AgentRunID == "" || loaded.OutputRef == "" || loaded.ApprovalRef == "" {
		t.Fatalf("queued loaded workflow = %+v, want agent refs after background run", loaded)
	}
	stored, err := server.workflows.Get(context.Background(), "wf-queued")
	if err != nil {
		t.Fatalf("workflow store Get returned error: %v", err)
	}
	if stored.LeaseOwner != "host-api-inprocess-worker" || stored.LeaseUntil.IsZero() {
		t.Fatalf("queued workflow lease = %+v, want in-process worker lease", stored)
	}
	routes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-queued/llm-routes", nil)
	if got := selectedModelAlias(t, routes); got != "local-free" {
		t.Fatalf("queued selected model = %q, want local-free; routes=%+v", got, routes.Routes)
	}

	approved := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows/wf-queued/approve", map[string]string{
		"approved_by": "operator-queued",
		"note":        "accepted queued",
	})
	if approved.Status != string(workflowkit.StatusSucceeded) {
		t.Fatalf("queued approval = %+v, want succeeded", approved)
	}
}

func TestHostAPILoadsLLMKitConfigModelsAndProviders(t *testing.T) {
	var gotAuthorization string
	var gotModel string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode provider request: %v", err)
		}
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"configured provider response"}}],"usage":{"prompt_tokens":11,"completion_tokens":13}}`))
	}))
	defer provider.Close()

	llmHome := t.TempDir()
	t.Setenv("HOST_API_CONFIG_KEY", "secret-from-env")
	writeLLMKitConfig(t, llmHome, fmt.Sprintf(`
accounts:
  - alias: configured-account
    provider: openai_compatible
    base_url: %s/v1
    api_key_env: HOST_API_CONFIG_KEY
models:
  - alias: configured-advanced
    model: configured-model-name
    provider: openai_compatible
    account_alias: configured-account
    capability_level: advanced
    context_window_class: long
    price_class: high
    latency_class: normal
`, provider.URL))

	server, err := NewServer(Config{RuntimeHome: t.TempDir(), LLMKitHome: llmHome})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	models := doJSON[modelsResponse](t, server.Handler(), http.MethodGet, "/llmkit/models", nil)
	if len(models.Models) != 1 || !hasModel(models.Models, "configured-advanced") {
		t.Fatalf("models = %+v, want configured-advanced from config", models.Models)
	}

	create := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":                  "wf-configured-provider",
		"input":               "Use configured provider.",
		"task_profile_preset": "high_success",
	})
	run := doJSON[agentRunResponse](t, server.Handler(), http.MethodGet, "/agent-runs/"+create.AgentRunID, nil)
	if run.Summary.InputTokens != 11 || run.Summary.OutputTokens != 13 {
		t.Fatalf("run summary usage = %+v, want configured provider usage", run.Summary)
	}
	routes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-configured-provider/llm-routes", nil)
	if got := selectedModelAlias(t, routes); got != "configured-advanced" {
		t.Fatalf("configured route selected %q, want configured-advanced; routes=%+v", got, routes.Routes)
	}
	if gotModel != "configured-model-name" {
		t.Fatalf("provider model = %q, want configured-model-name", gotModel)
	}
	if gotAuthorization != "Bearer secret-from-env" {
		t.Fatalf("provider authorization = %q, want bearer secret", gotAuthorization)
	}
}

func TestHostAPIRejectsConfiguredMissingAPIKeyEnv(t *testing.T) {
	llmHome := t.TempDir()
	writeLLMKitConfig(t, llmHome, `
accounts:
  - alias: configured-account
    provider: openai_compatible
    base_url: http://127.0.0.1:65535/v1
    api_key_env: HOST_API_MISSING_KEY
models:
  - alias: configured-advanced
    model: configured-model-name
    provider: openai_compatible
    account_alias: configured-account
    capability_level: advanced
    context_window_class: long
    price_class: high
    latency_class: normal
`)

	_, err := NewServer(Config{RuntimeHome: t.TempDir(), LLMKitHome: llmHome})
	if err == nil {
		t.Fatal("NewServer error = nil, want missing API key env error")
	}
	if !strings.Contains(err.Error(), "HOST_API_MISSING_KEY") {
		t.Fatalf("NewServer error = %v, want missing env name", err)
	}
}

func doJSON[T any](t *testing.T, handler http.Handler, method, path string, body any) T {
	t.Helper()
	var payload *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		payload = bytes.NewReader(raw)
	} else {
		payload = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code < 200 || resp.Code >= 300 {
		t.Fatalf("%s %s status = %d; body=%s", method, path, resp.Code, resp.Body.String())
	}
	var out T
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %s %s: %v; body=%s", method, path, err, resp.Body.String())
	}
	return out
}

func hasModel(models []modelResponse, alias string) bool {
	for _, model := range models {
		if model.Alias == alias {
			return true
		}
	}
	return false
}

func modelStatsFor(t *testing.T, stats []modelStatsResponse, taskType, modelAlias string) modelStatsResponse {
	t.Helper()
	for _, stat := range stats {
		if stat.TaskType == taskType && stat.ModelAlias == modelAlias {
			return stat
		}
	}
	t.Fatalf("model stats for task=%q model=%q not found: %+v", taskType, modelAlias, stats)
	return modelStatsResponse{}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type failingArtifactStore struct {
	err error
}

func (s failingArtifactStore) Put(context.Context, artifactkit.Artifact) error {
	return s.err
}

func (s failingArtifactStore) Get(context.Context, string) (artifactkit.Artifact, error) {
	return artifactkit.Artifact{}, s.err
}

type failingRunStore struct {
	err error
}

func (s failingRunStore) Create(context.Context, runkit.RunRecord) error {
	return s.err
}

func (s failingRunStore) Get(context.Context, string) (runkit.RunRecord, error) {
	return runkit.RunRecord{}, runkit.ErrRunNotFound
}

func (s failingRunStore) AppendEvent(context.Context, runkit.RunEvent) error {
	return nil
}

func (s failingRunStore) Events(context.Context, string) ([]runkit.RunEvent, error) {
	return nil, s.err
}

func (s failingRunStore) Complete(context.Context, string, runkit.TerminalSummary) error {
	return s.err
}

func (s failingRunStore) FindByWorkflowID(context.Context, string) ([]runkit.RunRecord, error) {
	return nil, s.err
}

func writeLLMKitConfig(t *testing.T, home string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

func selectedModelAlias(t *testing.T, routes llmRoutesResponse) string {
	t.Helper()
	return selectedRoute(t, routes).ModelAlias
}

func selectedTaskType(t *testing.T, routes llmRoutesResponse) string {
	t.Helper()
	return selectedRoute(t, routes).TaskType
}

func waitForWorkflowStatus(t *testing.T, handler http.Handler, id string, want workflowkit.Status) workflowResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last workflowResponse
	for time.Now().Before(deadline) {
		last = doJSON[workflowResponse](t, handler, http.MethodGet, "/workflows/"+id, nil)
		if last.Status == string(want) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("workflow %s status = %q, want %q; last=%+v", id, last.Status, want, last)
	return workflowResponse{}
}

func selectedRoute(t *testing.T, routes llmRoutesResponse) llmRouteResponse {
	t.Helper()
	for _, route := range routes.Routes {
		if route.Selected {
			return route
		}
	}
	t.Fatalf("no selected route found: %+v", routes.Routes)
	return llmRouteResponse{}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func routeCandidate(t *testing.T, candidates []llmRouteCandidateResponse, alias string) llmRouteCandidateResponse {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.Alias == alias {
			return candidate
		}
	}
	t.Fatalf("route candidate %q not found: %+v", alias, candidates)
	return llmRouteCandidateResponse{}
}
