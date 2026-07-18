package main

import (
	"context"
	"sync"
	"testing"
)

func TestPendingShutdownTrackerUsesTypedContextAndReturnsStableSnapshots(t *testing.T) {
	tracker := newPendingShutdownTracker()
	ctx := withPendingShutdownTracker(context.Background(), tracker)
	identity := pendingShutdownIdentity{
		CheckpointID:   "checkpoint-typed",
		RunID:          "run-typed",
		TenantID:       "tenant-typed",
		DefinitionHash: "agent-v1",
	}
	rememberPendingShutdownIdentity(ctx, identity)

	const readers = 16
	var workers sync.WaitGroup
	for range readers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			got, ok := tracker.Snapshot()
			if !ok || got != identity {
				t.Errorf("Snapshot() = %+v, %v, want %+v, true", got, ok, identity)
			}
		}()
	}
	workers.Wait()

	tracker.Clear(identity.CheckpointID)
	if got, ok := tracker.Snapshot(); ok {
		t.Fatalf("Snapshot() after Clear = %+v, true, want empty", got)
	}
}

func TestRememberPendingShutdownIdentityWithoutTrackerIsNoOp(t *testing.T) {
	rememberPendingShutdownIdentity(context.Background(), pendingShutdownIdentity{
		CheckpointID:   "checkpoint-no-tracker",
		RunID:          "run-no-tracker",
		TenantID:       "tenant-no-tracker",
		DefinitionHash: "agent-v1",
	})
}
