package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

func TestAgentStreamEmitsEventsAndTerminalResult(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "streamed answer"}}}
	agent, err := NewAgent(WithLLM(llm))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "hello"})

	var events []RunStreamEvent
	for event := range stream.Events {
		events = append(events, event)
	}
	result, err := stream.Wait()
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if result == nil || result.Content != "streamed answer" {
		t.Fatalf("result = %#v", result)
	}
	if len(events) == 0 {
		t.Fatal("no stream events received")
	}
	terminal := events[len(events)-1]
	if !terminal.Done || terminal.Result == nil || terminal.Result.Content != "streamed answer" || terminal.Error != nil {
		t.Fatalf("terminal event = %#v", terminal)
	}
	foundThink := false
	for _, event := range events {
		if event.Event.Type == EventStageStarted && event.Event.Stage == "think" {
			foundThink = true
		}
	}
	if !foundThink {
		t.Fatalf("stream events did not include think stage: %#v", events)
	}
}

func TestAgentStreamReturnsPartialResultOnPolicyDeny(t *testing.T) {
	toolRan := false
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write_file", Permission: policy.PermissionWrite},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			toolRan = true
			return &tools.Result{ForLLM: "wrote file"}, nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		ToolCalls: []ports.ToolCall{{
			ID:    "call_write_file",
			Name:  "write_file",
			Input: json.RawMessage(`{"path":"notes.md"}`),
		}},
		Usage: ports.Usage{InputTokens: 8, OutputTokens: 3},
	}}}
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "write notes"})
	var terminal RunStreamEvent
	for event := range stream.Events {
		if event.Done {
			terminal = event
		}
	}
	result, err := stream.Wait()
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
	if toolRan {
		t.Fatal("write tool ran after policy denial")
	}
	if result == nil || terminal.Result == nil {
		t.Fatalf("result=%#v terminal=%#v", result, terminal)
	}
	if result.ExecutionSummary.LLMCalls != 1 || result.ExecutionSummary.ToolCalls != 0 {
		t.Fatalf("summary = %#v", result.ExecutionSummary)
	}
	if terminal.Error == nil || !errors.Is(terminal.Error, ErrPolicyDenied) {
		t.Fatalf("terminal error = %v", terminal.Error)
	}
}

func TestAgentStreamFansOutToConfiguredEventSink(t *testing.T) {
	recorder := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "done"}}}
	agent, err := NewAgent(WithLLM(llm), WithEventSink(recorder))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "hello"})
	for range stream.Events {
	}
	if _, err := stream.Wait(); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if len(recorder.events) == 0 {
		t.Fatal("configured sink received no events")
	}
}

func TestAgentStreamWaitDoesNotDependOnConsumingEvents(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{}, {}, {}, {}, {}, {}, {}, {},
	}}
	agent, err := NewAgent(WithLLM(llm), WithMaxIterations(8))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "loop"})

	done := make(chan error, 1)
	go func() {
		_, err := stream.Wait()
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrMaxIterations) {
			t.Fatalf("err = %v, want ErrMaxIterations", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait blocked while stream events were not consumed")
	}

	stream.DiscardEvents()
	select {
	case <-stream.relayDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("event relay did not stop after DiscardEvents")
	}
	if _, ok := <-stream.Events; ok {
		t.Fatal("Events remained open after DiscardEvents")
	}
}

func TestAgentStreamPreservesTailEventsWhenConsumedAfterWait(t *testing.T) {
	recorder := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{}, {}, {}, {}, {}, {}, {}, {Content: "done"},
	}}
	agent, err := NewAgent(WithLLM(llm), WithMaxIterations(8), WithEventSink(recorder))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "loop then finish"})
	result, err := stream.Wait()
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if result == nil || result.Content != "done" {
		t.Fatalf("result = %#v", result)
	}

	var foundFinalized bool
	var events []RunStreamEvent
	var runtimeEvents []Event
	for event := range stream.Events {
		events = append(events, event)
		if event.Event.Type == EventFinalized {
			foundFinalized = true
		}
		if !event.Done {
			runtimeEvents = append(runtimeEvents, event.Event)
		}
	}
	if !foundFinalized {
		t.Fatal("stream dropped finalized event after its buffer filled")
	}
	if len(events) == 0 {
		t.Fatal("stream returned no events")
	}
	terminal := events[len(events)-1]
	if !terminal.Done || terminal.Result == nil || terminal.Result.Content != "done" {
		t.Fatalf("terminal event = %#v", terminal)
	}
	if !reflect.DeepEqual(runtimeEvents, recorder.events) {
		t.Fatalf("stream events differ from sink events:\nstream=%#v\nsink=%#v", runtimeEvents, recorder.events)
	}
}

