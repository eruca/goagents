package workflowkit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

var ErrRunNotFound = errors.New("workflow run not found")
var ErrNoRunnableWorkflow = errors.New("no runnable workflow")
var ErrWorkflowLeaseNotOwned = errors.New("workflow lease not owned")
var ErrRunExists = errors.New("workflow run already exists")

type Store interface {
	Save(ctx context.Context, run WorkflowRun) error
	Get(ctx context.Context, id string) (WorkflowRun, error)
	Update(ctx context.Context, id string, mutate func(WorkflowRun) (WorkflowRun, error)) (WorkflowRun, error)
}

// CreateStore adds an insert that never replaces a stored run: Create fails
// with ErrRunExists when a run with the same id exists. Save is an upsert, so
// a host that accepts caller-chosen ids must create through Create.
type CreateStore interface {
	Store
	Create(ctx context.Context, run WorkflowRun) error
}

type WorkflowOrder string

const (
	WorkflowOrderAsc  WorkflowOrder = "asc"
	WorkflowOrderDesc WorkflowOrder = "desc"
)

func (o WorkflowOrder) IsValid() bool {
	switch o {
	case "", WorkflowOrderAsc, WorkflowOrderDesc:
		return true
	default:
		return false
	}
}

type WorkflowQuery struct {
	Status         Status
	MetadataEquals map[string]string
	Order          WorkflowOrder
	Limit          int
}

type WorkflowQueryStore interface {
	Store
	ListWorkflows(ctx context.Context, query WorkflowQuery) ([]WorkflowRun, error)
}

type QueueStore interface {
	Store
	ClaimRunnable(ctx context.Context, workerID string, lease time.Duration) (WorkflowRun, error)
}

// QueueLeaseStore extends and releases claimed leases. ReleaseLease clears
// the lease of a run that is no longer running. A run still running keeps
// its lease owner and its lease expires at once instead: the executor stopped
// without recording an outcome, so QueueRecoveryStore.FailExpiredRunning must
// still find and fail it.
type QueueLeaseStore interface {
	QueueStore
	ExtendLease(ctx context.Context, id string, workerID string, lease time.Duration) (WorkflowRun, error)
	ReleaseLease(ctx context.Context, id string, workerID string) (WorkflowRun, error)
}

// QueueRecoveryStore fails running workflows whose lease expired, which means
// their worker stopped heartbeating. Recovery is fail-closed: the interrupted
// step may already have had side effects, so it is never re-executed
// automatically. A host can requeue the failed run after inspecting it.
// Runs without a lease owner are never touched.
type QueueRecoveryStore interface {
	QueueLeaseStore
	FailExpiredRunning(ctx context.Context, reason string) ([]WorkflowRun, error)
}

type MemoryStore struct {
	mu   sync.RWMutex
	runs map[string]WorkflowRun
	now  func() time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		runs: make(map[string]WorkflowRun),
		now:  time.Now,
	}
}

func (s *MemoryStore) Save(ctx context.Context, run WorkflowRun) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = s.now()
	}
	run.UpdatedAt = s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[run.ID] = cloneRun(run)
	return nil
}

func (s *MemoryStore) Create(ctx context.Context, run WorkflowRun) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = s.now()
	}
	run.UpdatedAt = s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[run.ID]; exists {
		return ErrRunExists
	}
	s.runs[run.ID] = cloneRun(run)
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, id string) (WorkflowRun, error) {
	select {
	case <-ctx.Done():
		return WorkflowRun{}, ctx.Err()
	default:
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	run, ok := s.runs[id]
	if !ok {
		return WorkflowRun{}, ErrRunNotFound
	}
	return cloneRun(run), nil
}

func (s *MemoryStore) Update(ctx context.Context, id string, mutate func(WorkflowRun) (WorkflowRun, error)) (WorkflowRun, error) {
	select {
	case <-ctx.Done():
		return WorkflowRun{}, ctx.Err()
	default:
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.runs[id]
	if !ok {
		return WorkflowRun{}, ErrRunNotFound
	}
	updated, err := mutate(cloneRun(current))
	if err != nil {
		return WorkflowRun{}, err
	}
	if updated.CreatedAt.IsZero() {
		updated.CreatedAt = current.CreatedAt
	}
	updated.UpdatedAt = s.now()
	s.runs[id] = cloneRun(updated)
	return cloneRun(updated), nil
}

func (s *MemoryStore) ListWorkflows(ctx context.Context, query WorkflowQuery) ([]WorkflowRun, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if query.Status != "" && !query.Status.IsValid() {
		return nil, fmt.Errorf("invalid workflow status: %s", query.Status)
	}
	if !query.Order.IsValid() {
		return nil, fmt.Errorf("invalid workflow order: %s", query.Order)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	runs := make([]WorkflowRun, 0, len(s.runs))
	for _, run := range s.runs {
		if query.Status != "" && run.Status != query.Status {
			continue
		}
		if !workflowMetadataMatches(run, query.MetadataEquals) {
			continue
		}
		runs = append(runs, cloneRun(run))
	}
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			if query.Order == WorkflowOrderDesc {
				return runs[i].CreatedAt.After(runs[j].CreatedAt)
			}
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		if query.Order == WorkflowOrderDesc {
			return runs[i].ID > runs[j].ID
		}
		return runs[i].ID < runs[j].ID
	})
	if query.Limit > 0 && len(runs) > query.Limit {
		runs = runs[:query.Limit]
	}
	return runs, nil
}

func workflowMetadataMatches(run WorkflowRun, equals map[string]string) bool {
	for key, want := range equals {
		got, ok := run.Metadata[key]
		if !ok {
			return false
		}
		if MetadataValueString(got) != want {
			return false
		}
	}
	return true
}

// MetadataValueString is the string WorkflowQuery.MetadataEquals compares a
// metadata value against. Numbers format the same whether a store kept the
// original int or decoded JSON into float64, so 1234567 matches "1234567" in
// every store rather than "1.234567e+06" in some.
func MetadataValueString(value any) string {
	switch v := value.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	default:
		return fmt.Sprint(value)
	}
}

