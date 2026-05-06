package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eruca/runkit"
	"github.com/eruca/runkit/storetest"
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
