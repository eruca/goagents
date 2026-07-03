package workflowkit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var ErrRunNotFound = errors.New("workflow run not found")
var ErrNoRunnableWorkflow = errors.New("no runnable workflow")
var ErrWorkflowLeaseNotOwned = errors.New("workflow lease not owned")

type Store interface {
	Save(ctx context.Context, run WorkflowRun) error
	Get(ctx context.Context, id string) (WorkflowRun, error)
	Update(ctx context.Context, id string, mutate func(WorkflowRun) (WorkflowRun, error)) (WorkflowRun, error)
}

type WorkflowQuery struct {
	Status Status
	Limit  int
}

type WorkflowQueryStore interface {
	Store
	ListWorkflows(ctx context.Context, query WorkflowQuery) ([]WorkflowRun, error)
}

type QueueStore interface {
	Store
	ClaimRunnable(ctx context.Context, workerID string, lease time.Duration) (WorkflowRun, error)
}

type QueueLeaseStore interface {
	QueueStore
	ExtendLease(ctx context.Context, id string, workerID string, lease time.Duration) (WorkflowRun, error)
	ReleaseLease(ctx context.Context, id string, workerID string) (WorkflowRun, error)
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

	s.mu.RLock()
	defer s.mu.RUnlock()

	runs := make([]WorkflowRun, 0, len(s.runs))
	for _, run := range s.runs {
		if query.Status != "" && run.Status != query.Status {
			continue
		}
		runs = append(runs, cloneRun(run))
	}
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		return runs[i].ID < runs[j].ID
	})
	if query.Limit > 0 && len(runs) > query.Limit {
		runs = runs[:query.Limit]
	}
	return runs, nil
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
		if run.Status != StatusPending {
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
	run.LeaseOwner = ""
	run.LeaseUntil = time.Time{}
	run.UpdatedAt = now
	s.runs[id] = cloneRun(run)
	return cloneRun(run), nil
}
