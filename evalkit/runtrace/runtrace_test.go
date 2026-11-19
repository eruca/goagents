package runtrace_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/evalkit"
	"github.com/eruca/goagents/evalkit/runtrace"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
	"github.com/eruca/goagents/llmkit"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
)

var base = time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)

func at(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }

func event(seq int, eventType string, iteration int, seconds int, metadata map[string]any) runkit.RunEvent {
	return runkit.RunEvent{RunID: "run-1", Sequence: seq, Type: eventType, Iteration: iteration, RecordedAt: at(seconds), Metadata: metadata}
}

func summarize(steps []evalkit.TraceStep) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.Type+":"+step.Name+":"+step.Status)
	}
	return out
}

func TestEventStepsPairsToolCallsAndApprovals(t *testing.T) {
	events := []runkit.RunEvent{
		event(1, "stage.started", 1, 0, nil),
		event(2, "tool.rejected", 1, 1, map[string]any{"tool": "unknown"}),
		event(3, "approval.requested", 2, 2, map[string]any{"tool": "delete", "index": 0, "reason": "PRIVATE REASON"}),
		event(4, "approval.pending", 2, 3, map[string]any{"tool": "delete", "index": 0}),
		// Resumed later: the resolution closes the request recorded before it,
		// and index arrives as a JSON number from the store.
		event(5, "approval.completed", 2, 60, map[string]any{"tool": "delete", "index": float64(0)}),
		event(6, "tool.started", 2, 61, map[string]any{"tool": "search"}),
		event(7, "tool.started", 2, 61, map[string]any{"tool": "delete"}),
		event(8, "tool.completed", 2, 63, map[string]any{"tool": "delete", "index": float64(1), "ref": "artifact:out", "input": "PRIVATE INPUT"}),
		event(9, "tool.completed", 2, 64, map[string]any{"tool": "search", "index": 0, "is_error": true}),
		event(10, "tool.started", 3, 70, map[string]any{"tool": "fetch"}),
		event(11, "tool.failed", 3, 71, map[string]any{"tool": "fetch", "index": 0}),
		event(12, "tool.completed", 3, 72, map[string]any{"tool": "orphan", "index": 4}),
		event(13, "approval.denied", 3, 73, map[string]any{"tool": "drop", "index": 0}),
		event(14, "finalized", 3, 74, nil),
	}
	events[0].Message = "PRIVATE MESSAGE"
	steps := runtrace.EventSteps(events)
	want := []string{
		"agent_event:stage.started:running",
		"tool_call:unknown:rejected",
		"approval:delete:succeeded",
		"tool_call:search:failed",
		"tool_call:delete:succeeded",
		"tool_call:fetch:failed",
		"tool_call:orphan:succeeded",
		"approval:drop:rejected",
		"agent_event:finalized:succeeded",
	}
	if got := summarize(steps); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	approval := steps[2]
	if !approval.StartedAt.Equal(at(2)) || !approval.EndedAt.Equal(at(60)) {
		t.Fatalf("approval span = %s..%s", approval.StartedAt, approval.EndedAt)
	}
	deleteCall := steps[4]
	if !deleteCall.StartedAt.Equal(at(61)) || !deleteCall.EndedAt.Equal(at(63)) || deleteCall.Metadata["ref"] != "artifact:out" || deleteCall.Metadata["index"] != 1 {
		t.Fatalf("delete call = %+v", deleteCall)
	}
	encoded, _ := json.Marshal(steps)
	for _, secret := range []string{"PRIVATE", "reason", "input"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("trace contains %q: %s", secret, encoded)
		}
	}
}

func TestEventStepsKeepsUnfinishedCallsRunning(t *testing.T) {
	steps := runtrace.EventSteps([]runkit.RunEvent{
		event(1, "tool.started", 1, 0, map[string]any{"tool": "search"}),
		event(2, "approval.requested", 1, 1, map[string]any{"tool": "write", "index": 0}),
	})
	if got := strings.Join(summarize(steps), ","); got != "tool_call:search:running,approval:write:waiting_approval" {
		t.Fatalf("steps = %s", got)
	}
}

