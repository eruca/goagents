package workflowkit

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type stepFunc struct {
	name string
	run  func(context.Context, WorkflowRun) (StepResult, error)
}

func (s stepFunc) Name() string {
	return s.name
}

func (s stepFunc) Run(ctx context.Context, run WorkflowRun) (StepResult, error) {
	return s.run(ctx, run)
}

func TestExecutorMarksRunSucceededAfterOrderedStepsComplete(t *testing.T) {
	store := NewMemoryStore()
	var order []string
	executor := NewExecutor(store, []Step{
		stepFunc{name: "prepare", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			order = append(order, "prepare")
			return StepResult{Status: StatusRunning, Metadata: map[string]any{"prepared": true}}, nil
		}},
		stepFunc{name: "finalize", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			order = append(order, "finalize")
			return StepResult{Status: StatusSucceeded, OutputRef: "artifact:out", AuditRef: "audit:final"}, nil
		}},
	})

	result, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-1", Status: StatusPending})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Status != StatusSucceeded || result.OutputRef != "artifact:out" || result.AuditRef != "audit:final" {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(order, []string{"prepare", "finalize"}) {
		t.Fatalf("order = %#v", order)
	}
	stored, err := store.Get(context.Background(), "wf-1")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if stored.Status != StatusSucceeded || stored.OutputRef != "artifact:out" || stored.AuditRef != "audit:final" {
		t.Fatalf("stored = %#v", stored)
	}
	if len(stored.StepRecords) != 2 {
		t.Fatalf("step records = %#v", stored.StepRecords)
	}
	if stored.StepRecords[0].Name != "prepare" || stored.StepRecords[0].Status != StatusRunning || stored.StepRecords[0].Attempt != 1 {
		t.Fatalf("prepare record = %#v", stored.StepRecords[0])
	}
	if stored.StepRecords[1].Name != "finalize" || stored.StepRecords[1].Status != StatusSucceeded || stored.StepRecords[1].OutputRef != "artifact:out" || stored.StepRecords[1].AuditRef != "audit:final" {
		t.Fatalf("finalize record = %#v", stored.StepRecords[1])
	}
}

func TestExecutorStopsAndPersistsWaitingApproval(t *testing.T) {
	store := NewMemoryStore()
	var ranSecond bool
	executor := NewExecutor(store, []Step{
		stepFunc{name: "approval", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			return StepResult{
				Status:        StatusWaitingApproval,
				AgentRunID:    "agent-1",
				ApprovalRef:   "approval:req-1",
				WaitingReason: "operator approval required",
				Metadata:      map[string]any{"tool": "write_file"},
			}, nil
		}},
		stepFunc{name: "after", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			ranSecond = true
			return StepResult{Status: StatusSucceeded}, nil
		}},
	})

	result, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-approval"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if ranSecond {
		t.Fatal("executor ran step after waiting approval")
	}
	if result.Status != StatusWaitingApproval || result.AgentRunID != "agent-1" {
		t.Fatalf("result = %#v", result)
	}
	if result.ApprovalRef != "approval:req-1" || result.WaitingReason != "operator approval required" {
		t.Fatalf("approval fields = %#v", result)
	}
	stored, err := store.Get(context.Background(), "wf-approval")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if stored.Status != StatusWaitingApproval || stored.Metadata["tool"] != "write_file" {
		t.Fatalf("stored = %#v", stored)
	}
	if stored.ApprovalRef != "approval:req-1" || stored.WaitingReason != "operator approval required" {
		t.Fatalf("stored approval fields = %#v", stored)
	}
	if stored.CurrentStep != "approval" {
		t.Fatalf("current step = %q", stored.CurrentStep)
	}
	if !reflect.DeepEqual(stored.CompletedSteps, []string{"approval"}) {
		t.Fatalf("completed steps = %#v", stored.CompletedSteps)
	}
	if len(stored.StepRecords) != 1 || stored.StepRecords[0].Status != StatusWaitingApproval {
		t.Fatalf("step records = %#v", stored.StepRecords)
	}
	if stored.StepRecords[0].ApprovalRef != "approval:req-1" || stored.StepRecords[0].WaitingReason != "operator approval required" {
		t.Fatalf("approval record = %#v", stored.StepRecords[0])
	}
}

