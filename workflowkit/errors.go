package workflowkit

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidTransition = errors.New("invalid workflow status transition")
	ErrInvalidStatus     = errors.New("invalid workflow status")
	// ErrRunPreempted reports that a running workflow was cancelled, failed, or
	// claimed by another lease owner, so the executor stopped writing to it.
	ErrRunPreempted = errors.New("workflow run preempted")
	// ErrApprovalMismatch reports an approval that names a different approval
	// request than the one the run is waiting on.
	ErrApprovalMismatch = errors.New("approval does not match the pending approval request")
)

func runPreemptedError(current WorkflowRun) error {
	return fmt.Errorf("%w: workflow %s is %s with lease owner %q", ErrRunPreempted, current.ID, current.Status, current.LeaseOwner)
}

type InvalidTransitionError struct {
	From Status
	To   Status
	Op   string
}

func (e InvalidTransitionError) Error() string {
	return fmt.Sprintf("%s: %s cannot transition from %q to %q", ErrInvalidTransition, e.Op, e.From, e.To)
}

func (e InvalidTransitionError) Unwrap() error {
	return ErrInvalidTransition
}

type InvalidStatusError struct {
	Status Status
	Field  string
}

func (e InvalidStatusError) Error() string {
	return fmt.Sprintf("%s: %s has unsupported status %q", ErrInvalidStatus, e.Field, e.Status)
}

func (e InvalidStatusError) Unwrap() error {
	return ErrInvalidStatus
}
