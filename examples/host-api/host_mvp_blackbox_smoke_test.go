//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eruca/runkit"
	"github.com/eruca/workflowkit"
)

func TestHostAPIProcessMVPBlackBoxClosure(t *testing.T) {
	binary := buildHostBinary(t)
	t.Run("approval skill and restart", func(t *testing.T) {
		runMVPApprovalSkillRestart(t, binary)
	})
}

func runMVPApprovalSkillRestart(t *testing.T, binary string) {
	t.Helper()
	requireInteractiveLoginKeychain(t)
	provider := newMVPProviderStub(t, mvpProviderReady)
	oidc := newOIDCTestProvider(t)
	runtimeHome := t.TempDir()
	writeMVPLLMKitConfig(t, runtimeHome, provider.URL())
	token := oidc.mintToken(t, "operator-mvp", "host-api", time.Now().Add(time.Hour))
	skillRoot, err := filepath.Abs("skills")
	if err != nil {
		t.Fatalf("resolve bundled Skill root: %v", err)
	}
	keychainService := fmt.Sprintf("%s.smoke.mvp.%d", localApprovalKeychainService, time.Now().UnixNano())
	cleanupKeychain := smokeKeychainCleanup(t, keychainService, localApprovalKeyID)
	t.Cleanup(cleanupKeychain)
	environment := mvpHostEnvironment(skillRoot)

	first := startHostProcessWithEnv(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment)
	listed, status := processJSON[skillListResponse](t, first, http.MethodGet, "/skills", nil, "")
	if status != http.StatusOK || len(listed.Skills) != 1 || listed.Skills[0].Name != "workflow-review" || len(listed.Skills[0].Digest) != 64 {
		t.Fatalf("GET /skills status=%d skills=%#v", status, listed.Skills)
	}
	digest := listed.Skills[0].Digest

	toolWorkflow, status := processJSON[workflowResponse](t, first, http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-mvp-tool",
		"input": "Review the MVP workflow with the registered tool.",
		"task_profile": map[string]any{
			"complexity":  "simple",
			"privacy":     "cloud_allowed",
			"needs_tools": true,
		},
	}, "")
	if status != http.StatusAccepted || toolWorkflow.AgentApproval == nil || len(toolWorkflow.AgentApproval.Tools) != 1 || toolWorkflow.AgentApproval.Tools[0].Tool != recordReviewToolName {
		t.Fatalf("create tool workflow status=%d workflow=%#v", status, toolWorkflow)
	}
	pending := toolWorkflow.AgentApproval.Tools[0]
	_, invalidStatus := processJSON[map[string]any](t, first, http.MethodPost, "/workflows/"+toolWorkflow.ID+"/agent-approve", map[string]any{
		"resolutions": []map[string]any{{
			"index": pending.Index, "tool_call_id": pending.ToolCallID, "tool": pending.Tool, "allowed": true,
		}},
	}, "Bearer invalid")
	if invalidStatus != http.StatusUnauthorized {
		t.Fatalf("invalid approval status=%d, want 401", invalidStatus)
	}
	stopHostProcess(t, first)

	second := startHostProcessWithEnv(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment)
	persisted, status := processJSON[workflowResponse](t, second, http.MethodGet, "/workflows/"+toolWorkflow.ID, nil, "")
	if status != http.StatusOK || persisted.Status != string(workflowkit.StatusWaitingApproval) || persisted.AgentApproval == nil || persisted.AgentApproval.CheckpointID != toolWorkflow.AgentApproval.CheckpointID {
		t.Fatalf("persisted tool workflow status=%d workflow=%#v", status, persisted)
	}
	resumed, status := processJSON[workflowResponse](t, second, http.MethodPost, "/workflows/"+toolWorkflow.ID+"/agent-approve", map[string]any{
		"resolutions": []map[string]any{{
			"index": pending.Index, "tool_call_id": pending.ToolCallID, "tool": pending.Tool, "allowed": true,
		}},
	}, "Bearer "+token)
	if status != http.StatusOK || resumed.Status != string(workflowkit.StatusWaitingApproval) || resumed.AgentApproval != nil {
		t.Fatalf("resume tool workflow status=%d workflow=%#v", status, resumed)
	}
	completedTool, status := processJSON[workflowResponse](t, second, http.MethodPost, "/workflows/"+toolWorkflow.ID+"/approve", map[string]any{"note": "mvp tool accepted"}, "Bearer "+token)
	if status != http.StatusOK || completedTool.Status != string(workflowkit.StatusSucceeded) || completedTool.OutputRef == "" || completedTool.AgentRunID == "" {
		t.Fatalf("complete tool workflow status=%d workflow=%#v", status, completedTool)
	}

	skillWorkflow, status := processJSON[workflowResponse](t, second, http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-mvp-skill",
		"input": "Review the MVP workflow with the selected Skill.",
		"task_profile": map[string]any{
			"complexity": "simple",
			"privacy":    "cloud_allowed",
		},
		"skill_refs": []map[string]string{{"name": "workflow-review"}},
	}, "")
	if status != http.StatusAccepted || skillWorkflow.Status != string(workflowkit.StatusWaitingApproval) || len(skillWorkflow.SkillRefs) != 1 || skillWorkflow.SkillRefs[0].Digest != digest {
		t.Fatalf("create skill workflow status=%d workflow=%#v", status, skillWorkflow)
	}
	completedSkill, status := processJSON[workflowResponse](t, second, http.MethodPost, "/workflows/"+skillWorkflow.ID+"/approve", map[string]any{"note": "mvp skill accepted"}, "Bearer "+token)
	if status != http.StatusOK || completedSkill.Status != string(workflowkit.StatusSucceeded) || completedSkill.OutputRef == "" || completedSkill.AgentRunID == "" {
		t.Fatalf("complete skill workflow status=%d workflow=%#v", status, completedSkill)
	}
	stopHostProcess(t, second)

	third := startHostProcessWithEnv(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment)
	assertMVPCompletedWorkflow(t, third, completedTool, 1)
	assertMVPCompletedWorkflow(t, third, completedSkill, 0)
	listedAfterRestart, status := processJSON[skillListResponse](t, third, http.MethodGet, "/skills", nil, "")
	if status != http.StatusOK || len(listedAfterRestart.Skills) != 1 || listedAfterRestart.Skills[0].Digest != digest {
		t.Fatalf("skills after restart status=%d skills=%#v", status, listedAfterRestart.Skills)
	}
	stopHostProcess(t, third)

	requests := provider.Requests()
	var sawToolRequest, sawToolObservation bool
	for _, request := range requests {
		if request.Authorization != "Bearer "+mvpProviderAPIKey {
			t.Fatalf("provider authorization was not the synthetic smoke key")
		}
		for _, tool := range request.ToolNames {
			if tool == recordReviewToolName {
				sawToolRequest = true
			}
		}
		if request.HasToolObservation {
			sawToolObservation = true
		}
	}
	if !sawToolRequest || !sawToolObservation {
		t.Fatalf("provider requests missing tool round trip: %#v", requests)
	}
	if strings.Contains(first.output.String()+second.output.String()+third.output.String(), mvpProviderAPIKey) {
		t.Fatal("host process output leaked the synthetic provider key")
	}
	cleanupKeychain()
}

