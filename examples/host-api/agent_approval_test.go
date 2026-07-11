package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eruca/artifactkit"
	"github.com/eruca/runkit"
	"github.com/eruca/workflowkit"
)

func TestHostAPIAgentToolApprovalWaitsBeforeWrite(t *testing.T) {
	cipher := &testApprovalCipher{}
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   cipher,
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-tools"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	created := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-agent-tool-pending",
		"input": "Review this draft with the approval-gated tool.",
		"task_profile": map[string]any{
			"needs_tools": true,
		},
	})
	if created.Status != string(workflowkit.StatusWaitingApproval) {
		t.Fatalf("create status = %q, want waiting_approval", created.Status)
	}
	if created.AgentApproval == nil {
		t.Fatalf("agent approval = nil, want safe pending tool metadata")
	}
	if created.AgentApproval.CheckpointID == "" || len(created.AgentApproval.Tools) != 1 {
		t.Fatalf("agent approval = %#v, want one checkpointed tool", created.AgentApproval)
	}
	pending := created.AgentApproval.Tools[0]
	if pending.Index != 0 || pending.ToolCallID == "" || pending.Tool != "record_review" {
		t.Fatalf("pending tool = %#v", pending)
	}
	if len(cipher.encryptAAD) != 1 || len(cipher.encryptAAD[0]) == 0 {
		t.Fatalf("cipher AAD = %#v, want one non-empty checkpoint binding", cipher.encryptAAD)
	}
	_, err = server.artifacts.Get(context.Background(), "artifact:wf-agent-tool-pending:review-action")
	if !errors.Is(err, artifactkit.ErrArtifactNotFound) {
		t.Fatalf("review action exists before approval: %v", err)
	}
}

func TestHostAPIAgentToolApprovalResumesBeforeFinalWorkflowApproval(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-tools"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-agent-tool-approve")
	pending := created.AgentApproval.Tools[0]

	response := agentApprovalRequestForTest(t, server.Handler(), created.ID, map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": pending.ToolCallID,
			"tool":         pending.Tool,
			"allowed":      true,
		}},
	}, "Bearer test-operator")
	if response.Code != http.StatusOK {
		t.Fatalf("agent approval status = %d; body=%s", response.Code, response.Body.String())
	}
	var resumed workflowResponse
	if err := json.NewDecoder(response.Body).Decode(&resumed); err != nil {
		t.Fatalf("decode resumed workflow: %v", err)
	}
	if resumed.Status != string(workflowkit.StatusWaitingApproval) || resumed.AgentApproval != nil {
		t.Fatalf("resumed workflow = %#v, want final-output approval state", resumed)
	}
	if resumed.ApprovalRef != "approval:"+created.ID || resumed.OutputRef == "" {
		t.Fatalf("resumed workflow refs = %#v", resumed)
	}
	if _, err := server.artifacts.Get(t.Context(), "artifact:"+created.ID+":review-action"); err != nil {
		t.Fatalf("review action artifact after approval: %v", err)
	}
	run, err := server.runs.Get(t.Context(), created.AgentRunID)
	if err != nil {
		t.Fatalf("Get agent run: %v", err)
	}
	if run.Summary.Status != runkit.StatusSucceeded || run.Summary.ToolCalls != 1 {
		t.Fatalf("agent summary = %#v, want one completed tool", run.Summary)
	}

	finalResponse := approveWorkflowRequestForTest(t, server.Handler(), created.ID, map[string]string{"note": "accept final output"}, "Bearer test-operator")
	if finalResponse.Code != http.StatusOK {
		t.Fatalf("final approval status = %d; body=%s", finalResponse.Code, finalResponse.Body.String())
	}
}

