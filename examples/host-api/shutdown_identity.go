package main

import (
	"context"
	"sync"
)

// pendingShutdownIdentity names one checkpoint that was committed by the
// current execution but has not yet been linked from durable workflow state.
type pendingShutdownIdentity struct {
	CheckpointID   string
	RunID          string
	TenantID       string
	DefinitionHash string
}

type pendingShutdownTracker struct {
	mu       sync.Mutex
	identity pendingShutdownIdentity
	present  bool
}

func newPendingShutdownTracker() *pendingShutdownTracker {
	return &pendingShutdownTracker{}
}

func (t *pendingShutdownTracker) Remember(identity pendingShutdownIdentity) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.identity = identity
	t.present = true
	t.mu.Unlock()
}

func (t *pendingShutdownTracker) Snapshot() (pendingShutdownIdentity, bool) {
	if t == nil {
		return pendingShutdownIdentity{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.identity, t.present
}

func (t *pendingShutdownTracker) Clear(checkpointID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.present && t.identity.CheckpointID == checkpointID {
		t.identity = pendingShutdownIdentity{}
		t.present = false
	}
	t.mu.Unlock()
}

type pendingShutdownTrackerContextKey struct{}

func withPendingShutdownTracker(ctx context.Context, tracker *pendingShutdownTracker) context.Context {
	return context.WithValue(ctx, pendingShutdownTrackerContextKey{}, tracker)
}

func rememberPendingShutdownIdentity(ctx context.Context, identity pendingShutdownIdentity) {
	tracker, _ := ctx.Value(pendingShutdownTrackerContextKey{}).(*pendingShutdownTracker)
	tracker.Remember(identity)
}