func assertMVPCompletedWorkflow(t *testing.T, process *hostProcess, expected workflowResponse, wantToolCalls int) {
	t.Helper()
	workflow, status := processJSON[workflowResponse](t, process, http.MethodGet, "/workflows/"+expected.ID, nil, "")
	if status != http.StatusOK || workflow.Status != string(workflowkit.StatusSucceeded) || workflow.InputRef != expected.InputRef || workflow.OutputRef != expected.OutputRef || workflow.AgentRunID != expected.AgentRunID {
		t.Fatalf("workflow after restart status=%d workflow=%#v expected=%#v", status, workflow, expected)
	}
	run, status := processJSON[agentRunResponse](t, process, http.MethodGet, "/agent-runs/"+workflow.AgentRunID, nil, "")
	if status != http.StatusOK || run.Status != string(runkit.StatusSucceeded) || run.Summary.ToolCalls != wantToolCalls {
		t.Fatalf("agent run status=%d run=%#v wantToolCalls=%d", status, run, wantToolCalls)
	}
	routes, status := processJSON[llmRoutesResponse](t, process, http.MethodGet, "/workflows/"+workflow.ID+"/llm-routes", nil, "")
	if status != http.StatusOK || !mvpRoutesContainSuccess(routes.Routes) {
		t.Fatalf("routes status=%d routes=%#v", status, routes.Routes)
	}
	events, status := processJSON[workflowEventsResponse](t, process, http.MethodGet, "/workflows/"+workflow.ID+"/events", nil, "")
	if status != http.StatusOK || events.Status != string(workflowkit.StatusSucceeded) || len(events.Events) == 0 {
		t.Fatalf("events status=%d response=%#v", status, events)
	}
}

func mvpRoutesContainSuccess(routes []llmRouteResponse) bool {
	for _, route := range routes {
		if route.Outcome != nil && route.Outcome.Success {
			return true
		}
	}
	return false
}

func waitForProcessWorkflowStatus(t *testing.T, process *hostProcess, id string, want workflowkit.Status) workflowResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last workflowResponse
	for time.Now().Before(deadline) {
		last, _ = processJSON[workflowResponse](t, process, http.MethodGet, "/workflows/"+id, nil, "")
		if last.Status == string(want) {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("workflow %s status=%q, want=%q; last=%+v", id, last.Status, want, last)
	return workflowResponse{}
}
