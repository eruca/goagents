package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/eruca/goagents/evalkit"
	"github.com/eruca/goagents/evalkit/runtrace"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
)

// buildHostEvalResult builds a workflow's content-free trajectory with
// evalkit/runtrace and adds the host-specific run mode and Skill labels.
// audit may be nil, in which case the trace has no model call steps.
func buildHostEvalResult(
	ctx context.Context,
	workflowID string,
	workflows workflowkit.Store,
	runs runkit.Store,
	audit runtrace.AuditReader,
	fingerprint runtrace.Fingerprint,
) (*evalkit.RunResult, error) {
	if workflows == nil {
		return nil, fmt.Errorf("workflow store is required")
	}
	if runs == nil {
		return nil, fmt.Errorf("run store is required")
	}
	result, err := runtrace.BuildWorkflow(ctx, runtrace.Sources{Workflows: workflows, Runs: runs, Audit: audit}, workflowID, fingerprint)
	if err != nil {
		return nil, err
	}
	workflow, err := workflows.Get(ctx, workflowID)
	if err != nil {
		return nil, fmt.Errorf("get workflow %q: %w", workflowID, err)
	}
	result.Outcome.Metadata = map[string]any{
		"run_mode": string(workflowRunMode(workflow, RunModeSync)),
	}
	refs := workflowSkillRefsFromMetadata(workflow.Metadata)
	skillRefs := make([]string, 0, len(refs))
	for _, ref := range refs {
		skillRefs = append(skillRefs, ref.Name+"@"+ref.Digest)
	}
	sort.Strings(skillRefs)
	if len(skillRefs) > 0 {
		if result.Trace.Labels == nil {
			result.Trace.Labels = map[string]string{}
		}
		result.Trace.Labels["skill.refs"] = strings.Join(skillRefs, ",")
	}
	return result, nil
}
