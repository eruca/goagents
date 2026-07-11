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
	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/ports"
	"github.com/eruca/llmkit/llmkit"
	"github.com/eruca/runkit"
	"github.com/eruca/skillkit"
	"github.com/eruca/workflowkit"
)

type skillListPayload struct {
	Skills []skillPayload `json:"skills"`
}

type skillPayload struct {
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	Digest       string        `json:"digest"`
	Scope        string        `json:"scope"`
	Availability string        `json:"availability"`
	Reasons      []skillReason `json:"reasons"`
}

type skillReason struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
}

type workflowSkillRefsResponse struct {
	ID        string `json:"id"`
	SkillRefs []struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
	} `json:"skill_refs"`
}

func TestListSkillsReturnsSafeAvailability(t *testing.T) {
	eligibleRoot := t.TempDir()
	writeHostAPISkill(t, eligibleRoot, "eligible-skill", "---\nname: eligible-skill\ndescription: A safe eligible skill.\nmetadata:\n  goagents:\n    requires:\n      os: [darwin]\n      host_features: [artifacts.v1]\n      tools:\n        required: [artifact.read]\n    resources:\n      allow: [references/private.md]\n---\n# Instructions\nPRIVATE SKILL BODY\n", map[string]string{
		"references/private.md": "PRIVATE RESOURCE BODY",
	})

	unavailableRoot := t.TempDir()
	writeHostAPISkill(t, unavailableRoot, "unavailable-skill", "---\nname: unavailable-skill\ndescription: An unavailable skill.\n---\n# Instructions\nUNAVAILABLE PRIVATE BODY\n", nil)

	invalidRoot := t.TempDir()
	writeHostAPISkill(t, invalidRoot, "invalid-skill", "---\nname: invalid-skill\ndescription: \n---\n# Instructions\nINVALID PRIVATE BODY\n", nil)

	catalog, err := skillkit.Discover([]skillkit.Root{
		{ID: "builtin-catalog", Dir: eligibleRoot, Scope: skillkit.ScopeBuiltin, Trusted: true, Enabled: true},
		{ID: unavailableRoot, Dir: unavailableRoot, Scope: skillkit.ScopeWorkspace, Trusted: false, Enabled: true},
		{ID: "invalid-catalog", Dir: invalidRoot, Scope: skillkit.ScopeUser, Trusted: true, Enabled: true},
	})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}

	server, err := NewServer(Config{
		LLMKitHome:   t.TempDir(),
		SkillCatalog: catalog,
		SkillGateContext: skillkit.GateContext{
			OS:             "darwin",
			HostFeatures:   map[string]bool{"artifacts.v1": true},
			AllowedToolIDs: map[string]bool{"artifact.read": true},
		},
	})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	resp := httptest.NewRecorder()
	server.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/skills", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /skills status = %d; body=%s", resp.Code, resp.Body.String())
	}

	var payload skillListPayload
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode GET /skills response: %v; body=%s", err, resp.Body.String())
	}
	if len(payload.Skills) != 3 {
		t.Fatalf("GET /skills returned %d skills, want 3: %+v", len(payload.Skills), payload.Skills)
	}

	byName := make(map[string]skillPayload, len(payload.Skills))
	for _, skill := range payload.Skills {
		byName[skill.Name] = skill
	}

	if skill := byName["eligible-skill"]; skill.Description != "A safe eligible skill." || skill.Digest == "" || skill.Scope != string(skillkit.ScopeBuiltin) || skill.Availability != string(skillkit.AvailabilityEligible) || len(skill.Reasons) != 0 {
		t.Fatalf("eligible skill = %+v, want an eligible builtin skill without reasons", skill)
	}
	if skill := byName["unavailable-skill"]; skill.Scope != string(skillkit.ScopeWorkspace) || skill.Availability != string(skillkit.AvailabilityUnavailable) || !containsSkillReason(skill.Reasons, "untrusted_root", "configured_root") {
		t.Fatalf("unavailable skill = %+v, want untrusted root reason", skill)
	}
	if skill := byName["invalid-skill"]; skill.Scope != string(skillkit.ScopeUser) || skill.Availability != string(skillkit.AvailabilityInvalid) || len(skill.Reasons) == 0 {
		t.Fatalf("invalid skill = %+v, want invalid availability with reasons", skill)
	}

	for _, secret := range []string{eligibleRoot, unavailableRoot, invalidRoot, "PRIVATE SKILL BODY", "PRIVATE RESOURCE BODY", "UNAVAILABLE PRIVATE BODY", "INVALID PRIVATE BODY", "references/private.md"} {
		if strings.Contains(resp.Body.String(), secret) {
			t.Fatalf("GET /skills leaked %q: %s", secret, resp.Body.String())
		}
	}
}

