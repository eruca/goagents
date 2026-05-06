package runkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var ErrRunNotFound = errors.New("run not found")

type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type RunRecord struct {
	RunID      string
	WorkflowID string
	TaskID     string
	Status     Status
	Summary    TerminalSummary
	Metadata   map[string]any
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type TerminalSummary struct {
	Status       Status
	ContentRef   string
	AbortReason  string
	InputTokens  int
	OutputTokens int
	LLMCalls     int
	ToolCalls    int
	UsedTools    []string
}

type RunEvent struct {
	RunID      string
	Sequence   int
	Type       string
	Stage      string
	Iteration  int
	Message    string
	Metadata   map[string]any
	RecordedAt time.Time
}

type Store interface {
	Create(context.Context, RunRecord) error
	Get(context.Context, string) (RunRecord, error)
	AppendEvent(context.Context, RunEvent) error
	Events(context.Context, string) ([]RunEvent, error)
	Complete(context.Context, string, TerminalSummary) error
	FindByWorkflowID(context.Context, string) ([]RunRecord, error)
}

type MemoryStore struct {
	mu      sync.RWMutex
	runs    map[string]RunRecord
	events  map[string][]RunEvent
	order   []string
	nowFunc func() time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		runs:    map[string]RunRecord{},
		events:  map[string][]RunEvent{},
		nowFunc: time.Now,
	}
}

func (s *MemoryStore) Create(ctx context.Context, record RunRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(record.RunID) == "" {
		return fmt.Errorf("run id is required")
	}
	now := s.now()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[record.RunID]; !exists {
		s.order = append(s.order, record.RunID)
	}
	s.runs[record.RunID] = cloneRunRecord(record)
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, runID string) (RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return RunRecord{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.runs[runID]
	if !ok {
		return RunRecord{}, fmt.Errorf("%w: %s", ErrRunNotFound, runID)
	}
	return cloneRunRecord(record), nil
}

func (s *MemoryStore) AppendEvent(ctx context.Context, event RunEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(event.RunID) == "" {
		return fmt.Errorf("run id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[event.RunID]; !ok {
		return fmt.Errorf("%w: %s", ErrRunNotFound, event.RunID)
	}
	event.Sequence = len(s.events[event.RunID]) + 1
	if event.RecordedAt.IsZero() {
		event.RecordedAt = s.now()
	}
	s.events[event.RunID] = append(s.events[event.RunID], cloneRunEvent(event))
	return nil
}

func (s *MemoryStore) Events(ctx context.Context, runID string) ([]RunEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.runs[runID]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, runID)
	}
	return cloneRunEvents(s.events[runID]), nil
}

func (s *MemoryStore) Complete(ctx context.Context, runID string, summary TerminalSummary) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRunNotFound, runID)
	}
	record.Status = summary.Status
	record.Summary = cloneTerminalSummary(summary)
	record.UpdatedAt = s.now()
	s.runs[runID] = cloneRunRecord(record)
	return nil
}

func (s *MemoryStore) FindByWorkflowID(ctx context.Context, workflowID string) ([]RunRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []RunRecord
	for _, runID := range s.order {
		record := s.runs[runID]
		if record.WorkflowID == workflowID {
			out = append(out, cloneRunRecord(record))
		}
	}
	return out, nil
}

func (s *MemoryStore) now() time.Time {
	if s.nowFunc == nil {
		return time.Now()
	}
	return s.nowFunc()
}

func cloneRunRecord(record RunRecord) RunRecord {
	record.Metadata = cloneMetadata(record.Metadata)
	record.Summary = cloneTerminalSummary(record.Summary)
	return record
}

func cloneTerminalSummary(summary TerminalSummary) TerminalSummary {
	summary.UsedTools = append([]string(nil), summary.UsedTools...)
	return summary
}

func cloneRunEvent(event RunEvent) RunEvent {
	event.Metadata = cloneMetadata(event.Metadata)
	return event
}

func cloneRunEvents(events []RunEvent) []RunEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]RunEvent, len(events))
	for i, event := range events {
		out[i] = cloneRunEvent(event)
	}
	return out
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
