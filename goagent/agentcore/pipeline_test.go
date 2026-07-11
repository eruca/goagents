package agentcore

import (
	"context"
	"fmt"
	"testing"
)

type recordingStage struct {
	name   string
	result StageResult
	seen   *[]string
}

func (s recordingStage) Name() string {
	return s.name
}

func (s recordingStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	*s.seen = append(*s.seen, s.name)
	return s.result, nil
}

func TestPipelineRunsStagesInOrderAndStopsOnBreak(t *testing.T) {
	seen := make([]string, 0, 3)
	pipeline := NewPipeline(
		recordingStage{name: "first", result: StageContinue, seen: &seen},
		recordingStage{name: "second", result: StageBreak, seen: &seen},
		recordingStage{name: "third", result: StageContinue, seen: &seen},
	)

	result, err := pipeline.Run(context.Background(), NewRunState(NewRunID(), RunRequest{}))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result != StageBreak {
		t.Fatalf("result = %v", result)
	}

	want := []string{"first", "second"}
	if len(seen) != len(want) {
		t.Fatalf("seen = %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen = %v", seen)
		}
	}
}

func TestPipelineEmitsStageEvents(t *testing.T) {
	sink := &recordingEventSink{}
	state := NewRunState(NewRunID(), RunRequest{})
	state.EventSink = sink
	pipeline := NewPipeline(
		recordingStage{name: "first", result: StageContinue, seen: &[]string{}},
		recordingStage{name: "second", result: StageBreak, seen: &[]string{}},
	)

	result, err := pipeline.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result != StageBreak {
		t.Fatalf("result = %v", result)
	}

	want := []EventType{
		EventStageStarted,
		EventStageCompleted,
		EventStageStarted,
		EventStageCompleted,
	}
	if len(sink.events) != len(want) {
		t.Fatalf("events = %#v", sink.events)
	}
	for i := range want {
		if sink.events[i].Type != want[i] {
			t.Fatalf("events = %#v", sink.events)
		}
	}
	if sink.events[0].Stage != "first" || sink.events[2].Stage != "second" {
		t.Fatalf("events = %#v", sink.events)
	}
}

func TestPipelineEmitsStageFailedEvent(t *testing.T) {
	sink := &recordingEventSink{}
	state := NewRunState(NewRunID(), RunRequest{})
	state.EventSink = sink
	pipeline := NewPipeline(errorStage{name: "broken"})

	result, err := pipeline.Run(context.Background(), state)
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if result != StageAbort {
		t.Fatalf("result = %v", result)
	}
	if len(sink.events) != 2 {
		t.Fatalf("events = %#v", sink.events)
	}
	if sink.events[0].Type != EventStageStarted || sink.events[1].Type != EventStageFailed {
		t.Fatalf("events = %#v", sink.events)
	}
	if sink.events[1].Stage != "broken" || sink.events[1].Message != "" {
		t.Fatalf("failed event = %#v", sink.events[1])
	}
}

type errorStage struct {
	name string
}

func (s errorStage) Name() string {
	return s.name
}

func (s errorStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	return StageAbort, fmt.Errorf("stage failed")
}