func TestCreateWorkflowPersistsResolvedSkillRefs(t *testing.T) {
	skillRoot := t.TempDir()
	writeHostAPISkill(t, skillRoot, "workflow-review", "---\nname: workflow-review\ndescription: Review a workflow safely.\nmetadata:\n  goagents:\n    requires:\n      tools:\n        required: [artifact.read]\n---\n# Instructions\nReview the workflow.\n", nil)
	catalog, err := skillkit.Discover([]skillkit.Root{{
		ID:      "workflow-skills",
		Dir:     skillRoot,
		Scope:   skillkit.ScopeBuiltin,
		Trusted: true,
		Enabled: true,
	}})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}

	eligibleGate := skillkit.GateContext{AllowedToolIDs: map[string]bool{"artifact.read": true}}
	runtimeHome := t.TempDir()
	server, err := NewServer(Config{
		RuntimeHome:      runtimeHome,
		SkillCatalog:     catalog,
		SkillGateContext: eligibleGate,
	})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}

	created := doJSON[workflowSkillRefsResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-skill-refs",
		"input": "Review this workflow.",
		"skill_refs": []map[string]string{{
			"name": "workflow-review",
		}},
	})
	if len(created.SkillRefs) != 1 || created.SkillRefs[0].Name != "workflow-review" || created.SkillRefs[0].Digest == "" {
		t.Fatalf("create skill refs = %+v, want resolved workflow-review digest", created.SkillRefs)
	}
	stored, err := server.workflows.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get persisted workflow: %v", err)
	}
	persistedJSON, err := json.Marshal(stored.Metadata["skill_refs"])
	if err != nil {
		t.Fatalf("marshal persisted skill_refs: %v", err)
	}
	var persistedRefs []map[string]string
	if err := json.Unmarshal(persistedJSON, &persistedRefs); err != nil {
		t.Fatalf("decode persisted skill_refs: %v", err)
	}
	if len(persistedRefs) != 1 || len(persistedRefs[0]) != 2 || persistedRefs[0]["name"] != created.SkillRefs[0].Name || persistedRefs[0]["digest"] != created.SkillRefs[0].Digest {
		t.Fatalf("persisted skill_refs = %#v, want only complete name/digest maps", stored.Metadata["skill_refs"])
	}

	closeStoreIfPossible(t, server.workflows)
	closeStoreIfPossible(t, server.runs)
	reopened, err := NewServer(Config{
		RuntimeHome:      runtimeHome,
		SkillCatalog:     catalog,
		SkillGateContext: eligibleGate,
	})
	if err != nil {
		t.Fatalf("reopen NewServer returned error: %v", err)
	}
	loaded := doJSON[workflowSkillRefsResponse](t, reopened.Handler(), http.MethodGet, "/workflows/wf-skill-refs", nil)
	if len(loaded.SkillRefs) != 1 || loaded.SkillRefs[0] != created.SkillRefs[0] {
		t.Fatalf("skill refs after reopen = %+v, want %+v", loaded.SkillRefs, created.SkillRefs)
	}

	t.Run("duplicate resolved reference", func(t *testing.T) {
		body, err := json.Marshal(map[string]any{
			"id":    "wf-duplicate-skill-refs",
			"input": "Reject duplicate workflow skills before creation.",
			"skill_refs": []map[string]string{
				{"name": "workflow-review"},
				{"name": "workflow-review"},
			},
		})
		if err != nil {
			t.Fatalf("marshal duplicate create request: %v", err)
		}
		response := httptest.NewRecorder()
		reopened.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/workflows", bytes.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("POST /workflows with duplicate skill refs status = %d; body=%s, want 400", response.Code, response.Body.String())
		}
	})

	for _, test := range []struct {
		name   string
		config Config
		ref    map[string]string
	}{
		{
			name:   "missing catalog",
			config: Config{RuntimeHome: t.TempDir()},
			ref:    map[string]string{"name": "workflow-review"},
		},
		{
			name: "unknown skill",
			config: Config{
				RuntimeHome:      t.TempDir(),
				SkillCatalog:     catalog,
				SkillGateContext: eligibleGate,
			},
			ref: map[string]string{"name": "unknown-skill"},
		},
		{
			name: "required tool unavailable",
			config: Config{
				RuntimeHome:  t.TempDir(),
				SkillCatalog: catalog,
			},
			ref: map[string]string{"name": "workflow-review"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalidServer, err := NewServer(test.config)
			if err != nil {
				t.Fatalf("NewServer returned error: %v", err)
			}
			body, err := json.Marshal(map[string]any{
				"id":         "wf-invalid-" + strings.ReplaceAll(test.name, " ", "-"),
				"input":      "Reject unavailable skills before workflow creation.",
				"skill_refs": []map[string]string{test.ref},
			})
			if err != nil {
				t.Fatalf("marshal create request: %v", err)
			}
			response := httptest.NewRecorder()
			invalidServer.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/workflows", bytes.NewReader(body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("POST /workflows status = %d; body=%s, want 400", response.Code, response.Body.String())
			}
		})
	}
}

