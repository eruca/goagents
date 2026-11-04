package agentstep

import (
	"context"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/workflowkit"
)

type Runner interface {
	RunDetailed(context.Context, agentcore.RunRequest) (*agentcore.RunResult, error)
}

type RequestBuilder func(workflowkit.WorkflowRun) agentcore.RunRequest

// ResultMapper turns an agent run into the step's result. It sees the run's
// error too, so it can map an interruption such as agentcore.ErrApprovalPending
// to waiting_approval by returning a nil error. A returned error fails the
// step; the default mapper is DefaultResult.
type ResultMapper func(run workflowkit.WorkflowRun, result *agentcore.RunResult, err error) (workflowkit.StepResult, error)

// maxContentPreviewRunes bounds the agent output DefaultResult keeps in step
// metadata; full output belongs in an artifact.
const maxContentPreviewRunes = 256

type Step struct {
	name    string
	runner  Runner
	builder RequestBuilder
	mapper  ResultMapper
}

type Option func(*Step)

func WithResultMapper(mapper ResultMapper) Option {
	return func(s *Step) {
		s.mapper = mapper
	}
}

func New(name string, runner Runner, builder RequestBuilder, options ...Option) Step {
	step := Step{name: name, runner: runner, builder: builder}
	for _, option := range options {
		option(&step)
	}
	return step
}

func (s Step) Name() string {
	return s.name
}

func (s Step) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	req := agentcore.RunRequest{}
	if s.builder != nil {
		req = s.builder(run)
	}
	result, err := s.runner.RunDetailed(ctx, req)
	if s.mapper != nil {
		return s.mapper(run, result, err)
	}
	return DefaultResult(run, result, err)
}

// DefaultResult maps a failed run to a failed step that keeps the agent run
// ID, and a successful one to a step that completes with no status, so the
// workflow continues with its next step. It records the agent run as the
// step's output and a bounded content preview.
func DefaultResult(_ workflowkit.WorkflowRun, result *agentcore.RunResult, err error) (workflowkit.StepResult, error) {
	if err != nil {
		return failedResult(result, err), err
	}
	if result == nil {
		return workflowkit.StepResult{}, nil
	}
	runID := result.RunID.String()
	return workflowkit.StepResult{
		AgentRunID: runID,
		OutputRef:  "agent:" + runID,
		Metadata: map[string]any{
			"content_preview": contentPreview(result.Content),
		},
	}, nil
}

func contentPreview(content string) string {
	runes := []rune(content)
	if len(runes) <= maxContentPreviewRunes {
		return content
	}
	return string(runes[:maxContentPreviewRunes])
}

func failedResult(result *agentcore.RunResult, err error) workflowkit.StepResult {
	out := workflowkit.StepResult{
		Status: workflowkit.StatusFailed,
		Error:  err.Error(),
	}
	if result == nil {
		return out
	}
	out.AgentRunID = result.RunID.String()
	if result.ExecutionSummary.AbortReason != "" {
		out.Error = result.ExecutionSummary.AbortReason
	}
	return out
}
