package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/eruca/goagent/agentcore"
	"github.com/eruca/goagent/ports"
)

type mockLLM struct {
	response ports.ChatResponse
}

func (m mockLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	return &m.response, nil
}

type demoRecord struct {
	Case         string          `json:"case"`
	Status       string          `json:"status"`
	Structured   json.RawMessage `json:"structured,omitempty"`
	OutputFormat string          `json:"output_format,omitempty"`
	Schema       string          `json:"schema,omitempty"`
	ErrorMatches bool            `json:"error_matches,omitempty"`
	HasPartial   bool            `json:"has_partial,omitempty"`
}

func runStructuredOutputDemo(w io.Writer) error {
	encoder := json.NewEncoder(w)

	success, err := runWithModelOutput(`{"status":"ok","risk":"low"}`)
	if err != nil {
		return err
	}
	if err := encoder.Encode(demoRecord{
		Case:         "schema-success",
		Status:       "validated",
		Structured:   success.StructuredOutput,
		OutputFormat: valueAsString(success.OutputMetadata["output_format"]),
		Schema:       valueAsString(success.OutputMetadata["schema"]),
	}); err != nil {
		return err
	}

	partial, err := runWithModelOutput(`{"status":3,"risk":"low"}`)
	if err == nil {
		return errors.New("expected schema validation failure")
	}
	return encoder.Encode(demoRecord{
		Case:         "schema-failure",
		Status:       "blocked",
		ErrorMatches: errors.Is(err, agentcore.ErrOutputInvalid),
		HasPartial:   partial != nil,
	})
}

func runWithModelOutput(output string) (*agentcore.RunResult, error) {
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(mockLLM{response: ports.ChatResponse{
			Content: output,
			Usage:   ports.Usage{InputTokens: 24, OutputTokens: 8},
		}}),
		agentcore.WithOutputFormat(agentcore.OutputFormat{
			Name:        "status_risk",
			Description: "A machine-readable status and risk summary.",
			JSONSchema: json.RawMessage(`{
				"type":"object",
				"required":["status","risk"],
				"properties":{
					"status":{"type":"string"},
					"risk":{"type":"string","enum":["low","medium","high"]}
				},
				"additionalProperties":false
			}`),
		}),
	)
	if err != nil {
		return nil, err
	}
	return agent.RunDetailed(context.Background(), agentcore.RunRequest{
		Input: "Return a compact status and risk summary.",
	})
}

func valueAsString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func main() {
	if err := runStructuredOutputDemo(os.Stdout); err != nil {
		panic(err)
	}
}
