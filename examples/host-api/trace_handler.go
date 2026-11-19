package main

import (
	"net/http"
	"time"

	"github.com/eruca/goagents/evalkit"
	"github.com/eruca/goagents/evalkit/runtrace"
)

// workflowTraceResponse is the content-free trajectory of one workflow, built
// by evalkit/runtrace. It carries identifiers, statuses, timestamps, and
// counters only; no input, output, prompt, tool argument, or message.
type workflowTraceResponse struct {
	WorkflowID string              `json:"workflow_id"`
	Outcome    traceOutcome        `json:"outcome"`
	Usage      traceUsage          `json:"usage"`
	Labels     map[string]string   `json:"labels,omitempty"`
	Steps      []traceStepResponse `json:"steps"`
}

type traceOutcome struct {
	Status    string         `json:"status"`
	OutputRef string         `json:"output_ref,omitempty"`
	ErrorCode string         `json:"error_code,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type traceUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	LLMCalls     int `json:"llm_calls"`
	ToolCalls    int `json:"tool_calls"`
}

type traceStepResponse struct {
	Type      string         `json:"type"`
	Name      string         `json:"name,omitempty"`
	Status    string         `json:"status,omitempty"`
	StartedAt *time.Time     `json:"started_at,omitempty"`
	EndedAt   *time.Time     `json:"ended_at,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

func (s *Server) handleGetWorkflowTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.workflows.Get(r.Context(), id)
	if err != nil {
		writeWorkflowStoreError(w, err)
		return
	}
	if _, ok := s.authorizeStoredWorkflow(w, r, run, memoryRead); !ok {
		return
	}
	result, err := buildHostEvalResult(r.Context(), id, s.workflows, s.runs, s.auditStore(), runtrace.Fingerprint{})
	if err != nil {
		writeInternalError(w, "trace_error", err)
		return
	}
	writeJSON(w, http.StatusOK, traceToResponse(id, result))
}

func traceToResponse(workflowID string, result *evalkit.RunResult) workflowTraceResponse {
	usage := result.Trace.Usage
	response := workflowTraceResponse{
		WorkflowID: workflowID,
		Outcome: traceOutcome{
			Status:    result.Outcome.Status,
			OutputRef: result.Outcome.OutputRef,
			ErrorCode: result.Outcome.ErrorCode,
			Metadata:  result.Outcome.Metadata,
		},
		Usage: traceUsage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			LLMCalls:     usage.LLMCalls,
			ToolCalls:    usage.ToolCalls,
		},
		Labels: result.Trace.Labels,
		Steps:  make([]traceStepResponse, 0, len(result.Trace.Steps)),
	}
	for _, step := range result.Trace.Steps {
		response.Steps = append(response.Steps, traceStepResponse{
			Type:      step.Type,
			Name:      step.Name,
			Status:    step.Status,
			StartedAt: optionalTime(step.StartedAt),
			EndedAt:   optionalTime(step.EndedAt),
			Metadata:  step.Metadata,
		})
	}
	return response
}

func optionalTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}
