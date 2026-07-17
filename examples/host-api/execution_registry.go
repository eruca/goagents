package main

import (
	"context"
	"sync"
)

type executionKind string

const (
	executionSyncWorkflow   executionKind = "sync_workflow"
	executionQueuedWorkflow executionKind = "queued_workflow"
	executionFinalApproval  executionKind = "final_approval"
	executionAgentApproval  executionKind = "agent_approval"
)

type executionCleanup func(context.Context) error

type executionSnapshot struct {
	workflowID string
	kind       executionKind
	done       <-chan struct{}
	cleanup    executionCleanup
}

type executionEntry struct {
	workflowID string
	kind       executionKind
	done       chan struct{}
	cleanup    executionCleanup
}

type executionRegistry struct {
	mu        sync.Mutex
	accepting bool
	nextID    uint64
	active    map[uint64]*executionEntry
	// idle is closed while there are no active executions. Begin replaces it
	// on the 0 -> 1 transition so Wait never races with a WaitGroup.Add.
	idle chan struct{}
}

type executionHandle struct {
	registry *executionRegistry
	id       uint64
	once     sync.Once
}

func newExecutionRegistry() *executionRegistry {
	idle := make(chan struct{})
	close(idle)
	return &executionRegistry{
		accepting: true,
		active:    make(map[uint64]*executionEntry),
		idle:      idle,
	}
}

func (r *executionRegistry) Begin(workflowID string, kind executionKind, cleanup executionCleanup) (*executionHandle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.accepting {
		return nil, false
	}
	if len(r.active) == 0 {
		r.idle = make(chan struct{})
	}
	r.nextID++
	entry := &executionEntry{
		workflowID: workflowID,
		kind:       kind,
		done:       make(chan struct{}),
		cleanup:    cleanup,
	}
	r.active[r.nextID] = entry
	return &executionHandle{registry: r, id: r.nextID}, true
}

func (r *executionRegistry) BeginDrain() {
	r.mu.Lock()
	r.accepting = false
	r.mu.Unlock()
}

func (r *executionRegistry) Snapshot() []executionSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshots := make([]executionSnapshot, 0, len(r.active))
	for _, entry := range r.active {
		snapshots = append(snapshots, executionSnapshot{
			workflowID: entry.workflowID,
			kind:       entry.kind,
			done:       entry.done,
			cleanup:    entry.cleanup,
		})
	}
	return snapshots
}

func (r *executionRegistry) Wait(ctx context.Context) error {
	r.mu.Lock()
	idle := r.idle
	r.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *executionHandle) Done() {
	h.once.Do(func() {
		r := h.registry
		r.mu.Lock()
		defer r.mu.Unlock()
		entry, ok := r.active[h.id]
		if !ok {
			return
		}
		delete(r.active, h.id)
		close(entry.done)
		if len(r.active) == 0 {
			close(r.idle)
		}
	})
}
