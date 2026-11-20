package agentcore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

type ThinkStage struct {
	LLM             ports.LLMClient
	ToolRegistry    ports.ToolRegistry
	MaxOutputTokens int
	ReasoningReplay ReasoningReplay
	// Projection, when it has a Projector, projects the context again with
	// ContextProjectionRequest.Overflow set after the provider rejects a call
	// as too long (ports.ErrContextTooLong), and the call is retried, up to
	// MaxContextOverflowRetries times. Without one the error ends the run.
	Projection ContextProjectionStage
}

// MaxContextOverflowRetries bounds how often one model call is projected
// again and retried after the provider reports a context window overflow.
const MaxContextOverflowRetries = 2

func (s ThinkStage) Name() string {
	return "think"
}

func (s ThinkStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	resp, delivered, err := s.call(ctx, state)
	for attempt := 1; err != nil && errors.Is(err, ports.ErrContextTooLong) && !delivered &&
		s.Projection.Projector != nil && attempt <= MaxContextOverflowRetries; attempt++ {
		rejected := requestChars(chatMessages(state, s.ReasoningReplay))
		state.Emit(ctx, Event{Type: EventContextOverflow, Metadata: map[string]any{"attempt": attempt, "rejected_chars": rejected}})
		if projectErr := s.Projection.project(ctx, state, &ContextOverflow{Attempt: attempt, RejectedChars: rejected}); projectErr != nil {
			return StageAbort, projectErr
		}
		if requestChars(chatMessages(state, s.ReasoningReplay)) >= rejected {
			// The projector could not make the request smaller; sending it
			// again would fail the same way.
			break
		}
		resp, delivered, err = s.call(ctx, state)
	}
	if err != nil {
		return StageAbort, err
	}
	state.recordLLMCall()
	state.LastResponse = resp
	state.Usage.InputTokens += resp.Usage.InputTokens
	state.Usage.OutputTokens += resp.Usage.OutputTokens
	state.Usage.ReasoningTokens += resp.Usage.ReasoningTokens
	state.PendingCalls = state.PendingCalls[:0]
	var rejected []Message
	calls, invalidJSON := normalizeToolCallInputs(resp.ToolCalls)
	resp.ToolCalls = calls
	for i, call := range resp.ToolCalls {
		reason := s.rejectToolCall(call)
		if invalidJSON[i] {
			reason = fmt.Sprintf("Tool %s returned a recoverable error: the arguments are not valid JSON. Send the arguments as one complete JSON object and try again.", call.Name)
		}
		if reason != "" {
			state.Emit(ctx, Event{Type: EventToolRejected, Metadata: map[string]any{"tool": call.Name}})
			rejected = append(rejected, Message{Role: "tool", Content: reason, ToolCallID: call.ID})
			continue
		}
		state.PendingCalls = append(state.PendingCalls, tools.Call{ID: call.ID, Name: call.Name, Input: call.Input})
	}
	if len(resp.ToolCalls) > 0 {
		state.Messages = append(state.Messages, Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: append([]ports.ToolCall(nil), resp.ToolCalls...),
			Reasoning: resp.Reasoning,
		})
		// Rejected calls never reach policy, approval, or execution; they are
		// answered now so the model can correct them on its next turn.
		state.Messages = append(state.Messages, rejected...)
	} else if resp.Content != "" {
		state.Messages = append(state.Messages, Message{Role: "assistant", Content: resp.Content, Reasoning: resp.Reasoning})
	}
	return StageContinue, nil
}

// call sends the current request to the model. delivered reports whether any
// output reached the stream consumer, after which the call is not retried.
func (s ThinkStage) call(ctx context.Context, state *RunState) (*ports.ChatResponse, bool, error) {
	req := ports.ChatRequest{
		Messages:        chatMessages(state, s.ReasoningReplay),
		Tools:           chatToolSpecs(s.ToolRegistry),
		MaxOutputTokens: s.MaxOutputTokens,
	}
	if streaming, ok := s.LLM.(ports.StreamingLLMClient); ok && state.onDelta != nil {
		iteration := state.Iteration
		delivered := false
		resp, err := streaming.ChatStream(ctx, req, func(delta ports.ChatDelta) error {
			if delta.Content != "" || delta.Reasoning != "" || delta.ToolCall != nil {
				delivered = true
				state.onDelta(ModelDelta{Iteration: iteration, Content: delta.Content, Reasoning: delta.Reasoning, ToolCall: delta.ToolCall})
			}
			return nil
		})
		return resp, delivered, err
	}
	resp, err := s.LLM.Chat(ctx, req)
	return resp, false, err
}