func TestAgentStreamDiscardEventsDoesNotCancelRunOrSink(t *testing.T) {
	recorder := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{}, {}, {}, {}, {}, {}, {}, {Content: "done"},
	}}
	agent, err := NewAgent(WithLLM(llm), WithMaxIterations(8), WithEventSink(recorder))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "discard events only"})
	stream.DiscardEvents()
	stream.DiscardEvents()
	result, err := stream.Wait()
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if result == nil || result.Content != "done" {
		t.Fatalf("result = %#v", result)
	}
	if !recorder.hasEvent(EventFinalized, result.RunID) {
		t.Fatal("configured sink did not receive finalized event after stream discard")
	}
	select {
	case <-stream.relayDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("event relay did not stop after discarded run completed")
	}
	if _, ok := <-stream.Events; ok {
		t.Fatal("Events remained open after discarded run completed")
	}
}

// streamingLLM streams each response's content in fixed-size chunks, then
// each tool call as a header piece followed by its arguments.
type streamingLLM struct {
	mockLLM
	chunk       int
	streamCalls int
}

func (s *streamingLLM) ChatStream(ctx context.Context, req ports.ChatRequest, onDelta func(ports.ChatDelta) error) (*ports.ChatResponse, error) {
	s.streamCalls++
	resp, err := s.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	for content := resp.Content; content != ""; {
		n := min(s.chunk, len(content))
		if err := onDelta(ports.ChatDelta{Content: content[:n]}); err != nil {
			return nil, err
		}
		content = content[n:]
	}
	for i, call := range resp.ToolCalls {
		for _, piece := range []ports.ToolCallDelta{{Index: i, ID: call.ID, Name: call.Name}, {Index: i, Arguments: string(call.Input)}} {
			if err := onDelta(ports.ChatDelta{ToolCall: &piece}); err != nil {
				return nil, err
			}
		}
	}
	return resp, nil
}

func TestAgentStreamDeliversModelDeltasBeforeResult(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &streamingLLM{chunk: 4, mockLLM: mockLLM{responses: []*ports.ChatResponse{
		{Content: "checking", ToolCalls: []ports.ToolCall{{ID: "call_1", Name: "lookup", Input: json.RawMessage(`{}`)}}},
		{Content: "final answer"},
	}}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "observation"}, nil
		},
	})
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry), WithEventSink(sink))
	if err != nil {
		t.Fatal(err)
	}

	stream := agent.Stream(context.Background(), RunRequest{Input: "hello"})
	var deltas []ModelDelta
	sawDone := false
	for event := range stream.Events {
		if event.Delta != nil {
			if sawDone {
				t.Fatal("delta arrived after the terminal event")
			}
			deltas = append(deltas, *event.Delta)
		}
		sawDone = sawDone || event.Done
	}
	result, err := stream.Wait()
	if err != nil || result.Content != "final answer" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	var first, second strings.Builder
	var toolPieces []ports.ToolCallDelta
	contentDeltas := 0
	for _, delta := range deltas {
		if delta.ToolCall != nil {
			if delta.Iteration != 0 {
				t.Fatalf("tool call delta in iteration %d, want 0", delta.Iteration)
			}
			toolPieces = append(toolPieces, *delta.ToolCall)
			continue
		}
		contentDeltas++
		switch delta.Iteration {
		case 0:
			first.WriteString(delta.Content)
		case 1:
			second.WriteString(delta.Content)
		default:
			t.Fatalf("delta iteration = %d", delta.Iteration)
		}
	}
	if first.String() != "checking" || second.String() != "final answer" || contentDeltas != 5 {
		t.Fatalf("deltas = %+v, want chunks of each model call grouped by iteration", deltas)
	}
	if len(toolPieces) != 2 || toolPieces[0].ID != "call_1" || toolPieces[0].Name != "lookup" || toolPieces[1].Arguments != "{}" {
		t.Fatalf("tool call pieces = %+v, want the lookup call header then its arguments", toolPieces)
	}
	for _, event := range sink.events {
		if strings.Contains(event.Message, "final") {
			t.Fatalf("event sink received model output: %+v", event)
		}
	}
}

func TestAgentRunDoesNotStreamModelOutput(t *testing.T) {
	llm := &streamingLLM{chunk: 4, mockLLM: mockLLM{responses: []*ports.ChatResponse{{Content: "answer"}}}}
	agent, err := NewAgent(WithLLM(llm))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), RunRequest{Input: "hello"}); err != nil {
		t.Fatal(err)
	}
	if llm.streamCalls != 0 {
		t.Fatalf("ChatStream calls = %d, want Chat for a non-streaming run", llm.streamCalls)
	}
}

func TestAgentStreamWithNonStreamingLLMHasNoDeltas(t *testing.T) {
	agent, err := NewAgent(WithLLM(&mockLLM{responses: []*ports.ChatResponse{{Content: "answer"}}}))
	if err != nil {
		t.Fatal(err)
	}
	stream := agent.Stream(context.Background(), RunRequest{Input: "hello"})
	for event := range stream.Events {
		if event.Delta != nil {
			t.Fatalf("unexpected delta %+v", event.Delta)
		}
	}
	if result, err := stream.Wait(); err != nil || result.Content != "answer" {
		t.Fatalf("result = %+v, %v", result, err)
	}
}
