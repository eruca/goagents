//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/llmkit/llmkit"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
)

func TestHostAPIProcessMVPBlackBoxClosure(t *testing.T) {
	binary := buildHostBinary(t)
	t.Run("approval skill and restart", func(t *testing.T) {
		runMVPApprovalSkillRestart(t, binary)
	})
	t.Run("provider failure requeue and success", func(t *testing.T) {
		runMVPProviderRequeue(t, binary)
	})
	t.Run("unregistered tool fails closed", func(t *testing.T) {
		runMVPUnregisteredTool(t, binary)
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
	processOutput := first.output.String() + second.output.String() + third.output.String()
	if strings.Contains(processOutput, mvpProviderAPIKey) {
		t.Fatal("host process output leaked the synthetic provider key")
	}
	if strings.Contains(processOutput, skillRoot) {
		t.Fatal("host process output leaked the configured Skill root")
	}
	cleanupKeychain()
}

func runMVPProviderRequeue(t *testing.T, binary string) {
	t.Helper()
	requireInteractiveLoginKeychain(t)
	provider := newMVPProviderStub(t, mvpProviderUnavailable)
	oidc := newOIDCTestProvider(t)
	runtimeHome := t.TempDir()
	writeMVPLLMKitConfig(t, runtimeHome, provider.URL())
	token := oidc.mintToken(t, "operator-mvp-requeue", "host-api", time.Now().Add(time.Hour))
	keychainService := fmt.Sprintf("%s.smoke.mvp.requeue.%d", localApprovalKeychainService, time.Now().UnixNano())
	cleanupKeychain := smokeKeychainCleanup(t, keychainService, localApprovalKeyID)
	t.Cleanup(cleanupKeychain)
	process := startHostProcessWithEnv(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, map[string]string{
		hostAPISkillRootEnv:  "",
		mvpProviderAPIKeyEnv: mvpProviderAPIKey,
	})

	created, status := processJSON[workflowResponse](t, process, http.MethodPost, "/workflows", map[string]any{
		"id":       "wf-mvp-provider-requeue",
		"input":    "Recover this workflow after a temporary provider outage.",
		"run_mode": string(RunModeQueued),
		"task_profile": map[string]any{
			"complexity": "simple",
			"privacy":    "cloud_allowed",
		},
	}, "")
	if status != http.StatusAccepted || created.Status != string(workflowkit.StatusPending) || created.RunMode != string(RunModeQueued) {
		t.Fatalf("create queued workflow status=%d workflow=%#v", status, created)
	}
	failed := waitForProcessWorkflowStatus(t, process, created.ID, workflowkit.StatusFailed)
	if failed.InputRef == "" || failed.OutputRef != failed.InputRef || failed.AgentRunID == "" || failed.ApprovalRef != "" {
		t.Fatalf("failed workflow=%#v, want retained ingest ref and observable failed agent run", failed)
	}
	failedRun, status := processJSON[agentRunResponse](t, process, http.MethodGet, "/agent-runs/"+failed.AgentRunID, nil, "")
	if status != http.StatusOK || failedRun.Status != string(runkit.StatusFailed) || failedRun.Summary.AbortReason == "" || failedRun.Summary.ToolCalls != 0 {
		t.Fatalf("failed provider agent run status=%d run=%#v", status, failedRun)
	}
	failedRoutes, status := processJSON[llmRoutesResponse](t, process, http.MethodGet, "/workflows/"+created.ID+"/llm-routes", nil, "")
	if status != http.StatusOK || !mvpRoutesContainFailure(failedRoutes.Routes, "provider_error", llmkit.ErrorClassTransient) {
		t.Fatalf("failed routes status=%d routes=%#v", status, failedRoutes.Routes)
	}

	provider.SetMode(mvpProviderReady)
	requeued, status := processJSON[workflowResponse](t, process, http.MethodPost, "/workflows/"+created.ID+"/requeue", nil, "")
	if status != http.StatusAccepted || requeued.Status != string(workflowkit.StatusPending) || requeued.RunMode != string(RunModeQueued) {
		t.Fatalf("requeue workflow status=%d workflow=%#v", status, requeued)
	}
	waiting := waitForProcessWorkflowStatus(t, process, created.ID, workflowkit.StatusWaitingApproval)
	if waiting.InputRef != failed.InputRef || waiting.OutputRef == "" || waiting.AgentRunID == "" || waiting.ApprovalRef == "" {
		t.Fatalf("retried workflow=%#v, want original input and new agent output refs", waiting)
	}
	completed, status := processJSON[workflowResponse](t, process, http.MethodPost, "/workflows/"+created.ID+"/approve", map[string]any{"note": "mvp provider retry accepted"}, "Bearer "+token)
	if status != http.StatusOK || completed.Status != string(workflowkit.StatusSucceeded) || completed.OutputRef == "" || completed.OutputRef == waiting.OutputRef || completed.AgentRunID != waiting.AgentRunID {
		t.Fatalf("complete requeued workflow status=%d workflow=%#v", status, completed)
	}

	routes, status := processJSON[llmRoutesResponse](t, process, http.MethodGet, "/workflows/"+created.ID+"/llm-routes", nil, "")
	if status != http.StatusOK || !mvpRoutesContainFailure(routes.Routes, "provider_error", llmkit.ErrorClassTransient) || !mvpRoutesContainSuccess(routes.Routes) {
		t.Fatalf("final routes status=%d routes=%#v", status, routes.Routes)
	}
	events, status := processJSON[workflowEventsResponse](t, process, http.MethodGet, "/workflows/"+created.ID+"/events", nil, "")
	if status != http.StatusOK || events.Status != string(workflowkit.StatusSucceeded) || !mvpEventsContainFailedStep(events.Events) || !mvpEventsContainRequeue(events.Events) {
		t.Fatalf("final events status=%d response=%#v", status, events)
	}
	run, status := processJSON[agentRunResponse](t, process, http.MethodGet, "/agent-runs/"+completed.AgentRunID, nil, "")
	if status != http.StatusOK || run.Status != string(runkit.StatusSucceeded) || run.Summary.ToolCalls != 0 {
		t.Fatalf("requeued agent run status=%d run=%#v", status, run)
	}
	if strings.Contains(process.output.String(), mvpProviderAPIKey) {
		t.Fatal("host process output leaked the synthetic provider key")
	}
	stopHostProcess(t, process)
	cleanupKeychain()
}

func runMVPUnregisteredTool(t *testing.T, binary string) {
	t.Helper()
	requireInteractiveLoginKeychain(t)
	provider := newMVPProviderStub(t, mvpProviderUnregisteredTool)
	oidc := newOIDCTestProvider(t)
	runtimeHome := t.TempDir()
	writeMVPLLMKitConfig(t, runtimeHome, provider.URL())
	keychainService := fmt.Sprintf("%s.smoke.mvp.unregistered.%d", localApprovalKeychainService, time.Now().UnixNano())
	cleanupKeychain := smokeKeychainCleanup(t, keychainService, localApprovalKeyID)
	t.Cleanup(cleanupKeychain)
	process := startHostProcessWithEnv(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, map[string]string{
		hostAPISkillRootEnv:  "",
		mvpProviderAPIKeyEnv: mvpProviderAPIKey,
	})

	created, status := processJSON[workflowResponse](t, process, http.MethodPost, "/workflows", map[string]any{
		"id":       "wf-mvp-unregistered-tool",
		"input":    "Reject any tool that is not in the host registry.",
		"run_mode": string(RunModeQueued),
		"task_profile": map[string]any{
			"complexity":  "simple",
			"privacy":     "cloud_allowed",
			"needs_tools": true,
		},
	}, "")
	if status != http.StatusAccepted || created.Status != string(workflowkit.StatusPending) {
		t.Fatalf("create unregistered-tool workflow status=%d workflow=%#v", status, created)
	}
	failed := waitForProcessWorkflowStatus(t, process, created.ID, workflowkit.StatusFailed)
	if failed.AgentRunID == "" || failed.AgentApproval != nil || failed.ApprovalRef != "" {
		t.Fatalf("failed workflow=%#v, want observable failed agent run without approval", failed)
	}

	routes, status := processJSON[llmRoutesResponse](t, process, http.MethodGet, "/workflows/"+created.ID+"/llm-routes", nil, "")
	if status != http.StatusOK || len(routes.Routes) != 1 || !mvpRoutesContainSuccess(routes.Routes) {
		t.Fatalf("unregistered-tool routes status=%d routes=%#v", status, routes.Routes)
	}
	events, status := processJSON[workflowEventsResponse](t, process, http.MethodGet, "/workflows/"+created.ID+"/events", nil, "")
	if status != http.StatusOK || events.Status != string(workflowkit.StatusFailed) || !mvpEventsContainFailedStep(events.Events) {
		t.Fatalf("unregistered-tool events status=%d response=%#v", status, events)
	}
	if !workflowEventsContain(events.Events, func(event workflowEventResponse) bool {
		return event.Type == "step" && event.Name == "agent_review" && strings.Contains(event.Error, `tool "unregistered_tool" not registered`)
	}) {
		t.Fatalf("workflow events=%#v, want explicit unregistered-tool failure", events.Events)
	}

	run, status := processJSON[agentRunResponse](t, process, http.MethodGet, "/agent-runs/"+failed.AgentRunID, nil, "")
	if status != http.StatusOK || run.Status != string(runkit.StatusFailed) || run.Summary.ToolCalls != 0 || len(run.Summary.UsedTools) != 0 {
		t.Fatalf("failed agent run status=%d run=%#v", status, run)
	}
	for _, event := range run.Events {
		if strings.HasPrefix(event.Type, "tool.") {
			t.Fatalf("unregistered tool emitted execution event: %#v", event)
		}
	}
	requests := provider.Requests()
	if len(requests) != 1 || requests[0].HasToolObservation || !slicesContain(requests[0].ToolNames, recordReviewToolName) {
		t.Fatalf("provider requests=%#v, want one registered-tool offer and no tool observation", requests)
	}
	if strings.Contains(process.output.String(), mvpProviderAPIKey) {
		t.Fatal("host process output leaked the synthetic provider key")
	}
	stopHostProcess(t, process)
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

func mvpRoutesContainFailure(routes []llmRouteResponse, errorCode string, errorClass llmkit.ErrorClass) bool {
	for _, route := range routes {
		if route.Outcome != nil && !route.Outcome.Success && route.Outcome.ErrorCode == errorCode && route.Outcome.ErrorClass == string(errorClass) {
			return true
		}
	}
	return false
}

func mvpEventsContainFailedStep(events []workflowEventResponse) bool {
	for _, event := range events {
		if event.Type == "step" && event.Status == string(workflowkit.StatusFailed) && event.Error != "" {
			return true
		}
	}
	return false
}

func mvpEventsContainRequeue(events []workflowEventResponse) bool {
	for _, event := range events {
		if event.Type == "workflow_requeued" && event.FromStatus == string(workflowkit.StatusFailed) && event.ToStatus == string(workflowkit.StatusPending) && !event.At.IsZero() {
			return true
		}
	}
	return false
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
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
