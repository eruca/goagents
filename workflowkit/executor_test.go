package workflowkit

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
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

func TestExecutorPreservesFailedStepDiagnosticRefs(t *testing.T) {
	store := NewMemoryStore()
	executor := NewExecutor(store, []Step{
		stepFunc{name: "agent", run: func(context.Context, WorkflowRun) (StepResult, error) {
			return StepResult{
				Status:     StatusFailed,
				AgentRunID: "agent-failed",
				AuditRef:   "audit-failed",
			}, errors.New("agent failed")
		}},
	})

	result, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-failed-refs"})
	if err == nil || err.Error() != "agent failed" {
		t.Fatalf("Run error = %v, want agent failed", err)
	}
	if result.Status != StatusFailed || result.AgentRunID != "agent-failed" || result.AuditRef != "audit-failed" {
		t.Fatalf("result = %#v, want failed diagnostic refs", result)
	}
	if len(result.StepRecords) != 1 || result.StepRecords[0].AgentRunID != "agent-failed" || result.StepRecords[0].AuditRef != "audit-failed" {
		t.Fatalf("step records = %#v, want failed diagnostic refs", result.StepRecords)
	}
	stored, err := store.Get(context.Background(), result.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if stored.AgentRunID != result.AgentRunID || stored.AuditRef != result.AuditRef {
		t.Fatalf("stored = %#v, want persisted failed diagnostic refs", stored)
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

func claimedRun(t *testing.T, store *MemoryStore, id string) WorkflowRun {
	t.Helper()
	if err := store.Save(context.Background(), WorkflowRun{ID: id, Status: StatusPending}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	claimed, err := store.ClaimRunnable(context.Background(), "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("ClaimRunnable returned error: %v", err)
	}
	return claimed
}

func TestExecutorKeepsLeaseExtendedDuringRun(t *testing.T) {
	store := NewMemoryStore()
	claimed := claimedRun(t, store, "wf-lease")
	var extendedUntil time.Time
	executor := NewExecutor(store, []Step{
		stepFunc{name: "work", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			extended, err := store.ExtendLease(ctx, run.ID, "worker-1", time.Hour)
			if err != nil {
				t.Fatalf("ExtendLease returned error: %v", err)
			}
			extendedUntil = extended.LeaseUntil
			return StepResult{}, nil
		}},
	})

	if _, err := executor.Run(context.Background(), claimed); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	stored, err := store.Get(context.Background(), "wf-lease")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if stored.Status != StatusSucceeded || !stored.LeaseUntil.Equal(extendedUntil) || stored.LeaseOwner != "worker-1" {
		t.Fatalf("stored = %+v, want succeeded with heartbeat lease %v kept", stored, extendedUntil)
	}
	if _, err := store.ExtendLease(context.Background(), "wf-lease", "worker-1", time.Minute); err != nil {
		t.Fatalf("heartbeat after executor writes returned error: %v", err)
	}
}

func TestExecutorStopsWhenRunIsCancelledMidStep(t *testing.T) {
	store := NewMemoryStore()
	var executor *Executor
	secondRan := false
	executor = NewExecutor(store, []Step{
		stepFunc{name: "first", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			if _, err := executor.Cancel(ctx, run.ID, "operator stop"); err != nil {
				t.Fatalf("Cancel returned error: %v", err)
			}
			return StepResult{Status: StatusSucceeded}, nil
		}},
		stepFunc{name: "second", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			secondRan = true
			return StepResult{}, nil
		}},
	})

	_, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-cancel-mid", Status: StatusPending})
	if !errors.Is(err, ErrRunPreempted) {
		t.Fatalf("Run err = %v, want ErrRunPreempted", err)
	}
	if secondRan {
		t.Fatal("second step ran after cancellation")
	}
	stored, err := store.Get(context.Background(), "wf-cancel-mid")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if stored.Status != StatusCancelled || stored.Error != "operator stop" {
		t.Fatalf("stored = %+v, want cancellation kept", stored)
	}
}

// rendezvousStore pairs up Get calls so each pair waits for the other. A
// check-then-act approval then lets both callers observe waiting_approval.
type rendezvousStore struct {
	*MemoryStore
	arrivals *atomic.Int32
	pairs    []chan struct{}
}

func newRendezvousStore(memory *MemoryStore) rendezvousStore {
	pairs := make([]chan struct{}, 8)
	for i := range pairs {
		pairs[i] = make(chan struct{})
	}
	return rendezvousStore{MemoryStore: memory, arrivals: new(atomic.Int32), pairs: pairs}
}

func (s rendezvousStore) Get(ctx context.Context, id string) (WorkflowRun, error) {
	run, err := s.MemoryStore.Get(ctx, id)
	n := int(s.arrivals.Add(1)) - 1
	if n/2 >= len(s.pairs) {
		return run, err
	}
	if n%2 == 1 {
		close(s.pairs[n/2])
		return run, err
	}
	select {
	case <-s.pairs[n/2]:
	case <-time.After(time.Second):
	}
	return run, err
}

func TestExecutorConcurrentApprovalsRunRemainingStepsOnce(t *testing.T) {
	memory := NewMemoryStore()
	if err := memory.Save(context.Background(), WorkflowRun{ID: "wf-double-approve", Status: StatusWaitingApproval, CompletedSteps: []string{"draft"}}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	store := newRendezvousStore(memory)
	var finalizeRuns atomic.Int32
	executor := NewExecutor(store, []Step{
		stepFunc{name: "draft", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			return StepResult{}, errors.New("completed step ran again")
		}},
		stepFunc{name: "finalize", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			finalizeRuns.Add(1)
			return StepResult{Status: StatusSucceeded}, nil
		}},
	})

	errs := make(chan error, 2)
	for _, audit := range []string{"audit:a", "audit:b"} {
		go func() {
			_, err := executor.Approve(context.Background(), "wf-double-approve", Approval{AuditRef: audit})
			errs <- err
		}()
	}
	var succeeded, rejected int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrInvalidTransition):
			rejected++
		default:
			t.Fatalf("Approve err = %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 || finalizeRuns.Load() != 1 {
		t.Fatalf("succeeded=%d rejected=%d finalize runs=%d, want one winner", succeeded, rejected, finalizeRuns.Load())
	}
}

