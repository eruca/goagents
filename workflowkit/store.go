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

type Store interface {
	Save(ctx context.Context, run WorkflowRun) error
	Get(ctx context.Context, id string) (WorkflowRun, error)
	Update(ctx context.Context, id string, mutate func(WorkflowRun) (WorkflowRun, error)) (WorkflowRun, error)
}

type QueueStore interface {
	Store
	ClaimRunnable(ctx context.Context, workerID string, lease time.Duration) (WorkflowRun, error)
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
