package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"

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

type runtimePayload struct {
	Type   agentcore.EventType `json:"type"`
	Stage  string              `json:"stage,omitempty"`
	Tool   string              `json:"tool,omitempty"`
	Ref    string              `json:"ref,omitempty"`
	Reason string              `json:"reason,omitempty"`
}

type donePayload struct {
	Content string         `json:"content,omitempty"`
	Error   string         `json:"error,omitempty"`
	Summary summaryPayload `json:"summary"`
}

type summaryPayload struct {
	LLMCalls    int      `json:"llm_calls"`
	ToolCalls   int      `json:"tool_calls"`
	UsedTools   []string `json:"used_tools,omitempty"`
	AbortReason string   `json:"abort_reason,omitempty"`
}

func newSSEHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		agent, err := newDemoAgent()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		stream := agent.Stream(r.Context(), agentcore.RunRequest{
			Input:              "Update the draft title.",
			AllowedPermissions: []policy.Permission{policy.PermissionWrite},
		})
		for event := range stream.Events {
			if event.Done {
				writeSSE(w, "done", newDonePayload(event.Result, event.Error))
				flush(w)
				continue
			}
			if !shouldStreamRuntimeEvent(event.Event) {
				continue
			}
			writeSSE(w, "runtime", newRuntimePayload(event.Event))
			flush(w)
		}
		_, _ = stream.Wait()
	})
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

func shouldStreamRuntimeEvent(event agentcore.Event) bool {
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

func newRuntimePayload(event agentcore.Event) runtimePayload {
	payload := runtimePayload{
		Type:  event.Type,
		Stage: event.Stage,
	}
	if event.Metadata != nil {
		payload.Tool = metadataString(event.Metadata, "tool")
		payload.Ref = metadataString(event.Metadata, "ref")
		payload.Reason = metadataString(event.Metadata, "reason")
	}
	return payload
}

func newDonePayload(result *agentcore.RunResult, err error) donePayload {
	payload := donePayload{}
	if err != nil {
		payload.Error = err.Error()
	}
	if result == nil {
		return payload
	}
	payload.Content = result.Content
	payload.Summary = summaryPayload{
		LLMCalls:    result.ExecutionSummary.LLMCalls,
		ToolCalls:   result.ExecutionSummary.ToolCalls,
		UsedTools:   result.ExecutionSummary.UsedTools,
		AbortReason: result.ExecutionSummary.AbortReason,
	}
	return payload
}

func metadataString(metadata map[string]any, key string) string {
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

func writeSSE(w io.Writer, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte(`{"error":"failed to encode event"}`)
	}
	fmt.Fprintf(w, "event: %s\n", event)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func flush(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func runOnce(w io.Writer) error {
	req := httptest.NewRequest(http.MethodGet, "/runs/stream", nil)
	rec := httptest.NewRecorder()
	newSSEHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	_, err := io.Copy(w, rec.Body)
	return err
}

func main() {
	once := flag.Bool("once", false, "print one deterministic SSE run and exit")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Parse()

	if *once {
		if err := runOnce(os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/runs/stream", newSSEHandler())
	log.Printf("listening on http://localhost%s/runs/stream", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
