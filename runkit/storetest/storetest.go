package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eruca/runkit"
)

type NewStore func(*testing.T) runkit.Store

func RunStoreConformance(t *testing.T, newStore NewStore) {
	t.Helper()

	t.Run("create append complete returns copies", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()

		record := runkit.RunRecord{
			RunID:      "agent-run-1",
			WorkflowID: "wf-1",
			TaskID:     "task-1",
			Status:     runkit.StatusRunning,
			Metadata:   map[string]any{"source": "test"},
		}
		if err := store.Create(ctx, record); err != nil {
			t.Fatalf("Create returned error: %v", err)
		}
		record.Metadata["source"] = "mutated"

		first := runkit.RunEvent{
			RunID:      "agent-run-1",
			Type:       "stage.started",
			Stage:      "think",
			Sequence:   10,
			RecordedAt: time.Date(2026, 5, 6, 1, 0, 0, 0, time.UTC),
			Metadata:   map[string]any{"iteration": 1},
		}
		if err := store.AppendEvent(ctx, first); err != nil {
			t.Fatalf("AppendEvent returned error: %v", err)
		}
		first.Metadata["iteration"] = 99
		if err := store.AppendEvent(ctx, runkit.RunEvent{
			RunID: "agent-run-1",
			Type:  "finalized",
		}); err != nil {
			t.Fatalf("AppendEvent second returned error: %v", err)
		}

		if err := store.Complete(ctx, "agent-run-1", runkit.TerminalSummary{
			Status:       runkit.StatusSucceeded,
			ContentRef:   "artifact:wf-1:agent-output",
			InputTokens:  10,
			OutputTokens: 20,
			LLMCalls:     1,
			ToolCalls:    2,
			UsedTools:    []string{"lookup", "write"},
		}); err != nil {
			t.Fatalf("Complete returned error: %v", err)
		}

		got, err := store.Get(ctx, "agent-run-1")
		if err != nil {
			t.Fatalf("Get returned error: %v", err)
		}
		if got.Status != runkit.StatusSucceeded || got.WorkflowID != "wf-1" || got.TaskID != "task-1" {
			t.Fatalf("record identity/status = %+v, want succeeded wf/task", got)
		}
		if got.Metadata["source"] != "test" {
			t.Fatalf("record metadata source = %v, want copied test", got.Metadata["source"])
		}
		if got.Summary.ContentRef != "artifact:wf-1:agent-output" || got.Summary.OutputTokens != 20 {
			t.Fatalf("summary = %+v, want terminal summary", got.Summary)
		}
		if len(got.Summary.UsedTools) != 2 || got.Summary.UsedTools[0] != "lookup" {
			t.Fatalf("used tools = %+v, want lookup/write", got.Summary.UsedTools)
		}

		events, err := store.Events(ctx, "agent-run-1")
		if err != nil {
			t.Fatalf("Events returned error: %v", err)
		}
		if len(events) != 2 {
			t.Fatalf("events len = %d, want 2", len(events))
		}
		if events[0].Sequence != 1 || events[1].Sequence != 2 {
			t.Fatalf("event sequences = %d/%d, want store assigned 1/2", events[0].Sequence, events[1].Sequence)
		}
		if events[0].Metadata["iteration"] != float64(1) && events[0].Metadata["iteration"] != 1 {
			t.Fatalf("event metadata iteration = %v, want copied 1", events[0].Metadata["iteration"])
		}

		events[0].Metadata["iteration"] = 42
		again, err := store.Events(ctx, "agent-run-1")
		if err != nil {
			t.Fatalf("Events second returned error: %v", err)
		}
		if again[0].Metadata["iteration"] == 42 {
			t.Fatalf("events leaked mutable state: %+v", again[0].Metadata)
		}
	})

	t.Run("finds runs by workflow id", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		for _, record := range []runkit.RunRecord{
			{RunID: "agent-run-1", WorkflowID: "wf-1", Status: runkit.StatusRunning},
			{RunID: "agent-run-2", WorkflowID: "wf-1", Status: runkit.StatusSucceeded},
			{RunID: "agent-run-3", WorkflowID: "wf-2", Status: runkit.StatusRunning},
		} {
			if err := store.Create(ctx, record); err != nil {
				t.Fatalf("Create(%s) returned error: %v", record.RunID, err)
			}
		}

		runs, err := store.FindByWorkflowID(ctx, "wf-1")
		if err != nil {
			t.Fatalf("FindByWorkflowID returned error: %v", err)
		}
		if len(runs) != 2 || runs[0].RunID != "agent-run-1" || runs[1].RunID != "agent-run-2" {
			t.Fatalf("workflow runs = %+v, want run-1/run-2 in insertion order", runs)
		}
	})

	t.Run("returns not found for missing run", func(t *testing.T) {
		store := newStore(t)

		if _, err := store.Get(context.Background(), "missing"); !errors.Is(err, runkit.ErrRunNotFound) {
			t.Fatalf("Get missing error = %v, want ErrRunNotFound", err)
		}
		if err := store.AppendEvent(context.Background(), runkit.RunEvent{RunID: "missing", Type: "x"}); !errors.Is(err, runkit.ErrRunNotFound) {
			t.Fatalf("AppendEvent missing error = %v, want ErrRunNotFound", err)
		}
		if _, err := store.Events(context.Background(), "missing"); !errors.Is(err, runkit.ErrRunNotFound) {
			t.Fatalf("Events missing error = %v, want ErrRunNotFound", err)
		}
		if err := store.Complete(context.Background(), "missing", runkit.TerminalSummary{Status: runkit.StatusFailed}); !errors.Is(err, runkit.ErrRunNotFound) {
			t.Fatalf("Complete missing error = %v, want ErrRunNotFound", err)
		}
	})

	t.Run("honors context cancellation", func(t *testing.T) {
		store := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := store.Create(ctx, runkit.RunRecord{RunID: "cancelled"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Create cancelled error = %v, want context.Canceled", err)
		}
		if _, err := store.Get(ctx, "cancelled"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Get cancelled error = %v, want context.Canceled", err)
		}
	})
}