func TestExecutorContinuesWaitingApprovalRunFromNextStep(t *testing.T) {
	store := NewMemoryStore()
	var order []string
	executor := NewExecutor(store, []Step{
		stepFunc{name: "approval", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			order = append(order, "approval")
			return StepResult{Status: StatusWaitingApproval}, nil
		}},
		stepFunc{name: "finalize", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			order = append(order, "finalize")
			return StepResult{Status: StatusSucceeded, OutputRef: "artifact:out"}, nil
		}},
	})
	first, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-resume"})
	if err != nil {
		t.Fatalf("first Run returned error: %v", err)
	}
	if first.Status != StatusWaitingApproval {
		t.Fatalf("first = %#v", first)
	}

	resumed, err := executor.Continue(context.Background(), "wf-resume")
	if err != nil {
		t.Fatalf("Continue returned error: %v", err)
	}
	if resumed.Status != StatusSucceeded || resumed.OutputRef != "artifact:out" {
		t.Fatalf("resumed = %#v", resumed)
	}
	if !reflect.DeepEqual(order, []string{"approval", "finalize"}) {
		t.Fatalf("order = %#v", order)
	}
	if !reflect.DeepEqual(resumed.CompletedSteps, []string{"approval", "finalize"}) {
		t.Fatalf("completed steps = %#v", resumed.CompletedSteps)
	}
}

func TestExecutorApproveRecordsAuditAndContinues(t *testing.T) {
	store := NewMemoryStore()
	var order []string
	executor := NewExecutor(store, []Step{
		stepFunc{name: "approval", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			order = append(order, "approval")
			return StepResult{Status: StatusWaitingApproval, ApprovalRef: "approval:req-1"}, nil
		}},
		stepFunc{name: "finalize", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			order = append(order, "finalize")
			if run.Metadata["approved_by"] != "operator-1" {
				t.Fatalf("approval metadata missing in finalize run: %#v", run.Metadata)
			}
			return StepResult{Status: StatusSucceeded, OutputRef: "artifact:out"}, nil
		}},
	})
	first, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-approve"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if first.Status != StatusWaitingApproval {
		t.Fatalf("first = %#v", first)
	}

	result, err := executor.Approve(context.Background(), "wf-approve", Approval{
		AuditRef: "audit:approval-1",
		Metadata: map[string]any{
			"approved_by": "operator-1",
		},
	})
	if err != nil {
		t.Fatalf("Approve returned error: %v", err)
	}
	if result.Status != StatusSucceeded || result.OutputRef != "artifact:out" || result.AuditRef != "audit:approval-1" {
		t.Fatalf("result = %#v", result)
	}
	if !reflect.DeepEqual(order, []string{"approval", "finalize"}) {
		t.Fatalf("order = %#v", order)
	}
}

func TestExecutorApproveRejectsNonWaitingRun(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save(context.Background(), WorkflowRun{ID: "wf-done", Status: StatusSucceeded}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	executor := NewExecutor(store, []Step{})

	result, err := executor.Approve(context.Background(), "wf-done", Approval{AuditRef: "audit:too-late"})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if result.Status != StatusSucceeded {
		t.Fatalf("result = %#v", result)
	}
}

func TestExecutorMarksRunFailedWhenStepReturnsError(t *testing.T) {
	store := NewMemoryStore()
	boom := errors.New("boom")
	executor := NewExecutor(store, []Step{
		stepFunc{name: "fail", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			return StepResult{}, boom
		}},
	})

	result, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-fail"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if result.Status != StatusFailed || result.Error != "boom" {
		t.Fatalf("result = %#v", result)
	}
	stored, err := store.Get(context.Background(), "wf-fail")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if stored.Status != StatusFailed || stored.Error != "boom" {
		t.Fatalf("stored = %#v", stored)
	}
	if len(stored.StepRecords) != 1 {
		t.Fatalf("step records = %#v", stored.StepRecords)
	}
	if stored.StepRecords[0].Name != "fail" || stored.StepRecords[0].Status != StatusFailed || stored.StepRecords[0].Error != "boom" {
		t.Fatalf("failure record = %#v", stored.StepRecords[0])
	}
}

