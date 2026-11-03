package workflowkit

import (
	"context"
	"errors"
	"time"
)

type Step interface {
	Name() string
	Run(context.Context, WorkflowRun) (StepResult, error)
}

type StepResult struct {
	Status        Status
	OutputRef     string
	AgentRunID    string
	AuditRef      string
	Error         string
	ApprovalRef   string
	WaitingReason string
	Metadata      map[string]any
}

// Executor runs workflow steps in order and persists progress after each one.
// Every write is guarded: it only applies while the stored run is still
// running under the executor's lease owner, so a cancelled, failed, or
// reclaimed run returns ErrRunPreempted instead of being overwritten.
//
// If ctx is cancelled, the executor stops without recording a terminal state;
// the caller owns finalization. Queued runs left running this way are failed
// by QueueRecoveryStore.FailExpiredRunning once their lease expires.
type Executor struct {
	store       Store
	steps       []Step
	retryPolicy RetryPolicy
}

type Approval struct {
	AuditRef string
	Metadata map[string]any
}

type Option func(*Executor)

func WithRetryPolicy(policy RetryPolicy) Option {
	return func(e *Executor) {
		e.retryPolicy = policy
	}
}

func NewExecutor(store Store, steps []Step, options ...Option) *Executor {
	executor := &Executor{
		store: store,
		steps: append([]Step(nil), steps...),
	}
	for _, option := range options {
		option(executor)
	}
	return executor
}

func (e *Executor) Run(ctx context.Context, run WorkflowRun) (WorkflowRun, error) {
	if run.Status == "" {
		run.Status = StatusPending
	}
	if run.Status != StatusPending {
		return run, InvalidTransitionError{From: run.Status, To: StatusRunning, Op: "run"}
	}
	started, err := e.start(ctx, run)
	if err != nil {
		return started, err
	}
	return e.runFrom(ctx, started)
}

// start moves a pending run to running. A stored run must still be pending
// and held by the same lease owner, so two workers cannot both start it.
func (e *Executor) start(ctx context.Context, run WorkflowRun) (WorkflowRun, error) {
	run.Status = StatusRunning
	if e.store == nil {
		return run, nil
	}
	observed := run
	started, err := e.store.Update(ctx, run.ID, func(current WorkflowRun) (WorkflowRun, error) {
		observed = current
		if current.Status != StatusPending {
			return current, InvalidTransitionError{From: current.Status, To: StatusRunning, Op: "run"}
		}
		if current.LeaseOwner != run.LeaseOwner {
			return current, runPreemptedError(current)
		}
		run.LeaseOwner = current.LeaseOwner
		run.LeaseUntil = current.LeaseUntil
		run.CreatedAt = current.CreatedAt
		return run, nil
	})
	if errors.Is(err, ErrRunNotFound) {
		if err := e.store.Save(ctx, run); err != nil {
			return run, err
		}
		return run, nil
	}
	if err != nil {
		return observed, err
	}
	return started, nil
}

func (e *Executor) Continue(ctx context.Context, id string) (WorkflowRun, error) {
	return e.resumeWaiting(ctx, id, "continue", func(*WorkflowRun) {})
}

func (e *Executor) Approve(ctx context.Context, id string, approval Approval) (WorkflowRun, error) {
	return e.resumeWaiting(ctx, id, "approve", func(run *WorkflowRun) {
		if approval.AuditRef != "" {
			run.AuditRef = approval.AuditRef
		}
		if len(approval.Metadata) > 0 {
			if run.Metadata == nil {
				run.Metadata = make(map[string]any, len(approval.Metadata))
			}
			for key, value := range approval.Metadata {
				run.Metadata[key] = value
			}
		}
	})
}

// resumeWaiting checks and leaves waiting_approval in one store update, so
// concurrent approvals cannot both run the remaining steps.
func (e *Executor) resumeWaiting(ctx context.Context, id string, op string, apply func(*WorkflowRun)) (WorkflowRun, error) {
	if e.store == nil {
		return WorkflowRun{}, ErrRunNotFound
	}
	var observed WorkflowRun
	run, err := e.store.Update(ctx, id, func(current WorkflowRun) (WorkflowRun, error) {
		observed = current
		if current.Status != StatusWaitingApproval {
			return current, InvalidTransitionError{From: current.Status, To: StatusRunning, Op: op}
		}
		apply(&current)
		current.Status = StatusRunning
		return current, nil
	})
	if err != nil {
		return observed, err
	}
	return e.runFrom(ctx, run)
}

// Cancel marks a non-terminal run cancelled. An executor still running it
// stops at its next state write instead of overwriting the cancellation.
func (e *Executor) Cancel(ctx context.Context, id string, reason string) (WorkflowRun, error) {
	if e.store == nil {
		return WorkflowRun{}, ErrRunNotFound
	}
	var observed WorkflowRun
	run, err := e.store.Update(ctx, id, func(current WorkflowRun) (WorkflowRun, error) {
		observed = current
		if isTerminal(current.Status) {
			return current, InvalidTransitionError{From: current.Status, To: StatusCancelled, Op: "cancel"}
		}
		current.Status = StatusCancelled
		current.Error = reason
		return current, nil
	})
	if err != nil {
		return observed, err
	}
	return run, nil
}

