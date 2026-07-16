package runkit

import (
	"context"
	"testing"

	"github.com/eruca/goagents/goagent/agentcore"
)

func TestGoagentEventSinkCreatesRunAndRecordsEvents(t *testing.T) {
	store := NewMemoryStore()
	sink := NewGoagentEventSink(store, func(event agentcore.Event) RunRecord {
		return RunRecord{
			RunID:      event.RunID.String(),
			WorkflowID: "wf-1",
			TaskID:     "task-1",
			Status:     StatusRunning,
		}
	})
	runID := agentcore.NewRunID()

	if err := sink.Emit(context.Background(), agentcore.Event{
		RunID: runID,
		Type:  agentcore.EventStageStarted,
		Stage: "think",
		Metadata: map[string]any{
			"source": "test",
		},
	}); err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}

	record, err := store.Get(context.Background(), runID.String())
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if record.WorkflowID != "wf-1" || record.Status != StatusRunning {
		t.Fatalf("record = %+v, want workflow metadata", record)
	}
	events, err := store.Events(context.Background(), runID.String())
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(events) != 1 || events[0].Type != string(agentcore.EventStageStarted) || events[0].Stage != "think" {
		t.Fatalf("events = %+v, want mapped stage event", events)
	}
	if events[0].Metadata["source"] != "test" {
		t.Fatalf("event metadata = %+v, want copied metadata", events[0].Metadata)
	}
}