func TestExecutorDoesNotMarkFailedStepResultCompleted(t *testing.T) {
	store := NewMemoryStore()
	var ranAfter bool
	executor := NewExecutor(store, []Step{
		stepFunc{name: "validate", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			return StepResult{Status: StatusFailed, Error: "validation failed"}, nil
		}},
		stepFunc{name: "after", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			ranAfter = true
			return StepResult{Status: StatusSucceeded}, nil
		}},
	})

	result, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-step-failed"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if ranAfter {
		t.Fatal("executor ran step after failed step result")
	}
	if result.Status != StatusFailed || result.Error != "validation failed" {
		t.Fatalf("result = %#v", result)
	}
	if len(result.CompletedSteps) != 0 {
		t.Fatalf("completed steps = %#v", result.CompletedSteps)
	}
	if len(result.StepRecords) != 1 || result.StepRecords[0].Status != StatusFailed {
		t.Fatalf("step records = %#v", result.StepRecords)
	}
}

func TestExecutorRejectsInvalidStepResultStatus(t *testing.T) {
	store := NewMemoryStore()
	executor := NewExecutor(store, []Step{
		stepFunc{name: "bad-status", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			return StepResult{Status: Status("paused")}, nil
		}},
	})

	result, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-invalid-status"})
	if !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("err = %v, want ErrInvalidStatus", err)
	}
	if result.Status != StatusFailed || result.Error == "" {
		t.Fatalf("result = %#v", result)
	}
	if len(result.CompletedSteps) != 0 {
		t.Fatalf("completed steps = %#v", result.CompletedSteps)
	}
	stored, err := store.Get(context.Background(), "wf-invalid-status")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if len(stored.StepRecords) != 1 || stored.StepRecords[0].Status != StatusFailed || stored.StepRecords[0].Error == "" {
		t.Fatalf("step records = %#v", stored.StepRecords)
	}
}

func TestExecutorRetriesTransientStepError(t *testing.T) {
	store := NewMemoryStore()
	attempts := 0
	executor := NewExecutor(store, []Step{
		stepFunc{name: "flaky", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			attempts++
			if attempts < 3 {
				return StepResult{}, TransientError{Err: errors.New("temporary outage")}
			}
			return StepResult{Status: StatusSucceeded, OutputRef: "artifact:out"}, nil
		}},
	}, WithRetryPolicy(RetryPolicy{MaxAttempts: 3}))

	result, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-retry"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d", attempts)
	}
	if result.Status != StatusSucceeded || result.OutputRef != "artifact:out" {
		t.Fatalf("result = %#v", result)
	}
	if result.StepAttempts["flaky"] != 3 {
		t.Fatalf("step attempts = %#v", result.StepAttempts)
	}
	if len(result.StepRecords) != 3 {
		t.Fatalf("step records = %#v", result.StepRecords)
	}
	if result.StepRecords[0].Status != StatusFailed || result.StepRecords[0].Attempt != 1 || result.StepRecords[0].Error != "temporary outage" {
		t.Fatalf("first retry record = %#v", result.StepRecords[0])
	}
	if result.StepRecords[1].Status != StatusFailed || result.StepRecords[1].Attempt != 2 || result.StepRecords[1].Error != "temporary outage" {
		t.Fatalf("second retry record = %#v", result.StepRecords[1])
	}
	if result.StepRecords[2].Status != StatusSucceeded || result.StepRecords[2].Attempt != 3 || result.StepRecords[2].OutputRef != "artifact:out" {
		t.Fatalf("success retry record = %#v", result.StepRecords[2])
	}
}