func TestModelCallSteps(t *testing.T) {
	steps := runtrace.ModelCallSteps([]llmkit.RouteAuditRecord{
		{Route: llmkit.RouteTrace{RouteID: "r1", Attempt: 1, ModelAlias: "fast", Provider: "p", Selected: true, RecordedAt: at(1)},
			Outcome: &llmkit.TaskOutcome{Success: false, ErrorClass: llmkit.ErrorClassTimeout, FailureReason: "PRIVATE PROVIDER ERROR", LatencyMillis: 900, RecordedAt: at(2)}},
		{Route: llmkit.RouteTrace{RouteID: "r1", Attempt: 2, ModelAlias: "strong", Selected: true, RecordedAt: at(3)},
			Outcome: &llmkit.TaskOutcome{Success: true, InputTokens: 10, OutputTokens: 4, RecordedAt: at(5)}},
		{Route: llmkit.RouteTrace{RouteID: "r2", ModelAlias: "none", Selected: false}},
		{Route: llmkit.RouteTrace{RouteID: "r3", ModelAlias: "lost", Selected: true, RecordedAt: at(6)}},
	})
	if got := strings.Join(summarize(steps), ","); got != "model_call:fast:failed,model_call:strong:succeeded,model_call:lost:running" {
		t.Fatalf("steps = %s", got)
	}
	if steps[0].Metadata["error_class"] != "timeout" || steps[0].Metadata["attempt"] != 1 || !steps[1].EndedAt.Equal(at(5)) {
		t.Fatalf("metadata = %+v / %+v", steps[0], steps[1])
	}
	encoded, _ := json.Marshal(steps)
	if strings.Contains(string(encoded), "PRIVATE") {
		t.Fatalf("model steps leak failure reason: %s", encoded)
	}
}

type fakeAudit struct {
	records map[string][]llmkit.RouteAuditRecord
	err     error
}

func (a fakeAudit) ReadRouteAudits(_ context.Context, filter llmkit.AuditFilter) ([]llmkit.RouteAuditRecord, error) {
	return a.records[filter.TaskID], a.err
}

