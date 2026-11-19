package evalkit

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Trace step types shared by trace adapters and trajectory graders. Adapters
// that produce other types remain valid; trajectory graders only interpret
// these.
const (
	StepWorkflow   = "workflow_step"
	StepToolCall   = "tool_call"
	StepModelCall  = "model_call"
	StepApproval   = "approval"
	StepAgentEvent = "agent_event"
)

// Trace step statuses interpreted by trajectory graders.
const (
	StatusSucceeded       = "succeeded"
	StatusFailed          = "failed"
	StatusRejected        = "rejected"
	StatusWaitingApproval = "waiting_approval"
	StatusRunning         = "running"
)

// StepsOfType returns the steps of one type in trace order.
func StepsOfType(trace Trace, stepType string) []TraceStep {
	var steps []TraceStep
	for _, step := range trace.Steps {
		if step.Type == stepType {
			steps = append(steps, step)
		}
	}
	return steps
}

// ToolCalls returns the tool call steps in trace order.
func ToolCalls(trace Trace) []TraceStep { return StepsOfType(trace, StepToolCall) }

// ModelCalls returns the model call steps in trace order.
func ModelCalls(trace Trace) []TraceStep { return StepsOfType(trace, StepModelCall) }

// TraceDuration is the span from the earliest step start to the latest step
// end. Steps without timestamps are ignored; it is zero when no step has one.
func TraceDuration(trace Trace) time.Duration {
	var first, last time.Time
	for _, step := range trace.Steps {
		for _, at := range []time.Time{step.StartedAt, step.EndedAt} {
			if at.IsZero() {
				continue
			}
			if first.IsZero() || at.Before(first) {
				first = at
			}
			if last.IsZero() || at.After(last) {
				last = at
			}
		}
	}
	return last.Sub(first)
}

// trajectoryGrader grades a trial from its outcome and trace only. It never
// reads Output or step messages, so it can run on content-free production
// traces.
type trajectoryGrader struct {
	name string
	fn   func(Trial) ([]Assertion, error)
}

func (g trajectoryGrader) Name() string { return g.name }

func (g trajectoryGrader) Grade(_ context.Context, req GradeRequest) (*GradeResult, error) {
	assertions, err := g.fn(req.Trial)
	if err != nil {
		return nil, err
	}
	if len(assertions) == 0 {
		return &GradeResult{Name: g.name, Passed: true, Score: 1}, nil
	}
	passed := 0
	for _, assertion := range assertions {
		if assertion.Passed {
			passed++
		}
	}
	return &GradeResult{
		Name:       g.name,
		Score:      float64(passed) / float64(len(assertions)),
		Assertions: assertions,
	}, nil
}

func cleanNames(names []string) []string {
	cleaned := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			cleaned = append(cleaned, name)
		}
	}
	return cleaned
}

func toolNames(trace Trace) []string {
	calls := ToolCalls(trace)
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.Name)
	}
	return names
}

// RequireTools passes when every named tool was called at least once.
func RequireTools(names ...string) Grader {
	names = cleanNames(names)
	return trajectoryGrader{name: "trajectory.require_tools", fn: func(trial Trial) ([]Assertion, error) {
		if len(names) == 0 {
			return nil, fmt.Errorf("require_tools needs at least one tool name")
		}
		called := map[string]bool{}
		for _, name := range toolNames(trial.Trace) {
			called[name] = true
		}
		assertions := make([]Assertion, 0, len(names))
		for _, name := range names {
			assertion := Assertion{Name: "called " + name, Passed: called[name]}
			if !assertion.Passed {
				assertion.Message = fmt.Sprintf("tool %q was not called", name)
			}
			assertions = append(assertions, assertion)
		}
		return assertions, nil
	}}
}

// ForbidTools passes when none of the named tools was called.
func ForbidTools(names ...string) Grader {
	names = cleanNames(names)
	return trajectoryGrader{name: "trajectory.forbid_tools", fn: func(trial Trial) ([]Assertion, error) {
		if len(names) == 0 {
			return nil, fmt.Errorf("forbid_tools needs at least one tool name")
		}
		calls := map[string]int{}
		for _, name := range toolNames(trial.Trace) {
			calls[name]++
		}
		assertions := make([]Assertion, 0, len(names))
		for _, name := range names {
			assertion := Assertion{Name: "did not call " + name, Passed: calls[name] == 0}
			if !assertion.Passed {
				assertion.Message = fmt.Sprintf("tool %q was called %d time(s)", name, calls[name])
			}
			assertions = append(assertions, assertion)
		}
		return assertions, nil
	}}
}

