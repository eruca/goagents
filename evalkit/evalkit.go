package evalkit

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Task is one eval case with stable input and reader-facing success criteria.
type Task struct {
	ID              string
	Input           string
	SuccessCriteria string
	Metadata        map[string]any
}

// Suite groups tasks and graders that should be evaluated together.
type Suite struct {
	Name    string
	Tasks   []Task
	Graders []Grader
}

// Harness adapts an agent, workflow, or service endpoint into evalkit.
type Harness interface {
	RunTask(context.Context, Task) (*RunResult, error)
}

type HarnessFunc func(context.Context, Task) (*RunResult, error)

func (f HarnessFunc) RunTask(ctx context.Context, task Task) (*RunResult, error) {
	return f(ctx, task)
}

type RunResult struct {
	Output   string
	Outcome  Outcome
	Trace    Trace
	Metadata map[string]any
}

// Outcome is the evaluated system's domain result. OutputRef points to
// host-owned content without copying that content into an eval report.
type Outcome struct {
	Status    string
	OutputRef string
	ErrorCode string
	Metadata  map[string]any
}

// Trace is the bounded trajectory record used by graders. Hosts decide how much
// detail to map in from runtime events, logs, or workflow records.
type Trace struct {
	RunID  string
	Steps  []TraceStep
	Usage  Usage
	Labels map[string]string
}

type TraceStep struct {
	Type      string
	Name      string
	Status    string
	Message   string
	StartedAt time.Time
	EndedAt   time.Time
	Metadata  map[string]any
}

type Usage struct {
	InputTokens  int
	OutputTokens int
	ToolCalls    int
	LLMCalls     int
}

type Grader interface {
	Name() string
	Grade(context.Context, GradeRequest) (*GradeResult, error)
}

type GraderFunc struct {
	GraderName string
	Fn         func(context.Context, GradeRequest) (*GradeResult, error)
}

func (g GraderFunc) Name() string {
	return g.GraderName
}

func (g GraderFunc) Grade(ctx context.Context, req GradeRequest) (*GradeResult, error) {
	if g.Fn == nil {
		return nil, fmt.Errorf("grader %q has no function", g.GraderName)
	}
	return g.Fn(ctx, req)
}

type GradeRequest struct {
	Task  Task
	Trial Trial
}

// GradeResult is one grader's decision for a trial. Assertions, when present,
// are aggregated into Passed so callers do not need to duplicate that logic.
type GradeResult struct {
	Name       string
	Score      float64
	Passed     bool
	Message    string
	Assertions []Assertion
	Metadata   map[string]any
}

type Assertion struct {
	Name    string
	Passed  bool
	Message string
}

type Trial struct {
	TaskID     string
	Index      int
	Output     string
	Outcome    Outcome
	Trace      Trace
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration
	Metadata   map[string]any
}

// TrialResult combines one harness attempt with all grader outputs.
type TrialResult struct {
	Trial   Trial
	Grades  []GradeResult
	Passed  bool
	Message string
}

type Summary struct {
	TotalTrials  int
	PassedTrials int
	FailedTrials int
	TaskCount    int
	GraderCount  int
	Duration     time.Duration
}

type SuiteResult struct {
	Name       string
	StartedAt  time.Time
	FinishedAt time.Time
	Summary    Summary
	Trials     []TrialResult
}

// Runner executes a suite sequentially. It intentionally does not own
// concurrency, durable storage, or model-as-judge infrastructure.
type Runner struct {
	Harness       Harness
	TrialsPerTask int
	Now           func() time.Time
}

func (r Runner) Run(ctx context.Context, suite Suite) (*SuiteResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.Harness == nil {
		return nil, fmt.Errorf("eval harness is required")
	}
	trialsPerTask := r.TrialsPerTask
	if trialsPerTask <= 0 {
		trialsPerTask = 1
	}
	if err := validateSuite(suite); err != nil {
		return nil, err
	}

	started := r.now()
	result := &SuiteResult{
		Name:      suite.Name,
		StartedAt: started,
	}
	for _, task := range suite.Tasks {
		task := cloneTask(task)
		for i := 1; i <= trialsPerTask; i++ {
			trialResult, err := r.runTrial(ctx, task, i, suite.Graders)
			if err != nil {
				return nil, err
			}
			result.Trials = append(result.Trials, trialResult)
		}
	}
	result.FinishedAt = r.now()
	result.Summary = summarize(*result, len(suite.Tasks), len(suite.Graders))
	return result, nil
}