// requestChars measures a request's messages the way ContextOverflow reports
// them.
func requestChars(messages []ports.ChatMessage) int {
	total := 0
	for _, msg := range messages {
		total += len(msg.Content) + len(msg.Reasoning)
		for _, call := range msg.ToolCalls {
			total += len(call.Name) + len(call.Input)
		}
	}
	return total
}

// normalizeToolCallInputs makes every call's arguments valid JSON before they
// enter run state. Missing arguments, which some providers send as "" for a
// tool without parameters, become {}. Arguments that are not JSON, such as a
// truncated object, reject the call and are replaced by {}: run messages and
// checkpoints are JSON, and a raw invalid argument would make the run
// impossible to checkpoint for approval.
// The returned set marks the calls whose arguments were not JSON.
func normalizeToolCallInputs(calls []ports.ToolCall) ([]ports.ToolCall, map[int]bool) {
	normalized := make([]ports.ToolCall, len(calls))
	invalid := map[int]bool{}
	for i, call := range calls {
		if len(bytes.TrimSpace(call.Input)) == 0 {
			call.Input = json.RawMessage(`{}`)
		} else if !json.Valid(call.Input) {
			invalid[i] = true
			call.Input = json.RawMessage(`{}`)
		}
		normalized[i] = call
	}
	return normalized, invalid
}

// rejectToolCall returns an observation for a call the model can fix itself:
// an unknown tool name or arguments that fail the tool's input schema. A broken
// schema is a host error, so that call is left for the executor to abort.
func (s ThinkStage) rejectToolCall(call ports.ToolCall) string {
	if s.ToolRegistry == nil {
		return ""
	}
	tool, ok := s.ToolRegistry.Get(call.Name)
	if !ok {
		names := make([]string, 0)
		for _, spec := range s.ToolRegistry.Specs() {
			names = append(names, spec.Name)
		}
		return fmt.Sprintf("Tool %q is not available. Available tools: %s. Use one of them or answer directly.", call.Name, strings.Join(names, ", "))
	}
	if err := tools.ValidateInput(tool.Spec().Schema, call.Input); err != nil && !errors.Is(err, tools.ErrToolSchemaInvalid) {
		return fmt.Sprintf("Tool %s returned a recoverable error: %v. Correct the arguments and try again.", call.Name, err)
	}
	return ""
}

func chatMessages(state *RunState, replay ReasoningReplay) []ports.ChatMessage {
	source := state.Messages
	if state.ContextProjection != nil {
		source = state.ContextProjection.Messages
	}
	source = replayView(source, replay)
	messages := make([]ports.ChatMessage, 0, len(source)+1)
	if state.CompiledPrompt != nil && state.CompiledPrompt.Content != "" {
		messages = append(messages, ports.ChatMessage{Role: "system", Content: state.CompiledPrompt.Content})
	}
	for _, msg := range source {
		messages = append(messages, ports.ChatMessage{
			Role:       msg.Role,
			Content:    msg.Content,
			ToolCallID: msg.ToolCallID,
			ToolCalls:  append([]ports.ToolCall(nil), msg.ToolCalls...),
			Reasoning:  msg.Reasoning,
		})
	}
	return messages
}

func chatToolSpecs(registry ports.ToolRegistry) []ports.ToolSpec {
	if registry == nil {
		return nil
	}
	specs := registry.Specs()
	chatSpecs := make([]ports.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		chatSpecs = append(chatSpecs, ports.ToolSpec{
			Name:        spec.Name,
			Description: spec.Description,
			Permission:  spec.Permission,
			Schema:      spec.Schema,
		})
	}
	return chatSpecs
}