func TestHostAPIAgentToolApprovalRejectionDoesNotExecuteTool(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-reject"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-agent-tool-reject")
	pending := created.AgentApproval.Tools[0]

	response := agentApprovalRequestForTest(t, server.Handler(), created.ID, map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": pending.ToolCallID,
			"tool":         pending.Tool,
			"allowed":      false,
		}},
	}, "Bearer test-operator")
	if response.Code != http.StatusOK {
		t.Fatalf("agent rejection status = %d; body=%s", response.Code, response.Body.String())
	}
	var rejected workflowResponse
	if err := json.NewDecoder(response.Body).Decode(&rejected); err != nil {
		t.Fatalf("decode rejected workflow: %v", err)
	}
	if rejected.Status != string(workflowkit.StatusCancelled) || rejected.AgentApproval != nil {
		t.Fatalf("rejected workflow = %#v, want cancelled without pending approval", rejected)
	}
	if _, err := server.artifacts.Get(t.Context(), "artifact:"+created.ID+":review-action"); !errors.Is(err, artifactkit.ErrArtifactNotFound) {
		t.Fatalf("review action exists after rejection: %v", err)
	}
	run, err := server.runs.Get(t.Context(), created.AgentRunID)
	if err != nil {
		t.Fatalf("Get agent run: %v", err)
	}
	if run.Summary.Status != runkit.StatusFailed || run.Summary.AbortReason != "agent tool approval rejected" {
		t.Fatalf("rejected agent summary = %#v", run.Summary)
	}
	checkpoints := server.runs.(runkit.CheckpointStore)
	checkpoint, err := checkpoints.GetCheckpoint(t.Context(), created.AgentApproval.CheckpointID, localApprovalTenant)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if checkpoint.Status != runkit.CheckpointRejected || checkpoint.Approval == nil || checkpoint.Approval.ApproverID != "operator-reject" {
		t.Fatalf("rejected checkpoint = %#v", checkpoint)
	}
}

func TestHostAPIAgentToolApprovalResumesAfterRuntimeReopen(t *testing.T) {
	runtimeHome := t.TempDir()
	server, err := NewServer(Config{
		RuntimeHome:           runtimeHome,
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-reopen"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-agent-tool-reopen")
	pending := created.AgentApproval.Tools[0]
	closeStoreIfPossible(t, server.workflows)
	closeStoreIfPossible(t, server.runs)

	reopened, err := NewServer(Config{
		RuntimeHome:           runtimeHome,
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-reopen"}},
	})
	if err != nil {
		t.Fatalf("reopen NewServer: %v", err)
	}
	response := agentApprovalRequestForTest(t, reopened.Handler(), created.ID, map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": pending.ToolCallID,
			"tool":         pending.Tool,
			"allowed":      true,
		}},
	}, "Bearer test-operator")
	if response.Code != http.StatusOK {
		t.Fatalf("approval after reopen status = %d; body=%s", response.Code, response.Body.String())
	}
	if _, err := reopened.artifacts.Get(t.Context(), "artifact:"+created.ID+":review-action"); err != nil {
		t.Fatalf("review action artifact after reopen: %v", err)
	}
}

func TestHostAPIAgentToolApprovalRejectsUnauthenticatedDecision(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome:          t.TempDir(),
		AgentApprovalCipher: &testApprovalCipher{},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-agent-tool-unauthenticated")
	pending := created.AgentApproval.Tools[0]
	response := agentApprovalRequestForTest(t, server.Handler(), created.ID, map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": pending.ToolCallID,
			"tool":         pending.Tool,
			"allowed":      true,
		}},
	}, "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d; body=%s", response.Code, response.Body.String())
	}
	if _, err := server.artifacts.Get(t.Context(), "artifact:"+created.ID+":review-action"); !errors.Is(err, artifactkit.ErrArtifactNotFound) {
		t.Fatalf("review action exists after rejected authentication: %v", err)
	}
	checkpoint, err := server.runs.(runkit.CheckpointStore).GetCheckpoint(t.Context(), created.AgentApproval.CheckpointID, localApprovalTenant)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if checkpoint.Status != runkit.CheckpointPending || checkpoint.Approval != nil {
		t.Fatalf("checkpoint after rejected authentication = %#v", checkpoint)
	}
}

func TestHostAPIAgentToolApprovalIgnoresStaleFailureAfterResume(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-stale"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-agent-tool-stale")
	staleRun, err := server.workflows.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get workflow before resume: %v", err)
	}
	pending := created.AgentApproval.Tools[0]
	response := agentApprovalRequestForTest(t, server.Handler(), created.ID, map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": pending.ToolCallID,
			"tool":         pending.Tool,
			"allowed":      true,
		}},
	}, "Bearer test-operator")
	if response.Code != http.StatusOK {
		t.Fatalf("approval status = %d; body=%s", response.Code, response.Body.String())
	}
	if err := server.failAgentApprovalWorkflow(t.Context(), staleRun, created.AgentApproval.CheckpointID, nil, errors.New("stale request")); !errors.Is(err, errAgentApprovalNotPending) {
		t.Fatalf("stale failure error = %v, want errAgentApprovalNotPending", err)
	}
	updated, err := server.workflows.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get workflow after stale failure: %v", err)
	}
	if updated.Status != workflowkit.StatusWaitingApproval || updated.AgentRunID != created.AgentRunID || agentApprovalFromMetadata(updated.Metadata) != nil {
		t.Fatalf("workflow after stale failure = %#v, want resumed final approval state", updated)
	}
}