func TestExecutorStopsWritingAfterLeaseOwnerChanges(t *testing.T) {
	store := NewMemoryStore()
	claimed := claimedRun(t, store, "wf-stolen")
	executor := NewExecutor(store, []Step{
		stepFunc{name: "work", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			if _, err := store.Update(ctx, run.ID, func(current WorkflowRun) (WorkflowRun, error) {
				current.LeaseOwner = "worker-2"
				return current, nil
			}); err != nil {
				t.Fatalf("Update returned error: %v", err)
			}
			return StepResult{Status: StatusSucceeded, OutputRef: "artifact:zombie"}, nil
		}},
	})

	if _, err := executor.Run(context.Background(), claimed); !errors.Is(err, ErrRunPreempted) {
		t.Fatalf("Run err = %v, want ErrRunPreempted", err)
	}
	stored, err := store.Get(context.Background(), "wf-stolen")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if stored.Status != StatusRunning || stored.OutputRef != "" || stored.LeaseOwner != "worker-2" {
		t.Fatalf("stored = %+v, want new owner's state untouched", stored)
	}
}

func TestExecutorRunRejectsPendingRunClaimedByAnotherOwner(t *testing.T) {
	store := NewMemoryStore()
	stale := claimedRun(t, store, "wf-reclaimed")
	if _, err := store.Update(context.Background(), stale.ID, func(current WorkflowRun) (WorkflowRun, error) {
		current.LeaseOwner = "worker-2"
		return current, nil
	}); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	executor := NewExecutor(store, []Step{
		stepFunc{name: "work", run: func(ctx context.Context, run WorkflowRun) (StepResult, error) {
			t.Fatal("stale owner ran a step")
			return StepResult{}, nil
		}},
	})

	if _, err := executor.Run(context.Background(), stale); !errors.Is(err, ErrRunPreempted) {
		t.Fatalf("Run err = %v, want ErrRunPreempted", err)
	}
}