func TestWorkflowSkillRefsActivateSameDigestAfterRestart(t *testing.T) {
	skillRoot := t.TempDir()
	const skillBody = "PERSISTED SKILL BODY: preserve the exact approved workflow review policy."
	writeHostAPISkill(t, skillRoot, "resumable-review", "---\nname: resumable-review\ndescription: Resume the approved workflow review safely.\n---\n# Instructions\n"+skillBody+"\n", nil)
	catalog, err := skillkit.Discover([]skillkit.Root{{
		ID:      "resumable-skills",
		Dir:     skillRoot,
		Scope:   skillkit.ScopeBuiltin,
		Trusted: true,
		Enabled: true,
	}})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}

	runtimeHome := t.TempDir()
	cipher := &testApprovalCipher{}
	config := Config{
		RuntimeHome:           runtimeHome,
		SkillCatalog:          catalog,
		SkillGateContext:      skillkit.GateContext{},
		AgentApprovalCipher:   cipher,
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-skill-restart"}},
	}
	server, err := NewServer(config)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	provider := &recordingSkillProvider{}
	server.providers["local-free"] = provider

	created := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-skill-restart",
		"input": "Review this persisted-skill workflow.",
		"skill_refs": []map[string]string{{
			"name": "resumable-review",
		}},
		"task_profile": map[string]any{"needs_tools": true},
	})
	if created.AgentApproval == nil || len(created.AgentApproval.Tools) != 1 {
		t.Fatalf("created workflow = %#v, want one pending tool approval", created)
	}
	if len(provider.requests) != 1 || !chatRequestContains(provider.requests[0], skillBody) {
		t.Fatalf("first model request = %#v, want persisted skill body", provider.requests)
	}

	storedCheckpoint, err := server.runs.(runkit.CheckpointStore).GetCheckpoint(t.Context(), created.AgentApproval.CheckpointID, localApprovalTenant)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	// The test cipher checks only that checkpoint AAD is present; production
	// decryption additionally binds the exact checkpoint identity.
	payload, err := cipher.Decrypt(t.Context(), storedCheckpoint.Ciphertext, []byte("checkpoint"))
	if err != nil {
		t.Fatalf("decrypt checkpoint: %v", err)
	}
	var checkpoint agentcore.RunCheckpoint
	if err := json.Unmarshal(payload, &checkpoint); err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	refs, ok := checkpoint.Request.Metadata["skill_refs"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("checkpoint skill_refs = %#v, want one persisted ref", checkpoint.Request.Metadata["skill_refs"])
	}
	checkpointRef, ok := refs[0].(map[string]any)
	if !ok || checkpointRef["name"] != "resumable-review" || checkpointRef["digest"] != created.SkillRefs[0].Digest {
		t.Fatalf("checkpoint skill_refs = %#v, want resolved resumable-review digest", refs)
	}

	closeStoreIfPossible(t, server.workflows)
	closeStoreIfPossible(t, server.runs)
	reopened, err := NewServer(config)
	if err != nil {
		t.Fatalf("reopen NewServer: %v", err)
	}
	reopened.providers["local-free"] = provider

	checkpoint.Request.Metadata["skill_refs"] = []any{map[string]any{"name": "resumable-review"}}
	requestsBeforeInvalidResume := len(provider.requests)
	if _, err := reopened.agentApprovals.runner.ResumeDetailed(t.Context(), checkpoint, nil); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("ResumeDetailed with missing persisted digest error = %v, want digest validation failure", err)
	}
	if len(provider.requests) != requestsBeforeInvalidResume {
		t.Fatalf("invalid persisted digest called model: requests=%d, want %d", len(provider.requests), requestsBeforeInvalidResume)
	}

	pending := created.AgentApproval.Tools[0]
	response := agentApprovalRequestForTest(t, reopened.Handler(), created.ID, map[string]any{
		"resolutions": []map[string]any{{
			"index":        pending.Index,
			"tool_call_id": pending.ToolCallID,
			"tool":         pending.Tool,
			"allowed":      true,
		}},
	}, "Bearer test-operator")
	if response.Code != http.StatusOK {
		t.Fatalf("approval after restart status = %d; body=%s", response.Code, response.Body.String())
	}
	if len(provider.requests) != 2 || !chatRequestContains(provider.requests[1], skillBody) {
		t.Fatalf("resumed model requests = %#v, want persisted skill body after restart", provider.requests)
	}
}

