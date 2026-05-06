package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/workflows", bytes.NewBufferString(`{
		"id": "wf-queued",
		"input": "Review later.",
		"run_mode": "queued"
	}`))
	req.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("queued status = %d, want 400; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "unsupported_run_mode") {
		t.Fatalf("queued body = %s, want unsupported_run_mode", resp.Body.String())
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