func TestRetryPolicyBackoffIsJitteredAndCapped(t *testing.T) {
	policy := RetryPolicy{BaseDelay: 10 * time.Millisecond, MaxDelay: 40 * time.Millisecond}
	for retry, ceiling := range map[int]time.Duration{1: 10, 2: 20, 3: 40, 10: 40} {
		ceiling *= time.Millisecond
		seen := map[time.Duration]bool{}
		for range 200 {
			delay := policy.Backoff(retry)
			if delay < 0 || delay > ceiling {
				t.Fatalf("Backoff(%d) = %s, want within [0, %s]", retry, delay, ceiling)
			}
			seen[delay] = true
		}
		if len(seen) < 10 {
			t.Fatalf("Backoff(%d) produced %d distinct delays, want jitter", retry, len(seen))
		}
	}
	if got := (RetryPolicy{}).Backoff(3); got != 0 {
		t.Fatalf("Backoff without BaseDelay = %s, want 0", got)
	}
}

func TestExecutorYieldsLeasedRunBetweenRetriesAndKeepsBudgetAcrossClaims(t *testing.T) {
	store := NewMemoryStore()
	attempts := 0
	executor := NewExecutor(store, []Step{
		stepFunc{name: "flaky", run: func(context.Context, WorkflowRun) (StepResult, error) {
			attempts++
			return StepResult{}, TransientError{Err: errors.New("temporary outage")}
		}},
	}, WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BaseDelay: time.Hour}))
	if err := store.Save(context.Background(), WorkflowRun{ID: "wf-yield", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}

	claimAndRun := func() (WorkflowRun, error) {
		t.Helper()
		claimed, err := store.ClaimRunnable(context.Background(), "worker-1", time.Minute)
		if err != nil {
			t.Fatalf("ClaimRunnable returned error: %v", err)
		}
		run, runErr := executor.Run(context.Background(), claimed)
		if _, err := store.ReleaseLease(context.Background(), run.ID, "worker-1"); err != nil {
			t.Fatalf("ReleaseLease returned error: %v", err)
		}
		return run, runErr
	}
	makeDue := func() {
		t.Helper()
		if _, err := store.Update(context.Background(), "wf-yield", func(run WorkflowRun) (WorkflowRun, error) {
			run.NextAttemptAt = time.Now().Add(-time.Second)
			return run, nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	for retry := 1; retry <= 2; retry++ {
		before := time.Now()
		run, err := claimAndRun()
		if err != nil {
			t.Fatalf("attempt %d: Run returned error: %v", retry, err)
		}
		if run.Status != StatusPending || run.StepRetries != retry || run.NextAttemptAt.Before(before) {
			t.Fatalf("attempt %d: run = %+v, want pending retry %d scheduled ahead", retry, run, retry)
		}
		if _, err := store.ClaimRunnable(context.Background(), "worker-2", time.Minute); !errors.Is(err, ErrNoRunnableWorkflow) {
			t.Fatalf("attempt %d: claim before retry is due = %v, want none", retry, err)
		}
		makeDue()
	}

	run, err := claimAndRun()
	if err == nil || run.Status != StatusFailed {
		t.Fatalf("final run = %+v, %v; want failure after the budget", run, err)
	}
	if attempts != 3 || run.StepAttempts["flaky"] != 3 || run.StepRetries != 0 {
		t.Fatalf("attempts = %d, run = %+v; want 3 attempts and a reset retry count", attempts, run)
	}
}

func TestExecutorWaitsInProcessForUnleasedRetry(t *testing.T) {
	store := NewMemoryStore()
	attempts := 0
	executor := NewExecutor(store, []Step{
		stepFunc{name: "flaky", run: func(context.Context, WorkflowRun) (StepResult, error) {
			attempts++
			if attempts < 3 {
				return StepResult{}, TransientError{Err: errors.New("temporary outage")}
			}
			return StepResult{}, nil
		}},
	}, WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond}))

	run, err := executor.Run(context.Background(), WorkflowRun{ID: "wf-sync-retry"})
	if err != nil || run.Status != StatusSucceeded || attempts != 3 || run.StepRetries != 0 {
		t.Fatalf("run = %+v, %v, attempts = %d; want success after in-process retries", run, err, attempts)
	}
}