type recordingSkillProvider struct {
	requests []ports.ChatRequest
}

func (p *recordingSkillProvider) Chat(_ context.Context, request ports.ChatRequest) (*ports.ChatResponse, error) {
	p.requests = append(p.requests, request)
	if len(request.Tools) > 0 && !hasToolObservation(request.Messages) {
		return &ports.ChatResponse{
			ToolCalls: []ports.ToolCall{{
				ID:    "call-record-review",
				Name:  recordReviewToolName,
				Input: json.RawMessage(`{}`),
			}},
		}, nil
	}
	return &ports.ChatResponse{Content: "skill-aware workflow response"}, nil
}

func chatRequestContains(request ports.ChatRequest, text string) bool {
	for _, message := range request.Messages {
		if strings.Contains(message.Content, text) {
			return true
		}
	}
	return false
}

func writeHostAPISkill(t *testing.T, root, name, source string, resources map[string]string) {
	t.Helper()
	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("create skill directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	for path, content := range resources {
		resourcePath := filepath.Join(skillDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(resourcePath), 0o755); err != nil {
			t.Fatalf("create resource directory: %v", err)
		}
		if err := os.WriteFile(resourcePath, []byte(content), 0o600); err != nil {
			t.Fatalf("write resource: %v", err)
		}
	}
}

func containsSkillReason(reasons []skillReason, code, subject string) bool {
	for _, reason := range reasons {
		if reason.Code == code && reason.Subject == subject {
			return true
		}
	}
	return false
}

func TestHostAPIWorkflowApprovalRunAndModelEndpoints(t *testing.T) {
	server, err := NewServer(Config{
		LLMKitHome: t.TempDir(),
		ApprovalAuthenticator: testApprovalAuthenticator{
			identity: ApprovalIdentity{Subject: "operator-api"},
		},
	})
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

	approved := doJSON[workflowResponse](t, handler, http.MethodPost, "/workflows/wf-api-1/approve", map[string]string{"note": "accepted"})
	if approved.Status != string(workflowkit.StatusSucceeded) {
		t.Fatalf("approved response = %+v, want succeeded", approved)
	}
	if approved.OutputRef == "" || approved.AuditRef == "" {
		t.Fatalf("approved refs should be populated: %+v", approved)
	}
}

