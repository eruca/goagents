// Package runtrace builds content-free evalkit trajectories from stored
// workflow runs, agent run events, and llmkit route audits.
//
// Only allowlisted identifiers, statuses, timestamps, and counters are copied.
// Inputs, outputs, prompts, tool arguments, event messages, approval reasons,
// and free-text errors never enter a trace.
package runtrace

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/eruca/goagents/evalkit"
	"github.com/eruca/goagents/llmkit"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
)

// WorkflowReader reads one workflow run. workflowkit stores satisfy it.
type WorkflowReader interface {
	Get(context.Context, string) (workflowkit.WorkflowRun, error)
}

// RunReader reads agent runs and their events. runkit stores satisfy it.
type RunReader interface {
	Get(context.Context, string) (runkit.RunRecord, error)
	Events(context.Context, string) ([]runkit.RunEvent, error)
	FindByWorkflowID(context.Context, string) ([]runkit.RunRecord, error)
}

// AuditReader reads joined llmkit route audits. llmkit/pgaudit stores satisfy
// it; wrap llmkit.ReadRouteAudits for the JSONL audit.
type AuditReader interface {
	ReadRouteAudits(context.Context, llmkit.AuditFilter) ([]llmkit.RouteAuditRecord, error)
}

// Sources are the stores a trace is read from. Audit is optional; without it
// a trace has no model call steps.
type Sources struct {
	Workflows WorkflowReader
	Runs      RunReader
	Audit     AuditReader
}

// Fingerprint identifies what produced a run. Its non-empty fields become
// trace labels; it is typed so arbitrary host metadata cannot leak into
// labels.
type Fingerprint struct {
	GitCommit           string
	Provider            string
	ModelAlias          string
	AgentDefinitionHash string
	PromptVersion       string
	VisibleToolIDs      []string
}

