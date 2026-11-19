package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eruca/goagents/evalkit"
	"github.com/eruca/goagents/workflowkit"
)

func TestHostAPIReturnsContentFreeWorkflowTrace(t *testing.T) {
	server, err := NewServer(Config{
		RuntimeHome:           t.TempDir(),
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-trace"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		closeStoreIfPossible(t, server.workflows)
		closeStoreIfPossible(t, server.runs)
	})
	const privateInput = "PRIVATE TRACE INPUT MUST NOT LEAK"
	created := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{
		"id":    "wf-trace-1",
		"input": privateInput,
	})
	if created.Status != string(workflowkit.StatusWaitingApproval) {
		t.Fatalf("created = %+v", created)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workflows/wf-trace-1/trace", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("trace status = %d, body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), privateInput) {
		t.Fatalf("trace leaks workflow input: %s", response.Body.String())
	}
	trace := doJSON[workflowTraceResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-trace-1/trace", nil)
	if trace.WorkflowID != "wf-trace-1" || trace.Outcome.Status != string(workflowkit.StatusWaitingApproval) || trace.Outcome.Metadata["run_mode"] != "sync" {
		t.Fatalf("trace outcome = %+v", trace)
	}
	if trace.Usage.LLMCalls != 1 || trace.Usage.InputTokens == 0 {
		t.Fatalf("trace usage = %+v", trace.Usage)
	}
	types := map[string]int{}
	var model traceStepResponse
	for _, step := range trace.Steps {
		types[step.Type]++
		if step.Type == evalkit.StepModelCall {
			model = step
		}
		if step.StartedAt != nil && step.StartedAt.Location().String() != "UTC" {
			t.Fatalf("step time not UTC: %+v", step)
		}
	}
	if types[evalkit.StepWorkflow] == 0 || types[evalkit.StepAgentEvent] == 0 || types[evalkit.StepModelCall] != 1 {
		t.Fatalf("step types = %v; steps=%+v", types, trace.Steps)
	}
	if model.Name != "local-free" || model.Status != evalkit.StatusSucceeded || model.Metadata["provider"] != "local" {
		t.Fatalf("model call step = %+v", model)
	}

	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/workflows/wf-missing/trace", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing workflow trace = %d", missing.Code)
	}
}