// ToolOrder passes when the tool call sequence contains the named tools as an
// ordered subsequence; other calls may come before, between, or after them.
func ToolOrder(names ...string) Grader {
	names = cleanNames(names)
	return trajectoryGrader{name: "trajectory.tool_order", fn: func(trial Trial) ([]Assertion, error) {
		if len(names) == 0 {
			return nil, fmt.Errorf("tool_order needs at least one tool name")
		}
		next := 0
		for _, name := range toolNames(trial.Trace) {
			if next < len(names) && name == names[next] {
				next++
			}
		}
		assertion := Assertion{Name: "tool order " + strings.Join(names, " > "), Passed: next == len(names)}
		if !assertion.Passed {
			assertion.Message = fmt.Sprintf("matched %d of %d tools; next expected %q", next, len(names), names[next])
		}
		return []Assertion{assertion}, nil
	}}
}

// NoFailedSteps passes when no step of the given types failed or was rejected.
// With no types it checks every step.
func NoFailedSteps(stepTypes ...string) Grader {
	stepTypes = cleanNames(stepTypes)
	return trajectoryGrader{name: "trajectory.no_failed_steps", fn: func(trial Trial) ([]Assertion, error) {
		checked := map[string]bool{}
		for _, stepType := range stepTypes {
			checked[stepType] = true
		}
		var assertions []Assertion
		for i, step := range trial.Trace.Steps {
			if len(checked) > 0 && !checked[step.Type] {
				continue
			}
			if step.Status != StatusFailed && step.Status != StatusRejected {
				continue
			}
			assertions = append(assertions, Assertion{
				Name:    fmt.Sprintf("step %d %s %s", i, step.Type, step.Name),
				Message: fmt.Sprintf("step %s %q is %s", step.Type, step.Name, step.Status),
			})
		}
		if len(assertions) == 0 {
			assertions = []Assertion{{Name: "no failed steps", Passed: true}}
		}
		return assertions, nil
	}}
}

// Budget limits a trajectory's cost. A zero field is not checked. Tool and
// model calls count the larger of the trace's steps and its Usage counters, so
// a trace that only reports usage is still bounded. Tokens are input plus
// output tokens from Usage. Duration is TraceDuration.
type Budget struct {
	MaxToolCalls  int
	MaxModelCalls int
	MaxTokens     int
	MaxDuration   time.Duration
}

// Grader returns a grader that fails each exceeded limit.
func (b Budget) Grader() Grader {
	return trajectoryGrader{name: "trajectory.budget", fn: func(trial Trial) ([]Assertion, error) {
		if b.MaxToolCalls < 0 || b.MaxModelCalls < 0 || b.MaxTokens < 0 || b.MaxDuration < 0 {
			return nil, fmt.Errorf("budget limits must not be negative")
		}
		trace := trial.Trace
		var assertions []Assertion
		check := func(name string, used, limit int) {
			if limit == 0 {
				return
			}
			assertion := Assertion{Name: name, Passed: used <= limit}
			if !assertion.Passed {
				assertion.Message = fmt.Sprintf("used %d, limit %d", used, limit)
			}
			assertions = append(assertions, assertion)
		}
		check("tool calls", max(len(ToolCalls(trace)), trace.Usage.ToolCalls), b.MaxToolCalls)
		check("model calls", max(len(ModelCalls(trace)), trace.Usage.LLMCalls), b.MaxModelCalls)
		check("tokens", trace.Usage.InputTokens+trace.Usage.OutputTokens, b.MaxTokens)
		if b.MaxDuration > 0 {
			duration := TraceDuration(trace)
			assertion := Assertion{Name: "duration", Passed: duration <= b.MaxDuration}
			if !assertion.Passed {
				assertion.Message = fmt.Sprintf("took %s, limit %s", duration, b.MaxDuration)
			}
			assertions = append(assertions, assertion)
		}
		return assertions, nil
	}}
}

// OutcomeStatus passes when the trial's Outcome.Status is one of statuses.
func OutcomeStatus(statuses ...string) Grader {
	statuses = cleanNames(statuses)
	return trajectoryGrader{name: "trajectory.outcome_status", fn: func(trial Trial) ([]Assertion, error) {
		if len(statuses) == 0 {
			return nil, fmt.Errorf("outcome_status needs at least one status")
		}
		status := trial.Outcome.Status
		assertion := Assertion{Name: "outcome status in " + strings.Join(statuses, ","), Passed: false}
		for _, allowed := range statuses {
			if status == allowed {
				assertion.Passed = true
			}
		}
		if !assertion.Passed {
			assertion.Message = fmt.Sprintf("outcome status is %q", status)
		}
		return []Assertion{assertion}, nil
	}}
}

// RecordedHarness grades already recorded results, such as traces built from
// production runs, without running anything. It returns the result recorded
// under Task.ID. Runner rejects it with more than one trial per task, since
// repeating a recording measures nothing.
type RecordedHarness map[string]*RunResult

func (h RecordedHarness) RunTask(_ context.Context, task Task) (*RunResult, error) {
	result, ok := h[task.ID]
	if !ok || result == nil {
		return nil, fmt.Errorf("no recorded result for task %q", task.ID)
	}
	return result, nil
}