// Labels returns the fingerprint as trace labels, or nil when it is empty.
func (f Fingerprint) Labels() map[string]string {
	labels := map[string]string{}
	setLabel(labels, "git.commit", f.GitCommit)
	setLabel(labels, "provider", f.Provider)
	setLabel(labels, "model.alias", f.ModelAlias)
	setLabel(labels, "agent.definition_hash", f.AgentDefinitionHash)
	setLabel(labels, "prompt.version", f.PromptVersion)
	if tools := sortedUnique(f.VisibleToolIDs); len(tools) > 0 {
		labels["tools.visible"] = strings.Join(tools, ",")
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

// BuildWorkflow builds the trajectory of one workflow: its step records, the
// events of every agent run it started, and, with an audit reader, the model
// calls whose route audit task ID is the workflow ID.
func BuildWorkflow(ctx context.Context, src Sources, workflowID string, fingerprint Fingerprint) (*evalkit.RunResult, error) {
	if src.Workflows == nil {
		return nil, fmt.Errorf("workflow store is required")
	}
	if src.Runs == nil {
		return nil, fmt.Errorf("run store is required")
	}
	workflow, err := src.Workflows.Get(ctx, workflowID)
	if err != nil {
		return nil, fmt.Errorf("get workflow %q: %w", workflowID, err)
	}
	agentRuns, err := src.Runs.FindByWorkflowID(ctx, workflowID)
	if err != nil {
		return nil, fmt.Errorf("find agent runs for workflow %q: %w", workflowID, err)
	}

	steps := WorkflowSteps(workflow.StepRecords)
	usage := evalkit.Usage{}
	for _, agentRun := range agentRuns {
		events, err := src.Runs.Events(ctx, agentRun.RunID)
		if err != nil {
			return nil, fmt.Errorf("read events for agent run %q: %w", agentRun.RunID, err)
		}
		steps = append(steps, EventSteps(events)...)
		addUsage(&usage, agentRun.Summary)
	}
	modelSteps, err := readModelCalls(ctx, src.Audit, workflowID)
	if err != nil {
		return nil, err
	}
	steps = append(steps, modelSteps...)
	SortSteps(steps)

	return &evalkit.RunResult{
		Outcome: evalkit.Outcome{
			Status:    string(workflow.Status),
			OutputRef: workflow.OutputRef,
			ErrorCode: workflowErrorCode(workflow.Status),
		},
		Trace: evalkit.Trace{
			RunID:  workflow.ID,
			Steps:  steps,
			Usage:  usage,
			Labels: fingerprint.Labels(),
		},
	}, nil
}

// BuildRun builds the trajectory of one agent run for hosts that use runkit
// without workflows. With an audit reader it adds the model calls whose route
// audit task ID is the run's TaskID, or its WorkflowID when TaskID is empty.
func BuildRun(ctx context.Context, src Sources, runID string, fingerprint Fingerprint) (*evalkit.RunResult, error) {
	if src.Runs == nil {
		return nil, fmt.Errorf("run store is required")
	}
	run, err := src.Runs.Get(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("get agent run %q: %w", runID, err)
	}
	events, err := src.Runs.Events(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read events for agent run %q: %w", runID, err)
	}
	steps := EventSteps(events)
	taskID := run.TaskID
	if strings.TrimSpace(taskID) == "" {
		taskID = run.WorkflowID
	}
	modelSteps, err := readModelCalls(ctx, src.Audit, taskID)
	if err != nil {
		return nil, err
	}
	steps = append(steps, modelSteps...)
	SortSteps(steps)

	status := run.Status
	if run.Summary.Status != "" {
		status = run.Summary.Status
	}
	usage := evalkit.Usage{}
	addUsage(&usage, run.Summary)
	errorCode := ""
	if status == runkit.StatusFailed {
		errorCode = "run_failed"
	}
	return &evalkit.RunResult{
		Outcome: evalkit.Outcome{
			Status:    string(status),
			OutputRef: run.Summary.ContentRef,
			ErrorCode: errorCode,
		},
		Trace: evalkit.Trace{
			RunID:  run.RunID,
			Steps:  steps,
			Usage:  usage,
			Labels: fingerprint.Labels(),
		},
	}, nil
}

func addUsage(usage *evalkit.Usage, summary runkit.TerminalSummary) {
	usage.InputTokens += summary.InputTokens
	usage.OutputTokens += summary.OutputTokens
	usage.LLMCalls += summary.LLMCalls
	usage.ToolCalls += summary.ToolCalls
}

func readModelCalls(ctx context.Context, audit AuditReader, taskID string) ([]evalkit.TraceStep, error) {
	if audit == nil || strings.TrimSpace(taskID) == "" {
		return nil, nil
	}
	records, err := audit.ReadRouteAudits(ctx, llmkit.AuditFilter{TaskID: taskID})
	if err != nil {
		return nil, fmt.Errorf("read route audits for task %q: %w", taskID, err)
	}
	return ModelCallSteps(records), nil
}

// WorkflowSteps maps workflow step records to StepWorkflow steps.
func WorkflowSteps(records []workflowkit.StepRecord) []evalkit.TraceStep {
	steps := make([]evalkit.TraceStep, 0, len(records))
	for _, record := range records {
		metadata := map[string]any{}
		if record.Attempt > 0 {
			metadata["attempt"] = record.Attempt
		}
		setNonEmpty(metadata, "output_ref", record.OutputRef)
		setNonEmpty(metadata, "agent_run_id", record.AgentRunID)
		setNonEmpty(metadata, "audit_ref", record.AuditRef)
		setNonEmpty(metadata, "approval_ref", record.ApprovalRef)
		steps = append(steps, evalkit.TraceStep{
			Type:      evalkit.StepWorkflow,
			Name:      record.Name,
			Status:    string(record.Status),
			StartedAt: record.StartedAt,
			EndedAt:   record.EndedAt,
			Metadata:  nonEmpty(metadata),
		})
	}
	return steps
}

// ModelCallSteps maps selected route audits to StepModelCall steps named by
// model alias. A route without an outcome is still running or was lost.
func ModelCallSteps(records []llmkit.RouteAuditRecord) []evalkit.TraceStep {
	var steps []evalkit.TraceStep
	for _, record := range records {
		route := record.Route
		if !route.Selected {
			continue
		}
		metadata := map[string]any{}
		setNonEmpty(metadata, "route_id", route.RouteID)
		if route.Attempt > 0 {
			metadata["attempt"] = route.Attempt
		}
		setNonEmpty(metadata, "provider", route.Provider)
		setNonEmpty(metadata, "account_alias", route.AccountAlias)
		step := evalkit.TraceStep{
			Type:      evalkit.StepModelCall,
			Name:      route.ModelAlias,
			Status:    evalkit.StatusRunning,
			StartedAt: route.RecordedAt,
		}
		if outcome := record.Outcome; outcome != nil {
			step.Status = evalkit.StatusFailed
			if outcome.Success {
				step.Status = evalkit.StatusSucceeded
			}
			step.EndedAt = outcome.RecordedAt
			setNonEmpty(metadata, "error_class", string(outcome.ErrorClass))
			setNonEmpty(metadata, "error_code", outcome.ErrorCode)
			setNonEmpty(metadata, "business_outcome", string(outcome.BusinessOutcome))
			setPositive(metadata, "latency_ms", outcome.LatencyMillis)
			setPositive(metadata, "input_tokens", outcome.InputTokens)
			setPositive(metadata, "output_tokens", outcome.OutputTokens)
		}
		step.Metadata = nonEmpty(metadata)
		steps = append(steps, step)
	}
	return steps
}

type openCall struct {
	step      int
	iteration int
	ordinal   int
}

type approvalKey struct {
	tool  string
	index int
}

// EventSteps maps goagent run events to trace steps. A tool.started event and
// the tool.completed or tool.failed event with the same iteration and call
// index become one StepToolCall (a call is matched by name in the same
// iteration, preferring the started event at the same position); a tool.rejected event is a rejected call. An
// approval.requested event and its pending, completed, or denied resolution
// become one StepApproval, matched by tool and index so a resolution after a
// resume closes the request recorded before it. Other events become
// StepAgentEvent. Unmatched resolutions stay single steps with their own
// timestamp.
func EventSteps(events []runkit.RunEvent) []evalkit.TraceStep {
	steps := make([]evalkit.TraceStep, 0, len(events))
	var openCalls []openCall
	startedInIteration := map[int]int{}
	openApprovals := map[approvalKey]int{}

	for _, event := range events {
		toolName := stringField(event.Metadata, "tool")
		index, hasIndex := intField(event.Metadata, "index")
		step := evalkit.TraceStep{
			Name:      toolName,
			StartedAt: event.RecordedAt,
			EndedAt:   event.RecordedAt,
			Metadata:  eventMetadata(event, index, hasIndex),
		}
		switch event.Type {
		case "tool.started":
			ordinal := startedInIteration[event.Iteration]
			startedInIteration[event.Iteration]++
			step.Type, step.Status, step.EndedAt = evalkit.StepToolCall, evalkit.StatusRunning, event.RecordedAt
			if step.Metadata == nil {
				step.Metadata = map[string]any{}
			}
			step.Metadata["index"] = ordinal
			openCalls = append(openCalls, openCall{step: len(steps), iteration: event.Iteration, ordinal: ordinal})
		case "tool.completed", "tool.failed":
			status := evalkit.StatusSucceeded
			if event.Type == "tool.failed" || boolField(event.Metadata, "is_error") {
				status = evalkit.StatusFailed
			}
			if match := matchOpenCall(openCalls, steps, event.Iteration, toolName, index, hasIndex); match >= 0 {
				at := openCalls[match].step
				openCalls = append(openCalls[:match], openCalls[match+1:]...)
				steps[at].Status = status
				steps[at].EndedAt = event.RecordedAt
				if ref := stringField(event.Metadata, "ref"); ref != "" {
					steps[at].Metadata["ref"] = ref
				}
				continue
			}
			step.Type, step.Status = evalkit.StepToolCall, status
		case "tool.rejected":
			step.Type, step.Status = evalkit.StepToolCall, evalkit.StatusRejected
		case "approval.requested":
			step.Type, step.Status = evalkit.StepApproval, evalkit.StatusWaitingApproval
			openApprovals[approvalKey{toolName, index}] = len(steps)
		case "approval.pending", "approval.completed", "approval.denied":
			status := evalkit.StatusWaitingApproval
			switch event.Type {
			case "approval.completed":
				status = evalkit.StatusSucceeded
			case "approval.denied":
				status = evalkit.StatusRejected
			}
			key := approvalKey{toolName, index}
			if at, ok := openApprovals[key]; ok {
				steps[at].Status = status
				if status != evalkit.StatusWaitingApproval {
					steps[at].EndedAt = event.RecordedAt
					delete(openApprovals, key)
				}
				continue
			}
			step.Type, step.Status = evalkit.StepApproval, status
		default:
			step.Type, step.Name, step.Status = evalkit.StepAgentEvent, event.Type, agentEventStatus(event.Type)
		}
		steps = append(steps, step)
	}
	return steps
}

// matchOpenCall returns the position in open of the call a completion closes:
// the open call with the same iteration and tool at the completion's index,
// else the earliest open call with the same iteration and tool, else -1.
func matchOpenCall(open []openCall, steps []evalkit.TraceStep, iteration int, tool string, index int, hasIndex bool) int {
	fallback := -1
	for i, call := range open {
		if call.iteration != iteration || steps[call.step].Name != tool {
			continue
		}
		if hasIndex && call.ordinal == index {
			return i
		}
		if fallback < 0 {
			fallback = i
		}
	}
	return fallback
}

func eventMetadata(event runkit.RunEvent, index int, hasIndex bool) map[string]any {
	metadata := map[string]any{}
	if event.Sequence > 0 {
		metadata["sequence"] = event.Sequence
	}
	setNonEmpty(metadata, "stage", event.Stage)
	if event.Iteration > 0 {
		metadata["iteration"] = event.Iteration
	}
	if hasIndex {
		metadata["index"] = index
	}
	setNonEmpty(metadata, "ref", stringField(event.Metadata, "ref"))
	return nonEmpty(metadata)
}

func agentEventStatus(eventType string) string {
	switch {
	case strings.HasSuffix(eventType, ".started"):
		return evalkit.StatusRunning
	case strings.HasSuffix(eventType, ".completed"), eventType == "finalized", eventType == "output.validated", eventType == "input.validated":
		return evalkit.StatusSucceeded
	case strings.HasSuffix(eventType, ".failed"):
		return evalkit.StatusFailed
	case strings.HasSuffix(eventType, ".rejected"), strings.HasSuffix(eventType, ".denied"):
		return evalkit.StatusRejected
	default:
		return ""
	}
}

func workflowErrorCode(status workflowkit.Status) string {
	switch status {
	case workflowkit.StatusFailed:
		return "workflow_failed"
	case workflowkit.StatusCancelled:
		return "workflow_cancelled"
	default:
		return ""
	}
}

// SortSteps orders steps by start time without inventing timestamps: timed
// steps come first, and equal or missing timestamps keep their source order.
func SortSteps(steps []evalkit.TraceStep) {
	sort.SliceStable(steps, func(i, j int) bool {
		left, right := steps[i].StartedAt, steps[j].StartedAt
		if left.IsZero() != right.IsZero() {
			return !left.IsZero()
		}
		return !left.IsZero() && left.Before(right)
	})
}

func stringField(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func boolField(metadata map[string]any, key string) bool {
	value, _ := metadata[key].(bool)
	return value
}

// intField accepts the integer forms metadata takes before and after a JSON
// round trip through a store.
func intField(metadata map[string]any, key string) (int, bool) {
	switch value := metadata[key].(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		if value == math.Trunc(value) && math.Abs(value) < 1<<31 {
			return int(value), true
		}
	case json.Number:
		if parsed, err := value.Int64(); err == nil {
			return int(parsed), true
		}
	}
	return 0, false
}

func setLabel(labels map[string]string, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		labels[key] = value
	}
}

func setNonEmpty(metadata map[string]any, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		metadata[key] = value
	}
}

func setPositive(metadata map[string]any, key string, value int) {
	if value > 0 {
		metadata[key] = value
	}
}

func nonEmpty(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
