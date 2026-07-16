package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/evalkit"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/skillkit"
	"github.com/eruca/goagents/workflowkit"
)

func TestHostEvalSuiteUsesPersistedTrajectoryAndOutcome(t *testing.T) {
	skillRoot := t.TempDir()
	writeHostAPISkill(t, skillRoot, "trajectory-review", "---\nname: trajectory-review\ndescription: Exercise the persisted trajectory adapter.\n---\n# Instructions\nReview the workflow without expanding capabilities.\n", nil)
	catalog, err := skillkit.Discover([]skillkit.Root{{
		ID:      "trajectory-skills",
		Dir:     skillRoot,
		Scope:   skillkit.ScopeBuiltin,
		Trusted: true,
		Enabled: true,
	}})
	if err != nil {
		t.Fatalf("discover Skill: %v", err)
	}

	server, err := NewServer(Config{
		RuntimeHome:           t.TempDir(),
		SkillCatalog:          catalog,
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-eval"}},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		closeStoreIfPossible(t, server.workflows)
		closeStoreIfPossible(t, server.runs)
	})
	server.providers["local-free"] = &skillEvalProvider{}

	const privateInput = "PRIVATE WORKFLOW INPUT MUST NOT ENTER TRACE"
	created := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-trajectory-eval",
		"input": privateInput,
		"skill_refs": []map[string]string{{
			"name": "trajectory-review",
		}},
	})
	if created.Status != string(workflowkit.StatusWaitingApproval) || len(created.SkillRefs) != 1 {
		t.Fatalf("created workflow = %#v, want final approval with one resolved Skill", created)
	}

	approvedResponse := approveWorkflowRequestForTest(t, server.Handler(), created.ID, map[string]string{
		"note": "accept eval output",
	}, "Bearer test-operator")
	if approvedResponse.Code != http.StatusOK {
		t.Fatalf("approve workflow status = %d; body=%s", approvedResponse.Code, approvedResponse.Body.String())
	}
	var approved workflowResponse
	if err := json.NewDecoder(approvedResponse.Body).Decode(&approved); err != nil {
		t.Fatalf("decode approved workflow: %v", err)
	}
	if approved.Status != string(workflowkit.StatusSucceeded) || approved.OutputRef == "" {
		t.Fatalf("approved workflow = %#v, want succeeded output", approved)
	}

	runs, err := server.runs.FindByWorkflowID(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("find persisted agent runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("persisted agent runs = %d, want 1", len(runs))
	}
	const privateEventMessage = "PRIVATE EVENT MESSAGE MUST NOT ENTER TRACE"
	const privateToolInput = "PRIVATE TOOL INPUT MUST NOT ENTER TRACE"
	if err := server.runs.AppendEvent(t.Context(), runkit.RunEvent{
		RunID:    runs[0].RunID,
		Type:     "tool.completed",
		Stage:    "eval-probe",
		Message:  privateEventMessage,
		Metadata: map[string]any{"tool": "lookup", "ref": "artifact:safe", "input": privateToolInput, "secret": "private-value"},
	}); err != nil {
		t.Fatalf("append persisted sanitization probe: %v", err)
	}

	fingerprint := hostEvalFingerprint{
		GitCommit:           "test-commit",
		Provider:            "local",
		ModelAlias:          "local-free",
		AgentDefinitionHash: "agent-definition-v1",
		PromptVersion:       "prompt-v1",
		VisibleToolIDs:      []string{"record_review", "artifact.read", "record_review"},
	}
	runner := evalkit.Runner{Harness: evalkit.HarnessFunc(func(ctx context.Context, _ evalkit.Task) (*evalkit.RunResult, error) {
		return buildHostEvalResult(ctx, created.ID, server.workflows, server.runs, fingerprint)
	})}
	suite := evalkit.Suite{
		Name: "host-persisted-trajectory",
		Tasks: []evalkit.Task{{
			ID:              "persisted-workflow-held-out",
			SuccessCriteria: "The persisted workflow outcome, policy trajectory, and usage remain within contract.",
			Metadata: map[string]any{
				"dataset_split":  "held_out",
				"max_llm_calls":  1,
				"max_tool_calls": 0,
			},
		}},
		Graders: []evalkit.Grader{
			outcomeContractGrader(approved.OutputRef),
			trajectoryPolicyGrader(created.SkillRefs[0], privateInput, privateEventMessage, privateToolInput, "private-value", skillRoot),
			efficiencyBudgetGrader(),
		},
	}

	result, err := runner.Run(t.Context(), suite)
	if err != nil {
		t.Fatalf("run persisted trajectory eval: %v", err)
	}
	if result.Summary.TotalTrials != 1 || result.Summary.PassedTrials != 1 || result.Summary.GraderCount != 3 {
		t.Fatalf("eval summary = %+v; trials=%+v", result.Summary, result.Trials)
	}
	trial := result.Trials[0]
	if len(trial.Grades) != 3 || trial.Grades[0].Name != "outcome-contract" || trial.Grades[1].Name != "trajectory-policy" || trial.Grades[2].Name != "efficiency-budget" {
		t.Fatalf("grades = %+v, want separate outcome/policy/efficiency graders", trial.Grades)
	}
}