func TestHostAPIAgentToolApprovalRejectsMismatchedResolutionBeforeWrite(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-mismatch"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-agent-tool-mismatch")
	pending := created.AgentApproval.Tools[0]
	response := agentApprovalRequestForTest(t, server.Handler(), created.ID, map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": "caller-substituted-call",
			"tool":         pending.Tool,
			"allowed":      true,
		}},
	}, "Bearer test-operator")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("mismatched resolution status = %d; body=%s", response.Code, response.Body.String())
	}
	if _, err := server.artifacts.Get(t.Context(), "artifact:"+created.ID+":review-action"); !errors.Is(err, artifactkit.ErrArtifactNotFound) {
		t.Fatalf("review action exists after mismatched resolution: %v", err)
	}
	run, err := server.workflows.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get workflow: %v", err)
	}
	if run.Status != workflowkit.StatusFailed {
		t.Fatalf("workflow after mismatched resolution = %#v, want failed", run)
	}
	checkpoint, err := server.runs.(runkit.CheckpointStore).GetCheckpoint(t.Context(), created.AgentApproval.CheckpointID, localApprovalTenant)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if checkpoint.Status != runkit.CheckpointFailed || checkpoint.FailureCode != "agent_resume_failed" {
		t.Fatalf("checkpoint after mismatched resolution = %#v", checkpoint)
	}
}

func TestHostAPIAgentToolApprovalRejectsCallerSuppliedFreeFormReason(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome:            t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-reason"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-agent-tool-reason")
	pending := created.AgentApproval.Tools[0]
	response := agentApprovalRequestForTest(t, server.Handler(), created.ID, map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": pending.ToolCallID,
			"tool":         pending.Tool,
			"allowed":      true,
			"reason":       "unbounded caller-provided reason",
		}},
	}, "Bearer test-operator")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("free-form reason status = %d; body=%s", response.Code, response.Body.String())
	}
	checkpoint, err := server.runs.(runkit.CheckpointStore).GetCheckpoint(t.Context(), created.AgentApproval.CheckpointID, localApprovalTenant)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if checkpoint.Status != runkit.CheckpointPending || checkpoint.Approval != nil {
		t.Fatalf("checkpoint after invalid request = %#v", checkpoint)
	}
}

func createToolApprovalWorkflow(t *testing.T, server *Server, id string) workflowResponse {
	t.Helper()
	created := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":    id,
		"input": "Review the approval-gated draft.",
		"task_profile": map[string]any{
			"needs_tools": true,
		},
	})
	if created.Status != string(workflowkit.StatusWaitingApproval) || created.AgentApproval == nil || len(created.AgentApproval.Tools) != 1 {
		t.Fatalf("tool workflow = %#v, want one pending tool approval", created)
	}
	return created
}

func agentApprovalRequestForTest(t *testing.T, handler http.Handler, workflowID string, body any, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal agent approval request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/workflows/"+workflowID+"/agent-approve", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", authorization)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type testApprovalCipher struct {
	encryptAAD [][]byte
}

func (c *testApprovalCipher) Encrypt(_ context.Context, plaintext, aad []byte) ([]byte, error) {
	c.encryptAAD = append(c.encryptAAD, append([]byte(nil), aad...))
	return append([]byte("test:"), plaintext...), nil
}

func (c *testApprovalCipher) Decrypt(_ context.Context, ciphertext, aad []byte) ([]byte, error) {
	if len(aad) == 0 || len(ciphertext) < len("test:") || string(ciphertext[:len("test:")]) != "test:" {
		return nil, errors.New("test cipher rejected ciphertext")
	}
	return append([]byte(nil), ciphertext[len("test:"):]...), nil
}
