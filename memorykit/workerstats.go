package memorykit

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrWorkerBacklogUnavailable reports a worker built without a backlog reader:
// neither the config nor its store can count the durable backlog.
var ErrWorkerBacklogUnavailable = errors.New("memory worker backlog reader unavailable")

// WorkerErrorClass is a fixed, low-cardinality label for why a worker run
// failed. It never carries error text, so it cannot leak memory content.
type WorkerErrorClass string

const (
	WorkerErrorContextCanceled         WorkerErrorClass = "context_canceled"
	WorkerErrorDeadlineExceeded        WorkerErrorClass = "deadline_exceeded"
	WorkerErrorPendingReadFailed       WorkerErrorClass = "pending_read_failed"
	WorkerErrorPendingResultInvalid    WorkerErrorClass = "pending_result_invalid"
	WorkerErrorProviderFailed          WorkerErrorClass = "provider_failed"
	WorkerErrorProviderResultInvalid   WorkerErrorClass = "provider_result_invalid"
	WorkerErrorEmbeddingWriteFailed    WorkerErrorClass = "embedding_write_failed"
	WorkerErrorClaimFailed             WorkerErrorClass = "claim_failed"
	WorkerErrorSourceReadFailed        WorkerErrorClass = "source_read_failed"
	WorkerErrorCandidateExtractFailed  WorkerErrorClass = "candidate_extract_failed"
	WorkerErrorCandidateValidateFailed WorkerErrorClass = "candidate_validate_failed"
	WorkerErrorCandidateWriteFailed    WorkerErrorClass = "candidate_write_failed"
)

// WorkerSnapshot is a content-free view of one worker. Backlog is read from
// the durable store when the snapshot is taken; the other fields are counters
// of the current process and reset on restart. A non-idle RunOnce counts as
// one success or one failure; an idle run counts as neither. A later success
// does not clear LastErrorClass.
type WorkerSnapshot struct {
	Backlog            int64
	SuccessTotal       uint64
	FailureTotal       uint64
	LastErrorClass     WorkerErrorClass
	LastTransitionTime time.Time
}

type workerStats struct {
	mu    sync.RWMutex
	state WorkerSnapshot
}

func (s *workerStats) recordSuccess(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.SuccessTotal++
	s.state.LastTransitionTime = workerTransitionTime(at)
}

func (s *workerStats) recordFailure(class WorkerErrorClass, at time.Time) {
	if !validWorkerErrorClass(class) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.FailureTotal++
	s.state.LastErrorClass = class
	s.state.LastTransitionTime = workerTransitionTime(at)
}

func (s *workerStats) snapshot() WorkerSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func workerTransitionTime(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now().UTC()
	}
	return at.UTC()
}

func validWorkerErrorClass(class WorkerErrorClass) bool {
	switch class {
	case WorkerErrorContextCanceled,
		WorkerErrorDeadlineExceeded,
		WorkerErrorPendingReadFailed,
		WorkerErrorPendingResultInvalid,
		WorkerErrorProviderFailed,
		WorkerErrorProviderResultInvalid,
		WorkerErrorEmbeddingWriteFailed,
		WorkerErrorClaimFailed,
		WorkerErrorSourceReadFailed,
		WorkerErrorCandidateExtractFailed,
		WorkerErrorCandidateValidateFailed,
		WorkerErrorCandidateWriteFailed:
		return true
	default:
		return false
	}
}

// classedWorkerError tags an error with the class of the step that produced
// it, so a caller further up can record the class without parsing the text.
type classedWorkerError struct {
	class WorkerErrorClass
	err   error
}

func (e *classedWorkerError) Error() string { return e.err.Error() }
func (e *classedWorkerError) Unwrap() error { return e.err }

func withWorkerErrorClass(class WorkerErrorClass, err error) error {
	if err == nil {
		return nil
	}
	return &classedWorkerError{class: class, err: err}
}

// classifyWorkerError prefers cancellation, then a class tagged on err, then
// the caller's fallback.
func classifyWorkerError(ctx context.Context, err error, fallback WorkerErrorClass) WorkerErrorClass {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return WorkerErrorDeadlineExceeded
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return WorkerErrorContextCanceled
	}
	var classed *classedWorkerError
	if errors.As(err, &classed) {
		return classed.class
	}
	return fallback
}
