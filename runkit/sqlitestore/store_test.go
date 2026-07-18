package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/runkit/storetest"
)

func TestStoreConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) runkit.Store {
		store, err := Open(filepath.Join(t.TempDir(), "runkit.db"))
		if err != nil {
			t.Fatalf("Open returned error: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Fatalf("Close returned error: %v", err)
			}
		})
		return store
	})
}

func TestStorePersistsRunEventsAndSummaryAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runkit.db")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	created := time.Date(2026, 5, 6, 2, 0, 0, 0, time.UTC)
	if err := store.Create(ctx, runkit.RunRecord{
		RunID:      "agent-run-sqlite",
		WorkflowID: "wf-sqlite",
		TaskID:     "agent",
		Status:     runkit.StatusRunning,
		Metadata:   map[string]any{"provider": "local", "model": "qwen"},
		CreatedAt:  created,
	}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := store.AppendEvent(ctx, runkit.RunEvent{
		RunID:     "agent-run-sqlite",
		Type:      "llm.completed",
		Stage:     "answer",
		Iteration: 2,
		Message:   "model completed",
		Metadata:  map[string]any{"latency_ms": 120},
	}); err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}
	if err := store.Complete(ctx, "agent-run-sqlite", runkit.TerminalSummary{
		Status:       runkit.StatusSucceeded,
		ContentRef:   "artifact:wf-sqlite:agent-output",
		InputTokens:  11,
		OutputTokens: 22,
		LLMCalls:     1,
		ToolCalls:    3,
		UsedTools:    []string{"search", "write"},
	}); err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen returned error: %v", err)
	}
	defer reopened.Close()

	record, err := reopened.Get(ctx, "agent-run-sqlite")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if record.Status != runkit.StatusSucceeded || record.WorkflowID != "wf-sqlite" || record.TaskID != "agent" {
		t.Fatalf("record = %+v, want persisted identity and status", record)
	}
	if record.Metadata["provider"] != "local" || record.Summary.ContentRef != "artifact:wf-sqlite:agent-output" {
		t.Fatalf("record metadata/summary = %+v", record)
	}
	if len(record.Summary.UsedTools) != 2 || record.Summary.UsedTools[1] != "write" {
		t.Fatalf("used tools = %+v, want persisted search/write", record.Summary.UsedTools)
	}

	events, err := reopened.Events(ctx, "agent-run-sqlite")
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(events) != 1 || events[0].Sequence != 1 || events[0].Type != "llm.completed" {
		t.Fatalf("events = %+v, want persisted event sequence/type", events)
	}
	if events[0].Metadata["latency_ms"] != float64(120) {
		t.Fatalf("event metadata = %+v, want persisted latency", events[0].Metadata)
	}

	runs, err := reopened.FindByWorkflowID(ctx, "wf-sqlite")
	if err != nil {
		t.Fatalf("FindByWorkflowID returned error: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != "agent-run-sqlite" {
		t.Fatalf("workflow runs = %+v, want persisted run", runs)
	}
}

func TestStoreMigratesSchemaVersion(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "runkit.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	var version int
	err = store.db.QueryRow(`SELECT version FROM runkit_schema WHERE id = 'sqlitestore'`).Scan(&version)
	if err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, SchemaVersion)
	}
}

func TestCheckpointPersistsFailedLeaseAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runkit.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now().UTC()
	checkpoint := runkit.ApprovalCheckpoint{
		ID:             "checkpoint-sqlite",
		RunID:          "run-sqlite",
		TenantID:       "tenant-sqlite",
		DefinitionHash: "agent-v1",
		Ciphertext:     []byte("ciphertext"),
		ExpiresAt:      now.Add(time.Hour),
	}
	if err := store.CreateCheckpoint(ctx, checkpoint); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if _, err := store.ApproveAndLease(ctx, runkit.ApprovalLeaseRequest{
		CheckpointID:   checkpoint.ID,
		TenantID:       checkpoint.TenantID,
		DefinitionHash: checkpoint.DefinitionHash,
		ApproverID:     "operator-1",
		AuditRef:       "audit-1",
		LeaseOwner:     "worker-1",
		LeaseDuration:  time.Minute,
		Now:            now,
	}); err != nil {
		t.Fatalf("ApproveAndLease: %v", err)
	}
	if err := store.FailLease(ctx, runkit.CheckpointLeaseCompletion{
		CheckpointID: checkpoint.ID,
		TenantID:     checkpoint.TenantID,
		LeaseOwner:   "worker-1",
		FailureCode:  "checkpoint_decode_failed",
		Now:          now.Add(time.Second),
	}); err != nil {
		t.Fatalf("FailLease: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	stored, err := reopened.GetCheckpoint(ctx, checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if stored.Status != runkit.CheckpointFailed || stored.FailureCode != "checkpoint_decode_failed" || string(stored.Ciphertext) != "ciphertext" {
		t.Fatalf("stored checkpoint = %#v", stored)
	}
	if stored.Approval == nil || stored.Approval.ApproverID != "operator-1" || !stored.Approval.Approved {
		t.Fatalf("stored approval = %#v", stored.Approval)
	}
}

