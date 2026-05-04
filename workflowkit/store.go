package workflowkit

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrRunNotFound = errors.New("workflow run not found")

type Store interface {
	Save(ctx context.Context, run WorkflowRun) error
	Get(ctx context.Context, id string) (WorkflowRun, error)
	Update(ctx context.Context, id string, mutate func(WorkflowRun) (WorkflowRun, error)) (WorkflowRun, error)
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
