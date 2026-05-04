package agentstep

import (
	"context"

	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/workflowkit"
)

type Runner interface {
	RunDetailed(context.Context, agentcore.RunRequest) (*agentcore.RunResult, error)
}

type RequestBuilder func(workflowkit.WorkflowRun) agentcore.RunRequest
type ResultMapper func(workflowkit.WorkflowRun, *agentcore.RunResult) workflowkit.StepResult

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
	if err != nil {
		return failedResult(result, err), err
	}
	if s.mapper != nil {
		return s.mapper(run, result), nil
	}
	return successResult(result), nil
}

func successResult(result *agentcore.RunResult) workflowkit.StepResult {
	if result == nil {
		return workflowkit.StepResult{Status: workflowkit.StatusSucceeded}
	}
	runID := result.RunID.String()
	return workflowkit.StepResult{
		Status:     workflowkit.StatusSucceeded,
		AgentRunID: runID,
		OutputRef:  "agent:" + runID,
		Metadata: map[string]any{
			"content_preview": result.Content,
		},
	}
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