func (e *Executor) runFrom(ctx context.Context, run WorkflowRun) (WorkflowRun, error) {
	for _, step := range e.steps {
		if stepCompleted(run, step.Name()) {
			continue
		}
		run.CurrentStep = step.Name()
		if err := e.save(ctx, run); err != nil {
			return run, err
		}
		result, err := e.runStepWithRetry(ctx, step, &run)
		if err != nil {
			if errors.Is(err, ErrRunPreempted) {
				return run, err
			}
			// Failed steps may still return diagnostic references such as an
			// AgentRunID. Preserve them before persisting the terminal failure.
			applyStepResult(&run, result)
			run.Status = StatusFailed
			run.Error = err.Error()
			// After cancellation the caller owns finalization, so a save
			// rejected by the cancelled context is expected and not reported.
			if saveErr := e.save(ctx, run); saveErr != nil && ctx.Err() == nil {
				return run, errors.Join(err, saveErr)
			}
			return run, err
		}
		applyStepResult(&run, result)
		if !isFailedOrCancelled(run.Status) {
			markStepCompleted(&run, step.Name())
		}
		if err := e.save(ctx, run); err != nil {
			return run, err
		}
		if isTerminalOrWaiting(run.Status) {
			return run, nil
		}
	}

	run.Status = StatusSucceeded
	if err := e.save(ctx, run); err != nil {
		return run, err
	}
	return run, nil
}

func (e *Executor) runStepWithRetry(ctx context.Context, step Step, run *WorkflowRun) (StepResult, error) {
	maxAttempts := e.retryPolicy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	var lastErr error
	var lastResult StepResult
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		recordStepAttempt(run, step.Name())
		record := StepRecord{
			Name:      step.Name(),
			Status:    StatusRunning,
			Attempt:   run.StepAttempts[step.Name()],
			StartedAt: time.Now(),
		}
		if err := e.save(ctx, *run); err != nil {
			return StepResult{}, err
		}
		result, err := step.Run(ctx, cloneRun(*run))
		if err == nil {
			err = validateStepResult(result)
		}
		if err == nil {
			finishStepRecord(&record, result, "")
			run.StepRecords = append(run.StepRecords, record)
			if err := e.save(ctx, *run); err != nil {
				return StepResult{}, err
			}
			return result, nil
		}
		lastErr = err
		failedResult := result
		failedResult.Status = StatusFailed
		failedResult.Error = err.Error()
		lastResult = failedResult
		finishStepRecord(&record, failedResult, err.Error())
		run.StepRecords = append(run.StepRecords, record)
		if err := e.save(ctx, *run); err != nil {
			return StepResult{}, err
		}
		if errors.Is(err, ErrInvalidStatus) {
			return failedResult, err
		}
		if !IsTransient(err) || attempt == maxAttempts {
			return failedResult, err
		}
		if e.retryPolicy.Delay > 0 {
			timer := time.NewTimer(e.retryPolicy.Delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return StepResult{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return lastResult, lastErr
}

func finishStepRecord(record *StepRecord, result StepResult, err string) {
	status := result.Status
	if status == "" {
		status = StatusSucceeded
	}
	record.Status = status
	record.OutputRef = result.OutputRef
	record.AgentRunID = result.AgentRunID
	record.AuditRef = result.AuditRef
	record.ApprovalRef = result.ApprovalRef
	record.WaitingReason = result.WaitingReason
	record.Metadata = cloneMetadata(result.Metadata)
	record.Error = result.Error
	if err != "" {
		record.Status = StatusFailed
		record.Error = err
	}
	record.EndedAt = time.Now()
}

func validateStepResult(result StepResult) error {
	if result.Status == "" {
		return nil
	}
	if !result.Status.IsValid() || result.Status == StatusPending {
		return InvalidStatusError{Status: result.Status, Field: "step result"}
	}
	return nil
}

// save writes executor progress only while the stored run is still running
// under the same lease owner. Lease fields are kept from the store because
// heartbeats extend them independently of the executor's copy.
func (e *Executor) save(ctx context.Context, run WorkflowRun) error {
	if e.store == nil {
		return nil
	}
	_, err := e.store.Update(ctx, run.ID, func(current WorkflowRun) (WorkflowRun, error) {
		if current.Status != StatusRunning || current.LeaseOwner != run.LeaseOwner {
			return current, runPreemptedError(current)
		}
		run.LeaseOwner = current.LeaseOwner
		run.LeaseUntil = current.LeaseUntil
		run.CreatedAt = current.CreatedAt
		return run, nil
	})
	return err
}

func applyStepResult(run *WorkflowRun, result StepResult) {
	if result.Status != "" {
		run.Status = result.Status
	}
	if result.OutputRef != "" {
		run.OutputRef = result.OutputRef
	}
	if result.AgentRunID != "" {
		run.AgentRunID = result.AgentRunID
	}
	if result.AuditRef != "" {
		run.AuditRef = result.AuditRef
	}
	if result.Error != "" {
		run.Error = result.Error
	}
	if result.ApprovalRef != "" {
		run.ApprovalRef = result.ApprovalRef
	}
	if result.WaitingReason != "" {
		run.WaitingReason = result.WaitingReason
	}
	if len(result.Metadata) > 0 {
		if run.Metadata == nil {
			run.Metadata = make(map[string]any, len(result.Metadata))
		}
		for key, value := range result.Metadata {
			run.Metadata[key] = value
		}
	}
}

func isTerminalOrWaiting(status Status) bool {
	switch status {
	case StatusWaitingApproval, StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

func isTerminal(status Status) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

func isFailedOrCancelled(status Status) bool {
	return status == StatusFailed || status == StatusCancelled
}

func stepCompleted(run WorkflowRun, name string) bool {
	for _, completed := range run.CompletedSteps {
		if completed == name {
			return true
		}
	}
	return false
}

func markStepCompleted(run *WorkflowRun, name string) {
	if stepCompleted(*run, name) {
		return
	}
	run.CompletedSteps = append(run.CompletedSteps, name)
}

func recordStepAttempt(run *WorkflowRun, name string) {
	if run.StepAttempts == nil {
		run.StepAttempts = make(map[string]int)
	}
	run.StepAttempts[name]++
}
