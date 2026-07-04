package evalkit

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunnerRepeatsTasksAndAggregatesGrades(t *testing.T) {
	now := scriptedClock(time.Unix(100, 0), time.Second)
	var calls []string
	runner := Runner{
		TrialsPerTask: 2,
		Now:           now,
		Harness: HarnessFunc(func(ctx context.Context, task Task) (*RunResult, error) {
			calls = append(calls, task.ID)
			return &RunResult{
				Output: "answer for " + task.ID,
				Trace: Trace{
					RunID: "run-" + task.ID,
					Steps: []TraceStep{{
						Type:   "tool",
						Name:   "lookup",
						Status: "completed",
					}},
					Usage: Usage{LLMCalls: 1, ToolCalls: 1},
				},
			}, nil
		}),
	}

	result, err := runner.Run(context.Background(), Suite{
		Name: "smoke",
		Tasks: []Task{
			{ID: "task-a", Input: "a"},
			{ID: "task-b", Input: "b"},
		},
		Graders: []Grader{GraderFunc{
			GraderName: "contains-answer",
			Fn: func(ctx context.Context, req GradeRequest) (*GradeResult, error) {
				return &GradeResult{
					Assertions: []Assertion{{
						Name:   "has-answer",
						Passed: strings.Contains(req.Trial.Output, "answer"),
					}},
					Score: 1,
				}, nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("run eval suite: %v", err)
	}

	if !reflect.DeepEqual(calls, []string{"task-a", "task-a", "task-b", "task-b"}) {
		t.Fatalf("harness calls = %#v", calls)
	}
	if result.Summary.TotalTrials != 4 || result.Summary.PassedTrials != 4 || result.Summary.FailedTrials != 0 {
		t.Fatalf("summary = %+v, want all trials passed", result.Summary)
	}
	if result.Summary.TaskCount != 2 || result.Summary.GraderCount != 1 {
		t.Fatalf("summary counts = %+v", result.Summary)
	}
	if result.Trials[0].Trial.Trace.RunID != "run-task-a" {
		t.Fatalf("trace run id = %q", result.Trials[0].Trial.Trace.RunID)
	}
}

func TestRunnerRecordsHarnessErrorAsFailedTrial(t *testing.T) {
	runner := Runner{
		Harness: HarnessFunc(func(ctx context.Context, task Task) (*RunResult, error) {
			return nil, errors.New("provider unavailable")
		}),
	}

	result, err := runner.Run(context.Background(), Suite{
		Name:  "errors",
		Tasks: []Task{{ID: "task-a", Input: "a"}},
	})
	if err != nil {
		t.Fatalf("run eval suite: %v", err)
	}

	if result.Summary.TotalTrials != 1 || result.Summary.FailedTrials != 1 {
		t.Fatalf("summary = %+v, want one failed trial", result.Summary)
	}
	trial := result.Trials[0]
	if trial.Passed {
		t.Fatal("trial passed after harness error")
	}
	if trial.Trial.Error != "provider unavailable" {
		t.Fatalf("trial error = %q", trial.Trial.Error)
	}
}

func TestRunnerMarksGraderFailure(t *testing.T) {
	runner := Runner{
		Harness: HarnessFunc(func(ctx context.Context, task Task) (*RunResult, error) {
			return &RunResult{Output: "wrong answer"}, nil
		}),
	}

	result, err := runner.Run(context.Background(), Suite{
		Tasks: []Task{{ID: "task-a", Input: "a"}},
		Graders: []Grader{GraderFunc{
			GraderName: "must-contain-right",
			Fn: func(ctx context.Context, req GradeRequest) (*GradeResult, error) {
				return &GradeResult{
					Assertions: []Assertion{{
						Name:    "contains-right",
						Passed:  strings.Contains(req.Trial.Output, "right"),
						Message: "output should contain right",
					}},
				}, nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("run eval suite: %v", err)
	}

	trial := result.Trials[0]
	if trial.Passed {
		t.Fatal("trial passed despite failed assertion")
	}
	if got := trial.Grades[0].Name; got != "must-contain-right" {
		t.Fatalf("grade name = %q", got)
	}
	if result.Summary.FailedTrials != 1 {
		t.Fatalf("summary = %+v, want failed trial", result.Summary)
	}
}

func TestRunnerValidatesSuite(t *testing.T) {
	runner := Runner{Harness: HarnessFunc(func(ctx context.Context, task Task) (*RunResult, error) {
		return &RunResult{}, nil
	})}

	if _, err := runner.Run(context.Background(), Suite{}); err == nil {
		t.Fatal("expected empty suite validation error")
	}
	_, err := runner.Run(context.Background(), Suite{
		Tasks: []Task{{ID: "dup"}, {ID: "dup"}},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate task id") {
		t.Fatalf("duplicate task error = %v", err)
	}
}

func TestRunnerRequiresHarness(t *testing.T) {
	_, err := Runner{}.Run(context.Background(), Suite{
		Tasks: []Task{{ID: "task-a"}},
	})
	if err == nil || !strings.Contains(err.Error(), "harness is required") {
		t.Fatalf("missing harness error = %v", err)
	}
}

func scriptedClock(start time.Time, step time.Duration) func() time.Time {
	current := start.Add(-step)
	return func() time.Time {
		current = current.Add(step)
		return current
	}
}