func (r Runner) runTrial(ctx context.Context, task Task, index int, graders []Grader) (TrialResult, error) {
	if err := ctx.Err(); err != nil {
		return TrialResult{}, err
	}
	started := r.now()
	run, err := r.Harness.RunTask(ctx, task)
	finished := r.now()

	trial := Trial{
		TaskID:     task.ID,
		Index:      index,
		StartedAt:  started,
		FinishedAt: finished,
		Duration:   finished.Sub(started),
	}
	if run != nil {
		trial.Output = run.Output
		trial.Outcome = cloneOutcome(run.Outcome)
		trial.Trace = cloneTrace(run.Trace)
		trial.Metadata = cloneMetadata(run.Metadata)
	}
	if err != nil {
		trial.Error = err.Error()
		return TrialResult{
			Trial:   trial,
			Passed:  false,
			Message: trial.Error,
		}, nil
	}

	out := TrialResult{
		Trial:  trial,
		Passed: true,
	}
	for _, grader := range graders {
		grade := runGrader(ctx, grader, task, trial)
		out.Grades = append(out.Grades, grade)
		if !grade.Passed {
			out.Passed = false
		}
	}
	return out, nil
}

func runGrader(ctx context.Context, grader Grader, task Task, trial Trial) GradeResult {
	name := grader.Name()
	if strings.TrimSpace(name) == "" {
		name = "unnamed"
	}
	result, err := grader.Grade(ctx, GradeRequest{Task: task, Trial: trial})
	if err != nil {
		return GradeResult{Name: name, Passed: false, Message: err.Error()}
	}
	if result == nil {
		return GradeResult{Name: name, Passed: false, Message: "grader returned nil result"}
	}
	grade := cloneGradeResult(*result)
	if strings.TrimSpace(grade.Name) == "" {
		grade.Name = name
	}
	if len(grade.Assertions) > 0 {
		grade.Passed = assertionsPassed(grade.Assertions)
	}
	return grade
}

func validateSuite(suite Suite) error {
	if len(suite.Tasks) == 0 {
		return fmt.Errorf("eval suite requires at least one task")
	}
	seen := map[string]struct{}{}
	for i, task := range suite.Tasks {
		id := strings.TrimSpace(task.ID)
		if id == "" {
			return fmt.Errorf("task %d id is required", i)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate task id %q", id)
		}
		seen[id] = struct{}{}
	}
	for i, grader := range suite.Graders {
		if grader == nil {
			return fmt.Errorf("grader %d is nil", i)
		}
	}
	return nil
}

func assertionsPassed(assertions []Assertion) bool {
	for _, assertion := range assertions {
		if !assertion.Passed {
			return false
		}
	}
	return true
}

func summarize(result SuiteResult, taskCount int, graderCount int) Summary {
	summary := Summary{
		TotalTrials: len(result.Trials),
		TaskCount:   taskCount,
		GraderCount: graderCount,
		Duration:    result.FinishedAt.Sub(result.StartedAt),
	}
	for _, trial := range result.Trials {
		if trial.Passed {
			summary.PassedTrials++
		} else {
			summary.FailedTrials++
		}
	}
	return summary
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func cloneTask(task Task) Task {
	task.Metadata = cloneMetadata(task.Metadata)
	return task
}

func cloneOutcome(outcome Outcome) Outcome {
	outcome.Metadata = cloneMetadata(outcome.Metadata)
	return outcome
}

func cloneTrace(trace Trace) Trace {
	trace.Steps = cloneTraceSteps(trace.Steps)
	if len(trace.Labels) > 0 {
		labels := make(map[string]string, len(trace.Labels))
		for key, value := range trace.Labels {
			labels[key] = value
		}
		trace.Labels = labels
	}
	return trace
}

func cloneTraceSteps(steps []TraceStep) []TraceStep {
	if len(steps) == 0 {
		return nil
	}
	out := make([]TraceStep, len(steps))
	for i, step := range steps {
		step.Metadata = cloneMetadata(step.Metadata)
		out[i] = step
	}
	return out
}

func cloneGradeResult(result GradeResult) GradeResult {
	result.Assertions = append([]Assertion(nil), result.Assertions...)
	result.Metadata = cloneMetadata(result.Metadata)
	return result
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]any, len(metadata))
	for key, value := range metadata {
		out[key] = value
	}
	return out
}