func TestCheckpointAllowsOnlyOneConcurrentLease(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "runkit.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	checkpoint := runkit.ApprovalCheckpoint{
		ID:             "checkpoint-concurrent",
		RunID:          "run-concurrent",
		TenantID:       "tenant-concurrent",
		DefinitionHash: "agent-v1",
		Ciphertext:     []byte("ciphertext"),
		ExpiresAt:      now.Add(time.Hour),
	}
	if err := store.CreateCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, worker := range []string{"worker-1", "worker-2"} {
		workers.Add(1)
		go func(worker string) {
			defer workers.Done()
			_, err := store.ApproveAndLease(context.Background(), runkit.ApprovalLeaseRequest{
				CheckpointID:   checkpoint.ID,
				TenantID:       checkpoint.TenantID,
				DefinitionHash: checkpoint.DefinitionHash,
				ApproverID:     "operator-" + worker,
				AuditRef:       "audit-" + worker,
				LeaseOwner:     worker,
				LeaseDuration:  time.Minute,
				Now:            now,
			})
			results <- err
		}(worker)
	}
	workers.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, runkit.ErrCheckpointNotClaimable) {
			t.Fatalf("lease error = %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful leases = %d, want 1", succeeded)
	}
}

func TestStoreFailsOnlyExactPendingCheckpointAndPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runkit.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	checkpoint := runkit.ApprovalCheckpoint{
		ID:             "checkpoint-pending-shutdown",
		RunID:          "run-pending-shutdown",
		TenantID:       "tenant-pending-shutdown",
		DefinitionHash: "agent-v1",
		Ciphertext:     []byte("ciphertext"),
		ExpiresAt:      now.Add(time.Hour),
	}
	if err := store.CreateCheckpoint(ctx, checkpoint); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	request := runkit.PendingCheckpointFailure{
		CheckpointID:   checkpoint.ID,
		RunID:          checkpoint.RunID,
		TenantID:       checkpoint.TenantID,
		DefinitionHash: checkpoint.DefinitionHash,
		FailureCode:    "host_shutdown_timeout",
		Now:            now,
	}
	wrong := request
	wrong.DefinitionHash = "other-definition"
	if err := store.FailPendingCheckpoint(ctx, wrong); !errors.Is(err, runkit.ErrCheckpointNotClaimable) {
		t.Fatalf("wrong identity error = %v, want ErrCheckpointNotClaimable", err)
	}
	if err := store.FailPendingCheckpoint(ctx, request); err != nil {
		t.Fatalf("FailPendingCheckpoint: %v", err)
	}
	first, err := store.GetCheckpoint(ctx, checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint first: %v", err)
	}
	if first.Status != runkit.CheckpointFailed || first.FailureCode != request.FailureCode || !first.UpdatedAt.Equal(now) {
		t.Fatalf("failed checkpoint = %#v", first)
	}
	repeated := request
	repeated.Now = now.Add(time.Minute)
	if err := store.FailPendingCheckpoint(ctx, repeated); err != nil {
		t.Fatalf("idempotent FailPendingCheckpoint: %v", err)
	}
	second, err := store.GetCheckpoint(ctx, checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint second: %v", err)
	}
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("idempotent UpdatedAt = %s, want %s", second.UpdatedAt, first.UpdatedAt)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetCheckpoint(ctx, checkpoint.ID, checkpoint.TenantID)
	if err != nil {
		t.Fatalf("GetCheckpoint reopened: %v", err)
	}
	if persisted.Status != runkit.CheckpointFailed || persisted.FailureCode != request.FailureCode {
		t.Fatalf("persisted checkpoint = %#v", persisted)
	}
}