func (s *MemoryStore) ClaimRunnable(ctx context.Context, workerID string, lease time.Duration) (WorkflowRun, error) {
	select {
	case <-ctx.Done():
		return WorkflowRun{}, ctx.Err()
	default:
	}
	if workerID == "" {
		return WorkflowRun{}, fmt.Errorf("worker id is required")
	}
	if lease <= 0 {
		return WorkflowRun{}, fmt.Errorf("lease must be greater than zero")
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left := s.runs[ids[i]]
		right := s.runs[ids[j]]
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.ID < right.ID
	})

	for _, id := range ids {
		run := s.runs[id]
		if run.Status != StatusPending || run.NextAttemptAt.After(now) {
			continue
		}
		if run.LeaseOwner != "" && run.LeaseUntil.After(now) {
			continue
		}
		run.LeaseOwner = workerID
		run.LeaseUntil = now.Add(lease)
		run.UpdatedAt = now
		s.runs[id] = cloneRun(run)
		return cloneRun(run), nil
	}
	return WorkflowRun{}, ErrNoRunnableWorkflow
}

func (s *MemoryStore) ExtendLease(ctx context.Context, id string, workerID string, lease time.Duration) (WorkflowRun, error) {
	select {
	case <-ctx.Done():
		return WorkflowRun{}, ctx.Err()
	default:
	}
	if id == "" {
		return WorkflowRun{}, fmt.Errorf("workflow id is required")
	}
	if workerID == "" {
		return WorkflowRun{}, fmt.Errorf("worker id is required")
	}
	if lease <= 0 {
		return WorkflowRun{}, fmt.Errorf("lease must be greater than zero")
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[id]
	if !ok {
		return WorkflowRun{}, ErrRunNotFound
	}
	if run.LeaseOwner != workerID || run.LeaseUntil.IsZero() || !run.LeaseUntil.After(now) {
		return WorkflowRun{}, ErrWorkflowLeaseNotOwned
	}
	run.LeaseUntil = now.Add(lease)
	run.UpdatedAt = now
	s.runs[id] = cloneRun(run)
	return cloneRun(run), nil
}

func (s *MemoryStore) ReleaseLease(ctx context.Context, id string, workerID string) (WorkflowRun, error) {
	select {
	case <-ctx.Done():
		return WorkflowRun{}, ctx.Err()
	default:
	}
	if id == "" {
		return WorkflowRun{}, fmt.Errorf("workflow id is required")
	}
	if workerID == "" {
		return WorkflowRun{}, fmt.Errorf("worker id is required")
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[id]
	if !ok {
		return WorkflowRun{}, ErrRunNotFound
	}
	if run.LeaseOwner != workerID {
		return WorkflowRun{}, ErrWorkflowLeaseNotOwned
	}
	if run.Status == StatusRunning {
		run.LeaseUntil = now
	} else {
		run.LeaseOwner = ""
		run.LeaseUntil = time.Time{}
	}
	run.UpdatedAt = now
	s.runs[id] = cloneRun(run)
	return cloneRun(run), nil
}

func (s *MemoryStore) FailExpiredRunning(ctx context.Context, reason string) ([]WorkflowRun, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	var failed []WorkflowRun
	for id, run := range s.runs {
		if run.Status != StatusRunning || run.LeaseOwner == "" || run.LeaseUntil.After(now) {
			continue
		}
		run.Status = StatusFailed
		run.Error = reason
		run.StepRetries = 0
		run.NextAttemptAt = time.Time{}
		run.LeaseOwner = ""
		run.LeaseUntil = time.Time{}
		run.UpdatedAt = now
		s.runs[id] = cloneRun(run)
		failed = append(failed, cloneRun(run))
	}
	sort.Slice(failed, func(i, j int) bool {
		return failed[i].ID < failed[j].ID
	})
	return failed, nil
}
