package evalkit

import (
	"context"
	"strings"
	"testing"
	"time"
)

func trajectoryTrial(status string, usage Usage, steps ...TraceStep) Trial {
	return Trial{Outcome: Outcome{Status: status}, Trace: Trace{Steps: steps, Usage: usage}}
}

func tool(name, status string) TraceStep {
	return TraceStep{Type: StepToolCall, Name: name, Status: status}
}

func grade(t *testing.T, grader Grader, trial Trial) GradeResult {
	t.Helper()
	return runGrader(context.Background(), grader, Task{ID: "task"}, trial)
}

func TestToolGraders(t *testing.T) {
	trial := trajectoryTrial("succeeded", Usage{},
		tool("search", StatusSucceeded),
		TraceStep{Type: StepModelCall, Name: "search"},
		tool("fetch", StatusSucceeded),
		tool("search", StatusSucceeded),
		tool("write", StatusSucceeded),
	)
	cases := []struct {
		name   string
		grader Grader
		passed bool
	}{
		{"require present", RequireTools("search", "write"), true},
		{"require missing", RequireTools("search", "delete"), false},
		{"require ignores model call names", RequireTools("fetch"), true},
		{"forbid absent", ForbidTools("delete"), true},
		{"forbid present", ForbidTools("delete", "fetch"), false},
		{"order subsequence", ToolOrder("search", "fetch", "write"), true},
		{"order repeated tool", ToolOrder("search", "search", "write"), true},
		{"order wrong", ToolOrder("write", "fetch"), false},
		{"order too long", ToolOrder("search", "search", "search"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := grade(t, tc.grader, trial)
			if got.Passed != tc.passed {
				t.Fatalf("passed = %v, want %v: %+v", got.Passed, tc.passed, got)
			}
			for _, assertion := range got.Assertions {
				if !assertion.Passed && assertion.Message == "" {
					t.Fatalf("failed assertion without message: %+v", got)
				}
			}
		})
	}
}

func TestToolGraderFailureMessages(t *testing.T) {
	trial := trajectoryTrial("", Usage{}, tool("fetch", StatusSucceeded), tool("fetch", StatusSucceeded))
	got := grade(t, ForbidTools("fetch"), trial)
	if got.Passed || got.Assertions[0].Message != `tool "fetch" was called 2 time(s)` {
		t.Fatalf("forbid = %+v", got)
	}
	got = grade(t, ToolOrder("fetch", "write"), trial)
	if got.Passed || got.Assertions[0].Message != `matched 1 of 2 tools; next expected "write"` {
		t.Fatalf("order = %+v", got)
	}
	got = grade(t, RequireTools("search", "fetch"), trial)
	if got.Passed || got.Score != 0.5 {
		t.Fatalf("require score = %+v", got)
	}
}

func TestGradersWithoutNamesFail(t *testing.T) {
	for _, grader := range []Grader{RequireTools(), ForbidTools(" "), ToolOrder(), OutcomeStatus(), Budget{MaxTokens: -1}.Grader()} {
		got := grade(t, grader, trajectoryTrial("succeeded", Usage{}))
		if got.Passed || got.Message == "" {
			t.Fatalf("%s = %+v, want configuration failure", grader.Name(), got)
		}
	}
}

func TestNoFailedSteps(t *testing.T) {
	trial := trajectoryTrial("", Usage{},
		tool("search", StatusSucceeded),
		tool("delete", StatusRejected),
		TraceStep{Type: StepModelCall, Name: "gpt", Status: StatusFailed},
		TraceStep{Type: StepApproval, Name: "approve", Status: StatusWaitingApproval},
	)
	if got := grade(t, NoFailedSteps(), trial); got.Passed || len(got.Assertions) != 2 {
		t.Fatalf("all types = %+v", got)
	}
	if got := grade(t, NoFailedSteps(StepToolCall), trial); got.Passed || len(got.Assertions) != 1 || !strings.Contains(got.Assertions[0].Message, `"delete" is rejected`) {
		t.Fatalf("tool type = %+v", got)
	}
	if got := grade(t, NoFailedSteps(StepApproval, StepWorkflow), trial); !got.Passed {
		t.Fatalf("approval type = %+v", got)
	}
}

