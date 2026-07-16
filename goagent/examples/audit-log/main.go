package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

type mockLLM struct {
	responses []*ports.ChatResponse
}

func (m *mockLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

type approveAll struct{}

func (a approveAll) ApproveTool(ctx context.Context, req agentcore.ToolApprovalRequest) agentcore.ToolApprovalDecision {
	return agentcore.ToolApprovalDecision{Allowed: true, Reason: "operator approved"}
}

type updateDraftTool struct{}

func (t updateDraftTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "update_draft",
		Description: "Updates a deterministic draft note.",
		Permission:  policy.PermissionWrite,
		Schema: tools.Schema{
			JSONSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`),
		},
	}
}

func (t updateDraftTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	var req struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}
	return &tools.Result{
		ForLLM:  fmt.Sprintf("draft updated title=%q", req.Title),
		ForUser: req.Title,
		Ref:     "draft:demo",
	}, nil
}

type auditRecorder struct {
	encoder  *json.Encoder
	sequence int
}

type auditEventRecord struct {
	Record     string         `json:"record"`
	RunID      string         `json:"run_id"`
	Sequence   int            `json:"sequence"`
	EventType  string         `json:"event_type"`
	Stage      string         `json:"stage,omitempty"`
	Iteration  int            `json:"iteration,omitempty"`
	Message    string         `json:"message,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	RecordedAt string         `json:"recorded_at"`
}

type auditTerminalRecord struct {
	Record         string              `json:"record"`
	RunID          string              `json:"run_id,omitempty"`
	Status         string              `json:"status"`
	Error          string              `json:"error,omitempty"`
	ContentPreview string              `json:"content_preview,omitempty"`
	Summary        auditSummaryPayload `json:"summary"`
}

type auditSummaryPayload struct {
	LLMCalls    int      `json:"llm_calls"`
	ToolCalls   int      `json:"tool_calls"`
	UsedTools   []string `json:"used_tools,omitempty"`
	DurationMS  int64    `json:"duration_ms,omitempty"`
	AbortReason string   `json:"abort_reason,omitempty"`
}

func newAuditRecorder(w io.Writer) *auditRecorder {
	return &auditRecorder{encoder: json.NewEncoder(w)}
}

func (r *auditRecorder) RecordEvent(event agentcore.Event) error {
	r.sequence++
	return r.encoder.Encode(auditEventRecord{
		Record:     "run_event",
		RunID:      event.RunID.String(),
		Sequence:   r.sequence,
		EventType:  string(event.Type),
		Stage:      event.Stage,
		Iteration:  event.Iteration,
		Message:    event.Message,
		Metadata:   allowlistedMetadata(event.Metadata),
		RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (r *auditRecorder) RecordTerminal(result *agentcore.RunResult, err error) error {
	record := auditTerminalRecord{
		Record: "run_terminal",
		Status: "succeeded",
	}
	if err != nil {
		record.Status = "aborted"
		record.Error = err.Error()
	}
	if result != nil {
		record.RunID = result.RunID.String()
		record.ContentPreview = result.Content
		record.Summary = auditSummaryPayload{
			LLMCalls:    result.ExecutionSummary.LLMCalls,
			ToolCalls:   result.ExecutionSummary.ToolCalls,
			UsedTools:   result.ExecutionSummary.UsedTools,
			DurationMS:  result.ExecutionSummary.Duration.Milliseconds(),
			AbortReason: result.ExecutionSummary.AbortReason,
		}
	}
	return r.encoder.Encode(record)
}

func allowlistedMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	allowed := make(map[string]any)
	for _, key := range []string{"tool", "ref", "index", "reason", "count", "permission"} {
		if value, ok := metadata[key]; ok {
			allowed[key] = value
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	return allowed
}

func runAuditDemo(w io.Writer) error {
	agent, err := newDemoAgent()
	if err != nil {
		return err
	}
	recorder := newAuditRecorder(w)
	stream := agent.Stream(context.Background(), agentcore.RunRequest{
		Input:              "Update the draft title.",
		AllowedPermissions: []policy.Permission{policy.PermissionWrite},
	})
	for event := range stream.Events {
		if event.Done {
			if err := recorder.RecordTerminal(event.Result, event.Error); err != nil {
				return err
			}
			continue
		}
		if !shouldRecordEvent(event.Event) {
			continue
		}
		if err := recorder.RecordEvent(event.Event); err != nil {
			return err
		}
	}
	_, err = stream.Wait()
	return err
}

func shouldRecordEvent(event agentcore.Event) bool {
	switch event.Type {
	case agentcore.EventApprovalRequested,
		agentcore.EventApprovalCompleted,
		agentcore.EventApprovalDenied,
		agentcore.EventToolStarted,
		agentcore.EventToolCompleted,
		agentcore.EventToolFailed,
		agentcore.EventFinalized:
		return true
	default:
		return false
	}
}

func newDemoAgent() (*agentcore.Agent, error) {
	registry := tools.NewRegistry()
	registry.Register(updateDraftTool{})

	return agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{ID: "call_update", Name: "update_draft", Input: json.RawMessage(`{"title":"Approved draft"}`)}}},
			{Content: "Final answer: draft updated after approval."},
		}}),
		agentcore.WithToolRegistry(registry),
		agentcore.WithToolApprover(approveAll{}),
	)
}

func main() {
	once := flag.Bool("once", false, "write one deterministic JSONL audit run and exit")
	flag.Parse()

	if !*once {
		log.Fatal("set --once to run the deterministic audit-log example")
	}
	if err := runAuditDemo(os.Stdout); err != nil {
		log.Fatal(err)
	}
}
