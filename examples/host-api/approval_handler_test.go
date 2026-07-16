package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eruca/goagents/workflowkit"
)

func TestHostAPIRejectsUnauthenticatedApproval(t *testing.T) {
	server, err := NewServer(Config{LLMKitHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	createWaitingWorkflow(t, server, "wf-unauthenticated")

	response := approveWorkflowRequestForTest(t, server.Handler(), "wf-unauthenticated", map[string]string{"note": "accepted"}, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("approval status = %d; body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", response.Header().Get("WWW-Authenticate"))
	}
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error != "unauthorized" {
		t.Fatalf("error response = %#v", body)
	}
	run, err := server.workflows.Get(t.Context(), "wf-unauthenticated")
	if err != nil || run.Status != workflowkit.StatusWaitingApproval {
		t.Fatalf("workflow after rejected approval = %#v, %v", run, err)
	}
}

func TestHostAPIOIDCApprovalRecordsTokenSubject(t *testing.T) {
	provider := newOIDCTestProvider(t)
	authenticator, err := NewOIDCApprovalAuthenticator(t.Context(), provider.issuer, "host-api")
	if err != nil {
		t.Fatalf("NewOIDCApprovalAuthenticator: %v", err)
	}
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		ApprovalAuthenticator: authenticator,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	createWaitingWorkflow(t, server, "wf-oidc")

	response := approveWorkflowRequestForTest(t, server.Handler(), "wf-oidc", map[string]string{"note": "accepted"}, "Bearer "+provider.mintToken(t, "operator-oidc", "host-api", time.Now().Add(time.Hour)))
	if response.Code != http.StatusOK {
		t.Fatalf("approval status = %d; body=%s", response.Code, response.Body.String())
	}
	run, err := server.workflows.Get(t.Context(), "wf-oidc")
	if err != nil {
		t.Fatalf("Get workflow: %v", err)
	}
	approvedBy, _ := run.Metadata["approved_by"].(string)
	if approvedBy != "operator-oidc" {
		t.Fatalf("approved_by = %q, want operator-oidc", approvedBy)
	}
}

func TestHostAPIApprovalRejectsCallerSuppliedApprover(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome: t.TempDir(),
		ApprovalAuthenticator: testApprovalAuthenticator{
			identity: ApprovalIdentity{Subject: "operator-token"},
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	createWaitingWorkflow(t, server, "wf-spoofed-approver")

	response := approveWorkflowRequestForTest(t, server.Handler(), "wf-spoofed-approver", map[string]string{
		"approved_by": "caller-spoofed",
		"note":        "accepted",
	}, "Bearer ignored-by-test-authenticator")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("approval status = %d; body=%s", response.Code, response.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error != "invalid_json" {
		t.Fatalf("error response = %#v", body)
	}
}

func createWaitingWorkflow(t *testing.T, server *Server, id string) {
	t.Helper()
	created := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{
		"id":    id,
		"input": "Review approval authentication.",
	})
	if created.Status != string(workflowkit.StatusWaitingApproval) {
		t.Fatalf("created workflow = %#v", created)
	}
}

func approveWorkflowRequestForTest(t *testing.T, handler http.Handler, workflowID string, body any, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/workflows/"+workflowID+"/approve", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type testApprovalAuthenticator struct {
	identity ApprovalIdentity
	err      error
}

func (a testApprovalAuthenticator) AuthenticateApproval(_ context.Context, _ string) (ApprovalIdentity, error) {
	return a.identity, a.err
}