func TestBudget(t *testing.T) {
	start := time.Unix(1000, 0)
	trial := trajectoryTrial("", Usage{InputTokens: 70, OutputTokens: 30, ToolCalls: 1, LLMCalls: 3},
		TraceStep{Type: StepToolCall, Name: "a", StartedAt: start, EndedAt: start.Add(time.Second)},
		TraceStep{Type: StepToolCall, Name: "b"},
		TraceStep{Type: StepModelCall, Name: "m", StartedAt: start.Add(4 * time.Second)},
	)
	if got := TraceDuration(trial.Trace); got != 4*time.Second {
		t.Fatalf("duration = %s", got)
	}
	cases := []struct {
		name   string
		budget Budget
		passed bool
	}{
		{"zero budget checks nothing", Budget{}, true},
		{"within", Budget{MaxToolCalls: 2, MaxModelCalls: 3, MaxTokens: 100, MaxDuration: 4 * time.Second}, true},
		{"tool steps exceed usage", Budget{MaxToolCalls: 1}, false},
		{"usage exceeds model steps", Budget{MaxModelCalls: 2}, false},
		{"tokens", Budget{MaxTokens: 99}, false},
		{"duration", Budget{MaxDuration: 3 * time.Second}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grade(t, tc.budget.Grader(), trial); got.Passed != tc.passed {
				t.Fatalf("passed = %v, want %v: %+v", got.Passed, tc.passed, got)
			}
		})
	}
	if got := TraceDuration(Trace{Steps: []TraceStep{{Type: StepToolCall}}}); got != 0 {
		t.Fatalf("untimed duration = %s", got)
	}
}

func TestOutcomeStatus(t *testing.T) {
	if got := grade(t, OutcomeStatus("succeeded", "waiting_approval"), trajectoryTrial("waiting_approval", Usage{})); !got.Passed {
		t.Fatalf("allowed = %+v", got)
	}
	got := grade(t, OutcomeStatus("succeeded"), trajectoryTrial("failed", Usage{}))
	if got.Passed || got.Assertions[0].Message != `outcome status is "failed"` {
		t.Fatalf("disallowed = %+v", got)
	}
}

func TestTrajectoryGradersIgnoreContent(t *testing.T) {
	trial := trajectoryTrial("succeeded", Usage{}, TraceStep{Type: StepToolCall, Name: "search", Status: StatusSucceeded, Message: "delete"})
	trial.Output = "called delete"
	if got := grade(t, ForbidTools("delete"), trial); !got.Passed {
		t.Fatalf("content leaked into grading: %+v", got)
	}
}

func TestRecordedHarnessGradesRecordings(t *testing.T) {
	harness := RecordedHarness{
		"good": {Outcome: Outcome{Status: "succeeded"}, Trace: Trace{Steps: []TraceStep{tool("search", StatusSucceeded)}}},
		"bad":  {Outcome: Outcome{Status: "succeeded"}, Trace: Trace{Steps: []TraceStep{tool("delete", StatusSucceeded)}}},
	}
	suite := Suite{
		Name:    "recorded",
		Tasks:   []Task{{ID: "good"}, {ID: "bad"}, {ID: "missing"}},
		Graders: []Grader{RequireTools("search"), ForbidTools("delete")},
	}
	result, err := Runner{Harness: harness}.Run(context.Background(), suite)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.PassedTrials != 1 || result.Summary.FailedTrials != 2 {
		t.Fatalf("summary = %+v", result.Summary)
	}
	if !result.Trials[0].Passed || result.Trials[1].Passed || result.Trials[2].Trial.Error != `no recorded result for task "missing"` {
		t.Fatalf("trials = %+v", result.Trials)
	}
	if _, err := (Runner{Harness: harness, TrialsPerTask: 2}).Run(context.Background(), suite); err == nil {
		t.Fatal("recorded harness accepted repeated trials")
	}
}
