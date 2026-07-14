package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/eruca/evalkit"
	"github.com/eruca/runkit"
	"github.com/eruca/workflowkit"
)

// hostEvalFingerprint accepts only known non-secret version fields. Keeping
// this typed prevents arbitrary host metadata from leaking into eval reports.
type hostEvalFingerprint struct {
	GitCommit           string
	Provider            string
	ModelAlias          string
	AgentDefinitionHash string
	PromptVersion       string
	VisibleToolIDs      []string
}

func buildHostEvalResult(
	ctx context.Context,
	workflowID string,
	workflows workflowkit.Store,
	runs runkit.Store,
	fingerprint hostEvalFingerprint,
) (*evalkit.RunResult, error) {
	if workflows == nil {
		return nil, fmt.Errorf("workflow store is required")
	}
	if runs == nil {
		return nil, fmt.Errorf("run store is required")
	}

	workflow, err := workflows.Get(ctx, workflowID)
	if err != nil {
		return nil, fmt.Errorf("get workflow %q: %w", workflowID, err)
	}
	agentRuns, err := runs.FindByWorkflowID(ctx, workflowID)
	if err != nil {
		return nil, fmt.Errorf("find agent runs for workflow %q: %w", workflowID, err)
	}

	steps := workflowTraceSteps(workflow.StepRecords)
	usage := evalkit.Usage{}
	for _, agentRun := range agentRuns {
		events, err := runs.Events(ctx, agentRun.RunID)
		if err != nil {
			return nil, fmt.Errorf("read events for agent run %q: %w", agentRun.RunID, err)
		}
		steps = append(steps, agentEventTraceSteps(events)...)
		usage.InputTokens += agentRun.Summary.InputTokens
		usage.OutputTokens += agentRun.Summary.OutputTokens
		usage.LLMCalls += agentRun.Summary.LLMCalls
		usage.ToolCalls += agentRun.Summary.ToolCalls
	}
	sortTraceSteps(steps)

	return &evalkit.RunResult{
		Outcome: evalkit.Outcome{
			Status:    string(workflow.Status),
			OutputRef: workflow.OutputRef,
			ErrorCode: workflowEvalErrorCode(workflow.Status),
			Metadata: map[string]any{
				"run_mode": string(workflowRunMode(workflow, RunModeSync)),
			},
		},
		Trace: evalkit.Trace{
			RunID:  workflow.ID,
			Steps:  steps,
			Usage:  usage,
			Labels: hostEvalLabels(workflow, fingerprint),
		},
	}, nil
}

func workflowTraceSteps(records []workflowkit.StepRecord) []evalkit.TraceStep {
	steps := make([]evalkit.TraceStep, 0, len(records))
	for _, record := range records {
		metadata := make(map[string]any, 5)
		if record.Attempt > 0 {
			metadata["attempt"] = record.Attempt
		}
		setNonEmpty(metadata, "output_ref", record.OutputRef)
		setNonEmpty(metadata, "agent_run_id", record.AgentRunID)
		setNonEmpty(metadata, "audit_ref", record.AuditRef)
		setNonEmpty(metadata, "approval_ref", record.ApprovalRef)
		steps = append(steps, evalkit.TraceStep{
			Type:      "workflow_step",
			Name:      record.Name,
			Status:    string(record.Status),
			StartedAt: record.StartedAt,
			EndedAt:   record.EndedAt,
			Metadata:  nonEmptyMetadata(metadata),
		})
	}
	return steps
}

func agentEventTraceSteps(events []runkit.RunEvent) []evalkit.TraceStep {
	steps := make([]evalkit.TraceStep, 0, len(events))
	for _, event := range events {
		metadata := make(map[string]any, 5)
		if event.Sequence > 0 {
			metadata["sequence"] = event.Sequence
		}
		setNonEmpty(metadata, "stage", event.Stage)
		if event.Iteration > 0 {
			metadata["iteration"] = event.Iteration
		}
		copyStringMetadata(metadata, event.Metadata, "tool")
		copyStringMetadata(metadata, event.Metadata, "ref")
		steps = append(steps, evalkit.TraceStep{
			Type:      "agent_event",
			Name:      event.Type,
			Status:    normalizedAgentEventStatus(event.Type),
			StartedAt: event.RecordedAt,
			EndedAt:   event.RecordedAt,
			Metadata:  nonEmptyMetadata(metadata),
		})
	}
	return steps
}

// sortTraceSteps keeps parent spans and events deterministic without inventing
// timestamps. Timed records come first; stable sorting preserves source order
// for equal or missing timestamps.
func sortTraceSteps(steps []evalkit.TraceStep) {
	sort.SliceStable(steps, func(i, j int) bool {
		left := steps[i].StartedAt
		right := steps[j].StartedAt
		if left.IsZero() != right.IsZero() {
			return !left.IsZero()
		}
		if left.IsZero() {
			return false
		}
		return left.Before(right)
	})
}

func normalizedAgentEventStatus(eventType string) string {
	eventType = strings.TrimSpace(eventType)
	switch {
	case strings.HasSuffix(eventType, ".started"):
		return "running"
	case strings.HasSuffix(eventType, ".completed"), eventType == "finalized", eventType == "output.validated", eventType == "input.validated":
		return "succeeded"
	case strings.HasSuffix(eventType, ".failed"), strings.HasSuffix(eventType, ".denied"), strings.HasSuffix(eventType, ".rejected"):
		return "failed"
	case strings.HasSuffix(eventType, ".pending"), strings.HasSuffix(eventType, ".requested"):
		return "waiting_approval"
	default:
		return ""
	}
}

func workflowEvalErrorCode(status workflowkit.Status) string {
	switch status {
	case workflowkit.StatusFailed:
		return "workflow_failed"
	case workflowkit.StatusCancelled:
		return "workflow_cancelled"
	default:
		return ""
	}
}

func hostEvalLabels(workflow workflowkit.WorkflowRun, fingerprint hostEvalFingerprint) map[string]string {
	labels := make(map[string]string, 7)
	setNonEmptyString(labels, "git.commit", fingerprint.GitCommit)
	setNonEmptyString(labels, "provider", fingerprint.Provider)
	setNonEmptyString(labels, "model.alias", fingerprint.ModelAlias)
	setNonEmptyString(labels, "agent.definition_hash", fingerprint.AgentDefinitionHash)
	setNonEmptyString(labels, "prompt.version", fingerprint.PromptVersion)

	refs := workflowSkillRefsFromMetadata(workflow.Metadata)
	skillRefs := make([]string, 0, len(refs))
	for _, ref := range refs {
		skillRefs = append(skillRefs, ref.Name+"@"+ref.Digest)
	}
	sort.Strings(skillRefs)
	if len(skillRefs) > 0 {
		labels["skill.refs"] = strings.Join(skillRefs, ",")
	}

	visibleTools := sortedUnique(fingerprint.VisibleToolIDs)
	if len(visibleTools) > 0 {
		labels["tools.visible"] = strings.Join(visibleTools, ",")
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func copyStringMetadata(target map[string]any, source map[string]any, key string) {
	value, _ := source[key].(string)
	setNonEmpty(target, key, value)
}

func setNonEmpty(target map[string]any, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		target[key] = value
	}
}

func setNonEmptyString(target map[string]string, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		target[key] = value
	}
}

func nonEmptyMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}