func TestBuildHostEvalResultRequiresStores(t *testing.T) {
	if _, err := buildHostEvalResult(t.Context(), "wf-1", nil, nil, hostEvalFingerprint{}); err == nil || !strings.Contains(err.Error(), "workflow store") {
		t.Fatalf("missing workflow store error = %v", err)
	}
	if _, err := buildHostEvalResult(t.Context(), "wf-1", workflowkit.NewMemoryStore(), nil, hostEvalFingerprint{}); err == nil || !strings.Contains(err.Error(), "run store") {
		t.Fatalf("missing run store error = %v", err)
	}
}

func TestNormalizedAgentEventStatus(t *testing.T) {
	tests := []struct {
		eventType string
		want      string
	}{
		{eventType: "stage.started", want: "running"},
		{eventType: "tool.completed", want: "succeeded"},
		{eventType: "finalized", want: "succeeded"},
		{eventType: "output.validated", want: "succeeded"},
		{eventType: "stage.failed", want: "failed"},
		{eventType: "approval.denied", want: "failed"},
		{eventType: "input.rejected", want: "failed"},
		{eventType: "approval.pending", want: "waiting_approval"},
		{eventType: "approval.requested", want: "waiting_approval"},
		{eventType: "memory.loaded", want: ""},
	}
	for _, test := range tests {
		t.Run(test.eventType, func(t *testing.T) {
			if got := normalizedAgentEventStatus(test.eventType); got != test.want {
				t.Fatalf("normalized status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWorkflowEvalErrorCode(t *testing.T) {
	if got := workflowEvalErrorCode(workflowkit.StatusFailed); got != "workflow_failed" {
		t.Fatalf("failed error code = %q", got)
	}
	if got := workflowEvalErrorCode(workflowkit.StatusCancelled); got != "workflow_cancelled" {
		t.Fatalf("cancelled error code = %q", got)
	}
	if got := workflowEvalErrorCode(workflowkit.StatusSucceeded); got != "" {
		t.Fatalf("succeeded error code = %q, want empty", got)
	}
}

func TestSortTraceStepsOrdersTimedBeforeUntimedAndPreservesTies(t *testing.T) {
	base := time.Date(2026, 7, 14, 2, 0, 0, 0, time.UTC)
	steps := []evalkit.TraceStep{
		{Name: "untimed-first"},
		{Name: "late", StartedAt: base.Add(time.Minute)},
		{Name: "same-first", StartedAt: base},
		{Name: "untimed-second"},
		{Name: "same-second", StartedAt: base},
	}

	sortTraceSteps(steps)
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.Name)
	}
	if got := strings.Join(names, ","); got != "same-first,same-second,late,untimed-first,untimed-second" {
		t.Fatalf("sorted steps = %q", got)
	}
}

func outcomeContractGrader(outputRef string) evalkit.Grader {
	return evalkit.GraderFunc{
		GraderName: "outcome-contract",
		Fn: func(_ context.Context, req evalkit.GradeRequest) (*evalkit.GradeResult, error) {
			return &evalkit.GradeResult{Assertions: []evalkit.Assertion{
				{Name: "succeeded", Passed: req.Trial.Outcome.Status == string(workflowkit.StatusSucceeded)},
				{Name: "output-ref", Passed: req.Trial.Outcome.OutputRef == outputRef},
				{Name: "no-error-code", Passed: req.Trial.Outcome.ErrorCode == ""},
			}}, nil
		},
	}
}

func trajectoryPolicyGrader(skillRef workflowSkillRef, secrets ...string) evalkit.Grader {
	return evalkit.GraderFunc{
		GraderName: "trajectory-policy",
		Fn: func(_ context.Context, req evalkit.GradeRequest) (*evalkit.GradeResult, error) {
			encoded, err := json.Marshal(req.Trial.Trace)
			if err != nil {
				return nil, err
			}
			traceJSON := string(encoded)
			secretsAbsent := true
			for _, secret := range secrets {
				secretsAbsent = secretsAbsent && !strings.Contains(traceJSON, secret)
			}
			return &evalkit.GradeResult{Assertions: []evalkit.Assertion{
				{Name: "held-out", Passed: req.Task.Metadata["dataset_split"] == "held_out"},
				{Name: "workflow-steps", Passed: traceHasStepType(req.Trial.Trace, "workflow_step")},
				{Name: "agent-events", Passed: traceHasStepType(req.Trial.Trace, "agent_event")},
				{Name: "agent-event-order", Passed: agentEventSequencesOrdered(req.Trial.Trace)},
				{Name: "git-commit", Passed: req.Trial.Trace.Labels["git.commit"] == "test-commit"},
				{Name: "provider", Passed: req.Trial.Trace.Labels["provider"] == "local"},
				{Name: "model", Passed: req.Trial.Trace.Labels["model.alias"] == "local-free"},
				{Name: "definition", Passed: req.Trial.Trace.Labels["agent.definition_hash"] == "agent-definition-v1"},
				{Name: "prompt", Passed: req.Trial.Trace.Labels["prompt.version"] == "prompt-v1"},
				{Name: "skill-ref", Passed: req.Trial.Trace.Labels["skill.refs"] == skillRef.Name+"@"+skillRef.Digest},
				{Name: "visible-tools", Passed: req.Trial.Trace.Labels["tools.visible"] == "artifact.read,record_review"},
				{Name: "secrets-absent", Passed: secretsAbsent},
				{Name: "metadata-allowlisted", Passed: traceMetadataIsAllowlisted(req.Trial.Trace)},
			}}, nil
		},
	}
}

func efficiencyBudgetGrader() evalkit.Grader {
	return evalkit.GraderFunc{
		GraderName: "efficiency-budget",
		Fn: func(_ context.Context, req evalkit.GradeRequest) (*evalkit.GradeResult, error) {
			maxLLMCalls, _ := req.Task.Metadata["max_llm_calls"].(int)
			maxToolCalls, _ := req.Task.Metadata["max_tool_calls"].(int)
			return &evalkit.GradeResult{Assertions: []evalkit.Assertion{
				{Name: "llm-calls", Passed: req.Trial.Trace.Usage.LLMCalls <= maxLLMCalls},
				{Name: "tool-calls", Passed: req.Trial.Trace.Usage.ToolCalls <= maxToolCalls},
				{Name: "non-negative-tokens", Passed: req.Trial.Trace.Usage.InputTokens >= 0 && req.Trial.Trace.Usage.OutputTokens >= 0},
			}}, nil
		},
	}
}

func traceHasStepType(trace evalkit.Trace, stepType string) bool {
	for _, step := range trace.Steps {
		if step.Type == stepType {
			return true
		}
	}
	return false
}

func agentEventSequencesOrdered(trace evalkit.Trace) bool {
	last := 0
	for _, step := range trace.Steps {
		if step.Type != "agent_event" {
			continue
		}
		sequence, _ := step.Metadata["sequence"].(int)
		if sequence <= last {
			return false
		}
		last = sequence
	}
	return last > 0
}

func traceMetadataIsAllowlisted(trace evalkit.Trace) bool {
	workflowKeys := map[string]bool{"attempt": true, "output_ref": true, "agent_run_id": true, "audit_ref": true, "approval_ref": true}
	agentKeys := map[string]bool{"sequence": true, "stage": true, "iteration": true, "tool": true, "ref": true}
	for _, step := range trace.Steps {
		allowed := agentKeys
		if step.Type == "workflow_step" {
			allowed = workflowKeys
		}
		for key := range step.Metadata {
			if !allowed[key] {
				return false
			}
		}
	}
	return true
}