func TestHostAPIDurableRuntimeResumesWorkflowAfterReopen(t *testing.T) {
	runtimeHome := t.TempDir()
	server, err := NewServer(Config{
		RuntimeHome: runtimeHome,
		ApprovalAuthenticator: testApprovalAuthenticator{
			identity: ApprovalIdentity{Subject: "operator-durable"},
		},
	})
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

	reopened, err := NewServer(Config{
		RuntimeHome: runtimeHome,
		ApprovalAuthenticator: testApprovalAuthenticator{
			identity: ApprovalIdentity{Subject: "operator-durable"},
		},
	})
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

	approved := doJSON[workflowResponse](t, reopened.Handler(), http.MethodPost, "/workflows/wf-durable-1/approve", map[string]string{"note": "resume after reopen"})
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

func TestHostAPIQueuedWorkerRecoversPendingWorkflowAfterReopen(t *testing.T) {
	runtimeHome := t.TempDir()
	server, err := NewServer(Config{RuntimeHome: runtimeHome})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	inputRef := "artifact:wf-queued-recover:input"
	if err := putTextArtifact(context.Background(), server.artifacts, inputRef, "Review queued recovery."); err != nil {
		t.Fatalf("put input artifact returned error: %v", err)
	}
	if err := server.workflows.Save(context.Background(), workflowkit.WorkflowRun{
		ID:       "wf-queued-recover",
		Status:   workflowkit.StatusPending,
		InputRef: inputRef,
		Metadata: map[string]any{
			"input_ref": inputRef,
		},
	}); err != nil {
		t.Fatalf("Save pending workflow returned error: %v", err)
	}
	closeStoreIfPossible(t, server.workflows)
	closeStoreIfPossible(t, server.runs)

	reopened, err := NewServer(Config{RuntimeHome: runtimeHome})
	if err != nil {
		t.Fatalf("reopen NewServer returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reopened.StartQueuedWorker(ctx)

	loaded := waitForWorkflowStatus(t, reopened.Handler(), "wf-queued-recover", workflowkit.StatusWaitingApproval)
	if loaded.AgentRunID == "" || loaded.OutputRef == "" || loaded.ApprovalRef == "" {
		t.Fatalf("recovered queued workflow = %+v, want agent refs after worker recovery", loaded)
	}
	stored := waitForWorkflowLeaseCleared(t, reopened.workflows, "wf-queued-recover")
	if stored.Status != workflowkit.StatusWaitingApproval {
		t.Fatalf("recovered workflow after release = %+v, want waiting approval", stored)
	}
}

func TestHostAPIQueuedWorkerStatusTracksCompletedRun(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server.StartQueuedWorker(ctx)

	queued := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{
		"id":       "wf-worker-status",
		"input":    "Review worker observability.",
		"run_mode": "queued",
	})
	if queued.Status != string(workflowkit.StatusPending) {
		t.Fatalf("queued response = %+v, want pending", queued)
	}
	waitForWorkflowStatus(t, server.Handler(), "wf-worker-status", workflowkit.StatusWaitingApproval)
	waitForWorkflowLeaseCleared(t, server.workflows, "wf-worker-status")

	status := waitForQueuedWorkerStatus(t, server.Handler(), func(status queuedWorkerStatusResponse) bool {
		return status.Started &&
			status.WorkerID == queuedWorkerID &&
			status.Claimed >= 1 &&
			status.Completed >= 1 &&
			status.LastWorkflowID == "wf-worker-status"
	})
	if status.Errors != 0 || status.LastError != "" {
		t.Fatalf("worker status = %+v, want no errors", status)
	}
}

func TestHostAPIQueuedWorkerExtendsLeaseWhileWorkflowRuns(t *testing.T) {
	t.Setenv("HOST_API_QUEUED_LEASE_DURATION", "40ms")

	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	server.executor = workflowkit.NewExecutor(server.workflows, []workflowkit.Step{
		slowApprovalStep{delay: 500 * time.Millisecond},
	})

	queued := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{
		"id":       "wf-worker-heartbeat",
		"input":    "Review worker heartbeat.",
		"run_mode": "queued",
	})
	if queued.Status != string(workflowkit.StatusPending) {
		t.Fatalf("queued response = %+v, want pending", queued)
	}

	claimed := waitForWorkflowLeasePresent(t, server.workflows, "wf-worker-heartbeat")
	extended := waitForWorkflowLeaseExtendedAfter(t, server.workflows, "wf-worker-heartbeat", claimed.LeaseUntil)
	if !extended.LeaseUntil.After(claimed.LeaseUntil) {
		t.Fatalf("extended lease = %+v, want after %s", extended, claimed.LeaseUntil)
	}

	status := waitForQueuedWorkerStatus(t, server.Handler(), func(status queuedWorkerStatusResponse) bool {
		return status.LeaseExtensions >= 1 && status.LastHeartbeatWorkflowID == "wf-worker-heartbeat"
	})
	if status.HeartbeatErrors != 0 || status.LastHeartbeatError != "" {
		t.Fatalf("worker status = %+v, want successful heartbeat", status)
	}

	waitForWorkflowStatus(t, server.Handler(), "wf-worker-heartbeat", workflowkit.StatusWaitingApproval)
	waitForWorkflowLeaseCleared(t, server.workflows, "wf-worker-heartbeat")
}

func TestHostAPIQueuedWorkerStatusTracksRunError(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	if err := server.workflows.Save(context.Background(), workflowkit.WorkflowRun{
		ID:       "wf-worker-error",
		Status:   workflowkit.StatusPending,
		InputRef: "artifact:wf-worker-error:missing",
	}); err != nil {
		t.Fatalf("Save pending workflow returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server.StartQueuedWorker(ctx)

	waitForWorkflowStatus(t, server.Handler(), "wf-worker-error", workflowkit.StatusFailed)
	status := waitForQueuedWorkerStatus(t, server.Handler(), func(status queuedWorkerStatusResponse) bool {
		return status.Errors >= 1 &&
			status.LastError != "" &&
			status.LastErrorWorkflowID == "wf-worker-error"
	})
	if status.Completed != 0 {
		t.Fatalf("worker status = %+v, want no completed runs after workflow error", status)
	}
}

func TestHostAPIRequeuesFailedWorkflow(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	inputRef := "artifact:wf-requeue-failed:input"
	if err := server.workflows.Save(context.Background(), workflowkit.WorkflowRun{
		ID:       "wf-requeue-failed",
		Status:   workflowkit.StatusPending,
		InputRef: inputRef,
		Metadata: map[string]any{
			"input_ref": inputRef,
			"run_mode":  string(RunModeQueued),
		},
	}); err != nil {
		t.Fatalf("Save pending workflow returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server.StartQueuedWorker(ctx)
	failed := waitForWorkflowStatus(t, server.Handler(), "wf-requeue-failed", workflowkit.StatusFailed)
	if failed.RunMode != string(RunModeQueued) {
		t.Fatalf("failed workflow = %+v, want queued workflow", failed)
	}
	storedFailed, err := server.workflows.Get(context.Background(), "wf-requeue-failed")
	if err != nil {
		t.Fatalf("Get failed workflow returned error: %v", err)
	}
	if storedFailed.Error == "" {
		t.Fatalf("failed workflow = %+v, want stored error", storedFailed)
	}

	if err := putTextArtifact(context.Background(), server.artifacts, inputRef, "Review requeued workflow."); err != nil {
		t.Fatalf("put input artifact returned error: %v", err)
	}
	requeued := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows/wf-requeue-failed/requeue", nil)
	if requeued.Status != string(workflowkit.StatusPending) || requeued.RunMode != string(RunModeQueued) {
		t.Fatalf("requeue response = %+v, want queued pending workflow", requeued)
	}

	loaded := waitForWorkflowStatus(t, server.Handler(), "wf-requeue-failed", workflowkit.StatusWaitingApproval)
	if loaded.AgentRunID == "" || loaded.OutputRef == "" || loaded.ApprovalRef == "" {
		t.Fatalf("requeued workflow = %+v, want agent refs after worker retry", loaded)
	}
	waitForWorkflowLeaseCleared(t, server.workflows, "wf-requeue-failed")
}

func TestHostAPIWorkflowEventsExposeStepFailureAndRequeue(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	inputRef := "artifact:wf-events:input"
	if err := server.workflows.Save(context.Background(), workflowkit.WorkflowRun{
		ID:       "wf-events",
		Status:   workflowkit.StatusPending,
		InputRef: inputRef,
		Metadata: map[string]any{
			"input_ref": inputRef,
			"run_mode":  string(RunModeQueued),
		},
	}); err != nil {
		t.Fatalf("Save pending workflow returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	server.StartQueuedWorker(ctx)
	waitForWorkflowStatus(t, server.Handler(), "wf-events", workflowkit.StatusFailed)

	if err := putTextArtifact(context.Background(), server.artifacts, inputRef, "Review workflow events."); err != nil {
		t.Fatalf("put input artifact returned error: %v", err)
	}
	doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows/wf-events/requeue", nil)
	waitForWorkflowStatus(t, server.Handler(), "wf-events", workflowkit.StatusWaitingApproval)

	events := doJSON[workflowEventsResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-events/events", nil)
	if events.WorkflowID != "wf-events" || events.Status != string(workflowkit.StatusWaitingApproval) || events.RunMode != string(RunModeQueued) {
		t.Fatalf("events response = %+v, want queued waiting workflow", events)
	}
	if !workflowEventsContain(events.Events, func(event workflowEventResponse) bool {
		return event.Type == "step" &&
			event.Name == "ingest" &&
			event.Status == string(workflowkit.StatusFailed) &&
			event.Error != ""
	}) {
		t.Fatalf("events = %+v, want failed ingest step event", events.Events)
	}
	if !workflowEventsContain(events.Events, func(event workflowEventResponse) bool {
		return event.Type == "workflow_requeued" &&
			event.FromStatus == string(workflowkit.StatusFailed) &&
			event.ToStatus == string(workflowkit.StatusPending) &&
			!event.At.IsZero()
	}) {
		t.Fatalf("events = %+v, want workflow_requeued event", events.Events)
	}
}

func TestHostAPIListsWorkflowsByStatusAndLimit(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	for _, run := range []workflowkit.WorkflowRun{
		{
			ID:        "wf-list-queued",
			Status:    workflowkit.StatusPending,
			InputRef:  "artifact:wf-list-queued:input",
			CreatedAt: base,
			Metadata: map[string]any{
				"run_mode": string(RunModeQueued),
			},
		},
		{
			ID:        "wf-list-sync",
			Status:    workflowkit.StatusWaitingApproval,
			InputRef:  "artifact:wf-list-sync:input",
			CreatedAt: base.Add(time.Minute),
			Metadata: map[string]any{
				"run_mode": string(RunModeSync),
			},
		},
	} {
		if err := server.workflows.Save(context.Background(), run); err != nil {
			t.Fatalf("Save(%s) returned error: %v", run.ID, err)
		}
	}

	pending := doJSON[workflowListResponse](t, server.Handler(), http.MethodGet, "/workflows?status=pending&limit=1", nil)
	if len(pending.Workflows) != 1 {
		t.Fatalf("pending workflows = %+v, want one result", pending.Workflows)
	}
	if pending.Workflows[0].ID != "wf-list-queued" || pending.Workflows[0].RunMode != string(RunModeQueued) {
		t.Fatalf("pending workflow = %+v, want queued workflow with persisted run mode", pending.Workflows[0])
	}

	waiting := doJSON[workflowListResponse](t, server.Handler(), http.MethodGet, "/workflows?status=waiting_approval", nil)
	if got := workflowResponseIDs(waiting.Workflows); !containsString(got, "wf-list-sync") {
		t.Fatalf("waiting workflows ids = %v, want wf-list-sync", got)
	}
}

func TestHostAPIListsWorkflowsByRunModeAndDescendingOrder(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	base := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	for _, run := range []workflowkit.WorkflowRun{
		{
			ID:        "wf-list-sync-new",
			Status:    workflowkit.StatusPending,
			InputRef:  "artifact:wf-list-sync-new:input",
			CreatedAt: base.Add(3 * time.Minute),
			Metadata: map[string]any{
				"run_mode": string(RunModeSync),
			},
		},
		{
			ID:        "wf-list-queued-old",
			Status:    workflowkit.StatusPending,
			InputRef:  "artifact:wf-list-queued-old:input",
			CreatedAt: base,
			Metadata: map[string]any{
				"run_mode": string(RunModeQueued),
			},
		},
		{
			ID:        "wf-list-queued-new",
			Status:    workflowkit.StatusPending,
			InputRef:  "artifact:wf-list-queued-new:input",
			CreatedAt: base.Add(2 * time.Minute),
			Metadata: map[string]any{
				"run_mode": string(RunModeQueued),
			},
		},
	} {
		if err := server.workflows.Save(context.Background(), run); err != nil {
			t.Fatalf("Save(%s) returned error: %v", run.ID, err)
		}
	}

	listed := doJSON[workflowListResponse](t, server.Handler(), http.MethodGet, "/workflows?status=pending&run_mode=queued&order=desc&limit=1", nil)
	if len(listed.Workflows) != 1 {
		t.Fatalf("listed workflows = %+v, want one result", listed.Workflows)
	}
	if listed.Workflows[0].ID != "wf-list-queued-new" || listed.Workflows[0].RunMode != string(RunModeQueued) {
		t.Fatalf("listed workflow = %+v, want newest queued workflow", listed.Workflows[0])
	}
}

func TestHostAPIReturnsWorkflowLLMRouteAudit(t *testing.T) {
	server, err := NewServer(Config{
		RuntimeHome: t.TempDir(),
		ApprovalAuthenticator: testApprovalAuthenticator{
			identity: ApprovalIdentity{Subject: "operator-routes"},
		},
	})
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

	approved := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows/wf-routes-1/approve", map[string]string{"note": "accepted"})
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
	server, err := NewServer(Config{
		LLMKitHome: t.TempDir(),
		ApprovalAuthenticator: testApprovalAuthenticator{
			identity: ApprovalIdentity{Subject: "operator-queued"},
		},
	})
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
	if loaded.RunMode != string(RunModeQueued) {
		t.Fatalf("queued loaded run mode = %q, want queued", loaded.RunMode)
	}
	stored := waitForWorkflowLeaseCleared(t, server.workflows, "wf-queued")
	if stored.Status != workflowkit.StatusWaitingApproval {
		t.Fatalf("queued workflow after release = %+v, want waiting approval", stored)
	}
	routes := doJSON[llmRoutesResponse](t, server.Handler(), http.MethodGet, "/workflows/wf-queued/llm-routes", nil)
	if got := selectedModelAlias(t, routes); got != "local-free" {
		t.Fatalf("queued selected model = %q, want local-free; routes=%+v", got, routes.Routes)
	}

	approved := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows/wf-queued/approve", map[string]string{"note": "accepted queued"})
	if approved.Status != string(workflowkit.StatusSucceeded) {
		t.Fatalf("queued approval = %+v, want succeeded", approved)
	}
	if approved.RunMode != string(RunModeQueued) {
		t.Fatalf("queued approved run mode = %q, want queued", approved.RunMode)
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

func waitForWorkflowLeaseCleared(t *testing.T, store workflowkit.Store, id string) workflowkit.WorkflowRun {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("workflow store Get returned error: %v", err)
		}
		if run.LeaseOwner == "" && run.LeaseUntil.IsZero() {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("workflow store Get returned error: %v", err)
	}
	t.Fatalf("workflow lease = %+v, want cleared lease", run)
	return workflowkit.WorkflowRun{}
}

type queuedWorkerStatusResponse struct {
	Started                 bool   `json:"started"`
	WorkerID                string `json:"worker_id"`
	ClaimAttempts           int    `json:"claim_attempts"`
	Claimed                 int    `json:"claimed"`
	Completed               int    `json:"completed"`
	Idle                    int    `json:"idle"`
	Errors                  int    `json:"errors"`
	LeaseExtensions         int    `json:"lease_extensions"`
	HeartbeatErrors         int    `json:"heartbeat_errors"`
	LastWorkflowID          string `json:"last_workflow_id,omitempty"`
	LastError               string `json:"last_error,omitempty"`
	LastErrorWorkflowID     string `json:"last_error_workflow_id,omitempty"`
	LastHeartbeatWorkflowID string `json:"last_heartbeat_workflow_id,omitempty"`
	LastHeartbeatError      string `json:"last_heartbeat_error,omitempty"`
}

func workflowEventsContain(events []workflowEventResponse, accept func(workflowEventResponse) bool) bool {
	for _, event := range events {
		if accept(event) {
			return true
		}
	}
	return false
}

func workflowResponseIDs(workflows []workflowResponse) []string {
	ids := make([]string, 0, len(workflows))
	for _, workflow := range workflows {
		ids = append(ids, workflow.ID)
	}
	return ids
}

func waitForQueuedWorkerStatus(t *testing.T, handler http.Handler, accept func(queuedWorkerStatusResponse) bool) queuedWorkerStatusResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last queuedWorkerStatusResponse
	for time.Now().Before(deadline) {
		last = doJSON[queuedWorkerStatusResponse](t, handler, http.MethodGet, "/workers/queued", nil)
		if accept(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queued worker status = %+v, did not satisfy condition", last)
	return queuedWorkerStatusResponse{}
}

type slowApprovalStep struct {
	delay time.Duration
}

func (s slowApprovalStep) Name() string {
	return "slow_approval"
}

func (s slowApprovalStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return workflowkit.StepResult{}, ctx.Err()
	case <-timer.C:
	}
	return workflowkit.StepResult{
		Status:        workflowkit.StatusWaitingApproval,
		ApprovalRef:   "approval:" + run.ID,
		WaitingReason: "operator approval required after slow review",
	}, nil
}

func waitForWorkflowLeasePresent(t *testing.T, store workflowkit.Store, id string) workflowkit.WorkflowRun {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("workflow store Get returned error: %v", err)
		}
		if run.LeaseOwner != "" && !run.LeaseUntil.IsZero() {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("workflow store Get returned error: %v", err)
	}
	t.Fatalf("workflow lease = %+v, want active lease", run)
	return workflowkit.WorkflowRun{}
}

func waitForWorkflowLeaseExtendedAfter(t *testing.T, store workflowkit.Store, id string, previous time.Time) workflowkit.WorkflowRun {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("workflow store Get returned error: %v", err)
		}
		if run.LeaseOwner != "" && run.LeaseUntil.After(previous) {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("workflow store Get returned error: %v", err)
	}
	t.Fatalf("workflow lease = %+v, want lease extended after %s", run, previous)
	return workflowkit.WorkflowRun{}
}

func closeStoreIfPossible(t *testing.T, store any) {
	t.Helper()
	closer, ok := store.(interface{ Close() error })
	if !ok {
		return
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
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
