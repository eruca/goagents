package agentcore

import (
	"context"
	"time"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
	"github.com/eruca/goagents/goagent/tools"
)

func NewRunState(runID RunID, req RunRequest) *RunState {
	metadata := make(map[string]any, len(req.Metadata))
	for k, v := range req.Metadata {
		metadata[k] = v
	}

	return &RunState{
		RunID:              runID,
		Input:              req,
		Messages:           make([]Message, 0),
		Metadata:           metadata,
		AllowedPermissions: append([]policy.Permission(nil), req.AllowedPermissions...),
		startedAt:          time.Now(),
	}
}

type RunState struct {
	RunID     RunID
	Input     RunRequest
	Iteration int
	Messages  []Message
	Final     *RunResult
	Usage     Usage
	Metadata  map[string]any
	EventSink EventSink

	PromptBlocks       []prompt.Block
	CompiledPrompt     *prompt.Compiled
	ContextProjection  *ContextProjectionResult
	LastResponse       *ports.ChatResponse
	PendingCalls       []tools.Call
	ToolResults        []tools.Execution
	AllowedPermissions []policy.Permission

	summary        ExecutionSummary
	startedAt      time.Time
	usedToolLookup map[string]struct{}
}

func (s *RunState) Emit(ctx context.Context, event Event) {
	if s.EventSink == nil {
		return
	}
	event.RunID = s.RunID
	event.Iteration = s.Iteration
	_ = s.EventSink.Emit(ctx, event)
}

func (s *RunState) recordLLMCall() {
	s.summary.LLMCalls++
}

func (s *RunState) recordToolResults(results []tools.Execution) {
	for _, result := range results {
		s.summary.ToolCalls++
		if s.usedToolLookup == nil {
			s.usedToolLookup = make(map[string]struct{})
		}
		name := result.Call.Name
		if _, ok := s.usedToolLookup[name]; ok {
			continue
		}
		s.usedToolLookup[name] = struct{}{}
		s.summary.UsedTools = append(s.summary.UsedTools, name)
	}
}

func (s *RunState) executionSummary(abortReason string) ExecutionSummary {
	summary := s.summary
	summary.UsedTools = append([]string(nil), summary.UsedTools...)
	summary.AbortReason = abortReason
	summary.Duration = time.Since(s.startedAt)
	if summary.Duration <= 0 {
		summary.Duration = time.Nanosecond
	}
	return summary
}