func TestExecutorDoesNotRetryNonTransientStepError(t *testing.T) {
	store := NewMemoryStore()
	attempts := 0
	boom := errors.New("validation failed")
	executor := NewExecutor(store, []Step{
		stepFunc{name: "validate", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			attempts++
			return StepResult{}, boom
		}},
	}, WithRetryPolicy(RetryPolicy{MaxAttempts: 3}))

	result, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-no-retry"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d", attempts)
	}
	if result.Status != StatusFailed || result.StepAttempts["validate"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestExecutorDoesNotRetryTransientErrorByDefault(t *testing.T) {
	store := NewMemoryStore()
	attempts := 0
	executor := NewExecutor(store, []Step{
		stepFunc{name: "flaky", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			attempts++
			return StepResult{}, TransientError{Err: errors.New("temporary outage")}
		}},
	})

	result, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-default-retry"})
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d", attempts)
	}
	if result.StepAttempts["flaky"] != 1 {
		t.Fatalf("step attempts = %#v", result.StepAttempts)
	}
}

func TestExecutorRunRejectsNonPendingStatus(t *testing.T) {
	executor := NewExecutor(NewMemoryStore(), []Step{})
	for _, status := range []Status{StatusRunning, StatusWaitingApproval, StatusSucceeded, StatusFailed, StatusCancelled} {
		result, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-" + string(status), Status: status})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("status %s err = %v, want ErrInvalidTransition", status, err)
		}
		var transition InvalidTransitionError
		if !errors.As(err, &transition) {
			t.Fatalf("status %s err type = %T", status, err)
		}
		if transition.From != status || transition.To != StatusRunning || transition.Op != "run" {
			t.Fatalf("transition = %#v", transition)
		}
		if result.Status != status {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestExecutorContinueRejectsNonWaitingStatus(t *testing.T) {
	store := NewMemoryStore()
	executor := NewExecutor(store, []Step{})
	for _, status := range []Status{StatusPending, StatusRunning, StatusSucceeded, StatusFailed, StatusCancelled} {
		id := "wf-continue-" + string(status)
		if err := store.Save(context.Background(), WorkflowRun{ID: id, Status: status}); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
		result, err := executor.Continue(context.Background(), id)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("status %s err = %v, want ErrInvalidTransition", status, err)
		}
		var transition InvalidTransitionError
		if !errors.As(err, &transition) {
			t.Fatalf("status %s err type = %T", status, err)
		}
		if transition.From != status || transition.To != StatusRunning || transition.Op != "continue" {
			t.Fatalf("transition = %#v", transition)
		}
		if result.Status != status {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestExecutorCancelWaitingApprovalRun(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save(context.Background(), WorkflowRun{ID: "wf-cancel", Status: StatusWaitingApproval}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	executor := NewExecutor(store, []Step{})

	result, err := executor.Cancel(context.Background(), "wf-cancel", "operator cancelled")
	if err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	if result.Status != StatusCancelled || result.Error != "operator cancelled" {
		t.Fatalf("result = %#v", result)
	}
	stored, err := store.Get(context.Background(), "wf-cancel")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if stored.Status != StatusCancelled || stored.Error != "operator cancelled" {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestExecutorCancelRejectsTerminalRun(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save(context.Background(), WorkflowRun{ID: "wf-done", Status: StatusSucceeded}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	executor := NewExecutor(store, []Step{})

	result, err := executor.Cancel(context.Background(), "wf-done", "too late")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if result.Status != StatusSucceeded {
		t.Fatalf("result = %#v", result)
	}
}
