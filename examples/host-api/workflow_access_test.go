package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/workflowkit"
)

func TestCreateWorkflowRejectsExistingIDWithoutTouchingIt(t *testing.T) {
	server, err := NewServer(Config{LLMKitHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	handler := server.Handler()

	first := doJSON[workflowResponse](t, handler, http.MethodPost, "/workflows", map[string]any{"id": "wf-dup", "input": "original"})
	if first.Status != string(workflowkit.StatusWaitingApproval) {
		t.Fatalf("first create status = %q, want waiting_approval", first.Status)
	}

	for _, mode := range []string{"sync", "queued"} {
		response := workflowRequestForTest(t, handler, http.MethodPost, "/workflows", map[string]any{"id": "wf-dup", "input": "evil", "run_mode": mode})
		if response.Code != http.StatusConflict {
			t.Fatalf("%s duplicate status = %d, body=%s; want 409", mode, response.Code, response.Body.String())
		}
	}
	stored, err := server.workflows.Get(t.Context(), "wf-dup")
	if err != nil || stored.Status != workflowkit.StatusWaitingApproval || len(stored.CompletedSteps) == 0 {
		t.Fatalf("stored = %+v, %v; want the original workflow untouched", stored, err)
	}
	input, err := server.artifacts.Get(t.Context(), "artifact:wf-dup:input")
	if err != nil || string(input.Content) != "original" {
		t.Fatalf("input artifact = %q, %v; want the original input", input.Content, err)
	}
}

func TestFinalApprovalCannotResolvePendingToolApproval(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-tools"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	created := createToolApprovalWorkflow(t, server, "wf-skip-tool-approval")

	response := workflowRequestForTest(t, server.Handler(), http.MethodPost, "/workflows/"+created.ID+"/approve", map[string]string{"note": "skip the tool"})
	if response.Code != http.StatusConflict {
		t.Fatalf("approve status = %d, body=%s; want 409", response.Code, response.Body.String())
	}
	stored, err := server.workflows.Get(t.Context(), created.ID)
	if err != nil || stored.Status != workflowkit.StatusWaitingApproval || stored.ApprovalRef != "agent-approval:"+created.AgentApproval.CheckpointID {
		t.Fatalf("stored = %+v, %v; want still waiting on the tool approval", stored, err)
	}
}

func TestMemoryWorkflowRoutesRequireAccessToStoredScope(t *testing.T) {
	config := validCompleteMemoryRuntimeConfig(t)
	authorizer := &recordingMemoryAuthorizer{identity: memoryIdentity{TenantID: "tenant-a", Subject: "user-a"}}
	config.Authorizer = authorizer
	server, err := NewServer(Config{RuntimeHome: t.TempDir(), Memory: config})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	handler := server.Handler()
	created := createMemoryWorkflowRequest(t, handler, map[string]any{
		"id": "wf-tenant-a", "input": "review", "run_mode": "queued", "project_id": "project-a", "memory_write_intent": true,
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, body=%s", created.Code, created.Body.String())
	}

	request := func(method, path, authorization string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	routes := []struct{ method, path string }{
		{http.MethodGet, "/workflows?project_id=project-a"},
		{http.MethodGet, "/workflows/wf-tenant-a"},
		{http.MethodGet, "/workflows/wf-tenant-a/events"},
		{http.MethodGet, "/workflows/wf-tenant-a/llm-routes"},
		{http.MethodPost, "/workflows/wf-tenant-a/requeue"},
	}
	for _, route := range routes {
		if response := request(route.method, route.path, ""); response.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without credentials = %d, want 401", route.method, route.path, response.Code)
		}
	}

	authorizer.identity = memoryIdentity{TenantID: "tenant-b", Subject: "user-b"}
	for _, route := range routes[1:] {
		if response := request(route.method, route.path, "Bearer tenant-b"); response.Code != http.StatusNotFound {
			t.Errorf("%s %s as another tenant = %d, want 404", route.method, route.path, response.Code)
		}
	}
	listed := request(http.MethodGet, "/workflows?project_id=project-a", "Bearer tenant-b")
	if listed.Code != http.StatusOK || containsWorkflowID(t, listed, "wf-tenant-a") {
		t.Fatalf("list as another tenant = %d %s; want no tenant-a workflows", listed.Code, listed.Body.String())
	}

	authorizer.identity = memoryIdentity{TenantID: "tenant-a", Subject: "user-other"}
	if response := request(http.MethodPost, "/workflows/wf-tenant-a/requeue", "Bearer tenant-a-other"); response.Code != http.StatusForbidden {
		t.Fatalf("requeue of a write-intent workflow by another user = %d, want 403", response.Code)
	}
	if response := request(http.MethodGet, "/workflows/wf-tenant-a", "Bearer tenant-a-other"); response.Code != http.StatusOK {
		t.Fatalf("get within the tenant = %d, body=%s; want 200", response.Code, response.Body.String())
	}
	authorizer.identity = memoryIdentity{TenantID: "tenant-a", Subject: "user-a"}
	if listed := request(http.MethodGet, "/workflows?project_id=project-a", "Bearer tenant-a"); listed.Code != http.StatusOK || !containsWorkflowID(t, listed, "wf-tenant-a") {
		t.Fatalf("list as the owning tenant = %d %s; want the workflow", listed.Code, listed.Body.String())
	}
}

func containsWorkflowID(t *testing.T, response *httptest.ResponseRecorder, id string) bool {
	t.Helper()
	var list workflowListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v; body=%s", err, response.Body.String())
	}
	for _, workflow := range list.Workflows {
		if workflow.ID == id {
			return true
		}
	}
	return false
}

func TestAgentRunResponseUsesSnakeCaseFields(t *testing.T) {
	server, err := NewServer(Config{LLMKitHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	created := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{"id": "wf-agent-run-json", "input": "review"})
	response := workflowRequestForTest(t, server.Handler(), http.MethodGet, "/agent-runs/"+created.AgentRunID, nil)
	var raw struct {
		Summary map[string]any   `json:"summary"`
		Events  []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v; body=%s", err, response.Body.String())
	}
	if _, ok := raw.Summary["llm_calls"]; !ok {
		t.Fatalf("summary = %v, want snake_case fields", raw.Summary)
	}
	if len(raw.Events) == 0 || raw.Events[0]["type"] == nil || raw.Events[0]["recorded_at"] == nil {
		t.Fatalf("events = %v, want snake_case fields", raw.Events)
	}
}

// waitForCancelStep blocks until its context ends and reports that it did.
type waitForCancelStep struct{ cancelled chan struct{} }

func (waitForCancelStep) Name() string { return "wait_for_cancel" }

func (s waitForCancelStep) Run(ctx context.Context, _ workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	select {
	case <-ctx.Done():
		close(s.cancelled)
		return workflowkit.StepResult{}, ctx.Err()
	case <-time.After(10 * time.Second):
		return workflowkit.StepResult{}, errors.New("step was never cancelled")
	}
}

func TestQueuedWorkerStopsRunWhenLeaseIsLost(t *testing.T) {
	t.Setenv("HOST_API_QUEUED_LEASE_DURATION", "200ms")
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	step := waitForCancelStep{cancelled: make(chan struct{})}
	server.executor = workflowkit.NewExecutor(server.workflows, []workflowkit.Step{step})
	ctx, cancel := context.WithCancel(context.Background())
	stopWorkerOnCleanup(t, server, cancel)
	server.StartQueuedWorker(ctx)
	doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{
		"id": "wf-lease-lost", "input": "review", "run_mode": "queued",
	})
	waitForWorkflowLeasePresent(t, server.workflows, "wf-lease-lost")
	// Another host takes the workflow over.
	if _, err := server.workflows.Update(t.Context(), "wf-lease-lost", func(run workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		run.LeaseOwner = "host-api:other"
		run.LeaseUntil = time.Now().Add(time.Hour)
		return run, nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-step.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the run kept going after its lease was lost")
	}
}

func TestOversizedRequestBodyIsRejected(t *testing.T) {
	server, err := NewServer(Config{LLMKitHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	response := workflowRequestForTest(t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id": "wf-huge", "input": strings.Repeat("x", maxRequestBodyBytes+1),
	})
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body=%.200s; want 413", response.Code, response.Body.String())
	}
	if _, err := server.workflows.Get(t.Context(), "wf-huge"); err == nil {
		t.Fatal("oversized workflow was created")
	}
}

func TestInternalErrorsDoNotExposeDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeWorkflowStoreError(recorder, errors.New("open /srv/runtime/workflow.db: disk I/O error"))
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "/srv/runtime") {
		t.Fatalf("response = %d %s; want a 500 without the internal error", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	writeWorkflowStoreError(recorder, workflowkit.InvalidTransitionError{From: workflowkit.StatusSucceeded, To: workflowkit.StatusRunning, Op: "approve"})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("invalid transition status = %d, want 409", recorder.Code)
	}
}
