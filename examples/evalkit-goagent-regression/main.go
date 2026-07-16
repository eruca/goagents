package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/eruca/goagents/evalkit"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
)

type deterministicLLM struct{}

func (l deterministicLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	input := lastUserMessage(req.Messages)
	if strings.Contains(input, "A-123") {
		return &ports.ChatResponse{
			Content: "account A-123 status: active; next action: monitor renewal",
			Usage:   ports.Usage{InputTokens: 32, OutputTokens: 12},
		}, nil
	}
	return &ports.ChatResponse{
		Content: "account status: unknown",
		Usage:   ports.Usage{InputTokens: 18, OutputTokens: 5},
	}, nil
}

func lastUserMessage(messages []ports.ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

type goagentHarness struct{}

func (h goagentHarness) RunTask(ctx context.Context, task evalkit.Task) (*evalkit.RunResult, error) {
	agent, err := agentcore.NewAgent(agentcore.WithLLM(deterministicLLM{}))
	if err != nil {
		return nil, err
	}

	result, err := agent.RunDetailed(ctx, agentcore.RunRequest{Input: task.Input})
	if err != nil {
		return nil, err
	}
	return &evalkit.RunResult{
		Output: result.Content,
		Trace: evalkit.Trace{
			RunID: result.RunID.String(),
			Steps: []evalkit.TraceStep{{
				Type:   "agent",
				Name:   "goagent.RunDetailed",
				Status: "completed",
			}},
			Usage: evalkit.Usage{
				InputTokens:  result.Usage.InputTokens,
				OutputTokens: result.Usage.OutputTokens,
				LLMCalls:     result.ExecutionSummary.LLMCalls,
				ToolCalls:    result.ExecutionSummary.ToolCalls,
			},
		},
		Metadata: map[string]any{
			"agent": "goagent",
		},
	}, nil
}

type report struct {
	Suite        string        `json:"suite"`
	TotalTrials  int           `json:"total_trials"`
	PassedTrials int           `json:"passed_trials"`
	FailedTrials int           `json:"failed_trials"`
	Trials       []trialReport `json:"trials"`
}

type trialReport struct {
	TaskID      string   `json:"task_id"`
	Index       int      `json:"index"`
	Passed      bool     `json:"passed"`
	Output      string   `json:"output"`
	LLMCalls    int      `json:"llm_calls"`
	ToolCalls   int      `json:"tool_calls"`
	GradeNames  []string `json:"grade_names"`
	StepCount   int      `json:"step_count"`
	AgentSource string   `json:"agent_source"`
}

func runRegressionDemo(w io.Writer) error {
	runner := evalkit.Runner{
		Harness:       goagentHarness{},
		TrialsPerTask: 2,
	}
	suite := evalkit.Suite{
		Name: "goagent-account-regression",
		Tasks: []evalkit.Task{{
			ID:              "account-a123-status",
			Input:           "Summarize account A-123 status for a customer-success handoff.",
			SuccessCriteria: "Mentions account A-123, reports status, and uses one agent run.",
		}},
		Graders: []evalkit.Grader{
			evalkit.GraderFunc{
				GraderName: "answer-contract",
				Fn: func(ctx context.Context, req evalkit.GradeRequest) (*evalkit.GradeResult, error) {
					output := strings.ToLower(req.Trial.Output)
					return &evalkit.GradeResult{
						Score: 1,
						Assertions: []evalkit.Assertion{
							{Name: "mentions-account", Passed: strings.Contains(req.Trial.Output, "A-123")},
							{Name: "mentions-status", Passed: strings.Contains(output, "status")},
						},
					}, nil
				},
			},
			evalkit.GraderFunc{
				GraderName: "trace-contract",
				Fn: func(ctx context.Context, req evalkit.GradeRequest) (*evalkit.GradeResult, error) {
					return &evalkit.GradeResult{
						Score: 1,
						Assertions: []evalkit.Assertion{
							{Name: "single-llm-call", Passed: req.Trial.Trace.Usage.LLMCalls == 1},
							{Name: "has-trace-step", Passed: len(req.Trial.Trace.Steps) == 1},
						},
					}, nil
				},
			},
		},
	}

	result, err := runner.Run(context.Background(), suite)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(toReport(result))
}

func toReport(result *evalkit.SuiteResult) report {
	out := report{
		Suite:        result.Name,
		TotalTrials:  result.Summary.TotalTrials,
		PassedTrials: result.Summary.PassedTrials,
		FailedTrials: result.Summary.FailedTrials,
	}
	for _, trial := range result.Trials {
		row := trialReport{
			TaskID:      trial.Trial.TaskID,
			Index:       trial.Trial.Index,
			Passed:      trial.Passed,
			Output:      trial.Trial.Output,
			LLMCalls:    trial.Trial.Trace.Usage.LLMCalls,
			ToolCalls:   trial.Trial.Trace.Usage.ToolCalls,
			StepCount:   len(trial.Trial.Trace.Steps),
			AgentSource: fmt.Sprint(trial.Trial.Metadata["agent"]),
		}
		for _, grade := range trial.Grades {
			row.GradeNames = append(row.GradeNames, grade.Name)
		}
		out.Trials = append(out.Trials, row)
	}
	return out
}

func main() {
	if err := runRegressionDemo(os.Stdout); err != nil {
		panic(err)
	}
}