func TestBuildWorkflow(t *testing.T) {
	ctx := context.Background()
	workflows := workflowkit.NewMemoryStore()
	if err := workflows.Create(ctx, workflowkit.WorkflowRun{
		ID:        "wf-1",
		Status:    workflowkit.StatusFailed,
		OutputRef: "artifact:wf-1:output",
		Error:     "PRIVATE WORKFLOW ERROR",
		StepRecords: []workflowkit.StepRecord{{
			Name: "review", Status: workflowkit.StatusFailed, Attempt: 2, AgentRunID: "run-1",
			Error: "PRIVATE STEP ERROR", StartedAt: at(0), EndedAt: at(10),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	runs := runkit.NewMemoryStore()
	if err := runs.Create(ctx, runkit.RunRecord{RunID: "run-1", WorkflowID: "wf-1", Status: runkit.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []runkit.RunEvent{
		event(0, "tool.started", 1, 2, map[string]any{"tool": "search"}),
		event(0, "tool.completed", 1, 3, map[string]any{"tool": "search", "index": 0}),
	} {
		if err := runs.AppendEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := runs.Complete(ctx, "run-1", runkit.TerminalSummary{Status: runkit.StatusFailed, InputTokens: 7, OutputTokens: 3, LLMCalls: 2, ToolCalls: 1}); err != nil {
		t.Fatal(err)
	}
	audit := fakeAudit{records: map[string][]llmkit.RouteAuditRecord{"wf-1": {
		{Route: llmkit.RouteTrace{ModelAlias: "fast", Selected: true, RecordedAt: at(1)}, Outcome: &llmkit.TaskOutcome{Success: true, RecordedAt: at(2)}},
	}}}
	src := runtrace.Sources{Workflows: workflows, Runs: runs, Audit: audit}

	result, err := runtrace.BuildWorkflow(ctx, src, "wf-1", runtrace.Fingerprint{GitCommit: "abc", VisibleToolIDs: []string{"search", "fetch", "search"}})
	if err != nil {
		t.Fatal(err)
	}
	if o := result.Outcome; o.Status != "failed" || o.OutputRef != "artifact:wf-1:output" || o.ErrorCode != "workflow_failed" || o.Metadata != nil {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
	if got := strings.Join(summarize(result.Trace.Steps), ","); got != "workflow_step:review:failed,model_call:fast:succeeded,tool_call:search:succeeded" {
		t.Fatalf("steps = %s", got)
	}
	if result.Trace.Usage != (evalkit.Usage{InputTokens: 7, OutputTokens: 3, LLMCalls: 2, ToolCalls: 1}) {
		t.Fatalf("usage = %+v", result.Trace.Usage)
	}
	if result.Trace.Labels["git.commit"] != "abc" || result.Trace.Labels["tools.visible"] != "fetch,search" || len(result.Trace.Labels) != 2 {
		t.Fatalf("labels = %+v", result.Trace.Labels)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "PRIVATE") {
		t.Fatalf("workflow trace leaks errors: %s", encoded)
	}

	withoutAudit, err := runtrace.BuildWorkflow(ctx, runtrace.Sources{Workflows: workflows, Runs: runs}, "wf-1", runtrace.Fingerprint{})
	if err != nil || len(evalkit.ModelCalls(withoutAudit.Trace)) != 0 || withoutAudit.Trace.Labels != nil {
		t.Fatalf("without audit = %+v, %v", withoutAudit, err)
	}
	if _, err := runtrace.BuildWorkflow(ctx, runtrace.Sources{Workflows: workflows, Runs: runs, Audit: fakeAudit{err: errors.New("down")}}, "wf-1", runtrace.Fingerprint{}); err == nil {
		t.Fatal("audit error was ignored")
	}
	if _, err := runtrace.BuildWorkflow(ctx, runtrace.Sources{Runs: runs}, "wf-1", runtrace.Fingerprint{}); err == nil || !strings.Contains(err.Error(), "workflow store") {
		t.Fatalf("missing workflow store error = %v", err)
	}
	if _, err := runtrace.BuildWorkflow(ctx, runtrace.Sources{Workflows: workflows}, "wf-1", runtrace.Fingerprint{}); err == nil || !strings.Contains(err.Error(), "run store") {
		t.Fatalf("missing run store error = %v", err)
	}
	if _, err := runtrace.BuildWorkflow(ctx, src, "wf-missing", runtrace.Fingerprint{}); err == nil {
		t.Fatal("missing workflow was accepted")
	}
}

type scriptedLLM struct {
	mu        sync.Mutex
	responses []*ports.ChatResponse
}

func (l *scriptedLLM) Chat(context.Context, ports.ChatRequest) (*ports.ChatResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	response := l.responses[0]
	l.responses = l.responses[1:]
	return response, nil
}

type funcTool struct {
	spec tools.Spec
	run  func() (*tools.Result, error)
}

func (t funcTool) Spec() tools.Spec { return t.spec }

func (t funcTool) Execute(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
	return t.run()
}

type allowAll struct{}

func (allowAll) ApproveTool(context.Context, agentcore.ToolApprovalRequest) agentcore.ToolApprovalDecision {
	return agentcore.ToolApprovalDecision{Allowed: true, Reason: "PRIVATE APPROVAL REASON"}
}

// TestBuildRunFromRealAgentRun records a real goagent run through the runkit
// sink and grades the resulting trajectory with evalkit's trajectory graders.
func TestBuildRunFromRealAgentRun(t *testing.T) {
	ctx := context.Background()
	const (
		privateInput      = "PRIVATE USER INPUT"
		privateToolInput  = "PRIVATE TOOL ARGUMENT"
		privateToolOutput = "PRIVATE TOOL OUTPUT"
		privateAnswer     = "PRIVATE FINAL ANSWER"
	)
	registry := tools.NewRegistry()
	for _, name := range []string{"search", "write"} {
		registry.Register(funcTool{
			spec: tools.Spec{Name: name, Permission: policy.PermissionRead},
			run: func() (*tools.Result, error) {
				return &tools.Result{ForLLM: privateToolOutput, Ref: "artifact:" + name}, nil
			},
		})
	}
	llm := &scriptedLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{ID: "c1", Name: "search", Input: json.RawMessage(`{"q":"` + privateToolInput + `"}`)}}, Usage: ports.Usage{InputTokens: 10, OutputTokens: 2}},
		{ToolCalls: []ports.ToolCall{{ID: "c2", Name: "write", Input: json.RawMessage(`{}`)}}, Usage: ports.Usage{InputTokens: 12, OutputTokens: 2}},
		{Content: privateAnswer, Usage: ports.Usage{InputTokens: 14, OutputTokens: 5}},
	}}
	runs := runkit.NewMemoryStore()
	sink := runkit.NewGoagentEventSink(runs, func(e agentcore.Event) runkit.RunRecord {
		return runkit.RunRecord{RunID: e.RunID.String(), TaskID: "task-1", Status: runkit.StatusRunning}
	})
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(llm),
		agentcore.WithToolRegistry(registry),
		agentcore.WithToolApprover(allowAll{}),
		agentcore.WithEventSink(sink),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.RunDetailed(ctx, agentcore.RunRequest{Input: privateInput})
	if err != nil {
		t.Fatal(err)
	}
	runID := result.RunID.String()
	if err := runs.Complete(ctx, runID, runkit.TerminalSummary{
		Status:       runkit.StatusSucceeded,
		ContentRef:   "artifact:answer",
		InputTokens:  result.Usage.InputTokens,
		OutputTokens: result.Usage.OutputTokens,
		LLMCalls:     result.ExecutionSummary.LLMCalls,
		ToolCalls:    result.ExecutionSummary.ToolCalls,
	}); err != nil {
		t.Fatal(err)
	}
	audit := fakeAudit{records: map[string][]llmkit.RouteAuditRecord{"task-1": {
		{Route: llmkit.RouteTrace{ModelAlias: "fast", Selected: true, RecordedAt: time.Now()}, Outcome: &llmkit.TaskOutcome{Success: true}},
	}}}

	recorded, err := runtrace.BuildRun(ctx, runtrace.Sources{Runs: runs, Audit: audit}, runID, runtrace.Fingerprint{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := evalkit.Runner{Harness: evalkit.RecordedHarness{"run": recorded}}.Run(ctx, evalkit.Suite{
		Name:  "recorded-agent-run",
		Tasks: []evalkit.Task{{ID: "run"}},
		Graders: []evalkit.Grader{
			evalkit.OutcomeStatus("succeeded"),
			evalkit.RequireTools("search", "write"),
			evalkit.ToolOrder("search", "write"),
			evalkit.ForbidTools("delete"),
			evalkit.NoFailedSteps(),
			evalkit.Budget{MaxToolCalls: 2, MaxModelCalls: 3, MaxTokens: 45}.Grader(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.PassedTrials != 1 {
		encoded, _ := json.MarshalIndent(report.Trials, "", "  ")
		t.Fatalf("recorded run failed grading:\n%s", encoded)
	}
	trace := recorded.Trace
	if approvals := evalkit.StepsOfType(trace, evalkit.StepApproval); len(approvals) != 2 || approvals[0].Status != evalkit.StatusSucceeded {
		t.Fatalf("approvals = %+v", approvals)
	}
	if calls := evalkit.ToolCalls(trace); len(calls) != 2 || calls[0].Metadata["ref"] != "artifact:search" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if recorded.Outcome.OutputRef != "artifact:answer" {
		t.Fatalf("outcome = %+v", recorded.Outcome)
	}
	encoded, _ := json.Marshal(recorded)
	for _, secret := range []string{privateInput, privateToolInput, privateToolOutput, privateAnswer, "PRIVATE APPROVAL REASON"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("trace contains %q", secret)
		}
	}
}

func TestSortStepsOrdersTimedBeforeUntimedAndPreservesTies(t *testing.T) {
	steps := []evalkit.TraceStep{
		{Name: "untimed-first"},
		{Name: "late", StartedAt: at(60)},
		{Name: "same-first", StartedAt: at(0)},
		{Name: "untimed-second"},
		{Name: "same-second", StartedAt: at(0)},
	}
	runtrace.SortSteps(steps)
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.Name)
	}
	if got := strings.Join(names, ","); got != "same-first,same-second,late,untimed-first,untimed-second" {
		t.Fatalf("sorted steps = %q", got)
	}
}
