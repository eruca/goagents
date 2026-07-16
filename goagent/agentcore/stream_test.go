package agentcore

import (
	"context"
	"encoding/json"
	"errors"
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
}
