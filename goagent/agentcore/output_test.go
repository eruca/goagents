package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/eruca/goagent/ports"
)

func TestOutputFormatAddsPromptAndStoresStructuredOutput(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: `{"status":"ok"}`}}}
	sink := &recordingEventSink{}
	agent, err := NewAgent(
		WithLLM(llm),
		WithEventSink(sink),
		WithOutputFormat(OutputFormat{
			Name:        "status_summary",
			Description: "A compact status summary.",
			JSONSchema: json.RawMessage(`{
				"type":"object",
				"required":["status"],
				"properties":{"status":{"type":"string"}},
				"additionalProperties":false
			}`),
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "summarize"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if string(result.StructuredOutput) != `{"status":"ok"}` {
		t.Fatalf("structured output = %s", result.StructuredOutput)
	}
	if result.OutputMetadata["output_format"] != "status_summary" {
		t.Fatalf("output metadata = %#v", result.OutputMetadata)
	}
	if len(llm.requests) != 1 || len(llm.requests[0].Messages) == 0 {
		t.Fatalf("llm requests = %#v", llm.requests)
	}
	system := llm.requests[0].Messages[0].Content
	if !strings.Contains(system, "Output format:") || !strings.Contains(system, "status_summary") || !strings.Contains(system, "JSON Schema") {
		t.Fatalf("system prompt = %q", system)
	}
	if !sink.hasEvent(EventOutputValidated, result.RunID) {
		t.Fatalf("missing output validated event: %#v", sink.events)
	}
}

func TestOutputValidationFailureAbortsBeforeMemorySave(t *testing.T) {
	memory := &mockMemoryProvider{}
	agent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{{Content: `{"status":3}`}}}),
		WithMemoryProvider(memory),
		WithOutputFormat(OutputFormat{
			Name: "status_summary",
			JSONSchema: json.RawMessage(`{
				"type":"object",
				"required":["status"],
				"properties":{"status":{"type":"string"}}
			}`),
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.RunDetailed(context.Background(), RunRequest{
		Input:     "summarize",
		SessionID: "session_1",
	})
	if err == nil {
		t.Fatal("RunDetailed returned nil error")
	}
	if !errors.Is(err, ErrOutputInvalid) {
		t.Fatalf("err = %v, want ErrOutputInvalid", err)
	}
	if result == nil || result.ExecutionSummary.AbortReason == "" {
		t.Fatalf("partial result = %#v", result)
	}
	if memory.saveCalls != 0 {
		t.Fatalf("memory save calls = %d", memory.saveCalls)
	}
}

func TestCustomOutputValidatorCanRejectFinalOutput(t *testing.T) {
	agent, err := NewAgent(
		WithLLM(&mockLLM{responses: []*ports.ChatResponse{{Content: "unsafe"}}}),
		WithOutputValidator(OutputValidatorFunc(func(ctx context.Context, req OutputValidationRequest) (*OutputValidationResult, error) {
			return nil, errors.New("blocked by host policy")
		})),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "answer"})
	if !errors.Is(err, ErrOutputInvalid) {
		t.Fatalf("err = %v, want ErrOutputInvalid", err)
	}
}
