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
}

func (s ThinkStage) Name() string {
	return "think"
}

func (s ThinkStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	req := ports.ChatRequest{
		Messages:        chatMessages(state),
		Tools:           chatToolSpecs(s.ToolRegistry),
		MaxOutputTokens: s.MaxOutputTokens,
	}
	var resp *ports.ChatResponse
	var err error
	if streaming, ok := s.LLM.(ports.StreamingLLMClient); ok && state.onDelta != nil {
		iteration := state.Iteration
		resp, err = streaming.ChatStream(ctx, req, func(delta ports.ChatDelta) error {
			if delta.Content != "" || delta.ToolCall != nil {
				state.onDelta(ModelDelta{Iteration: iteration, Content: delta.Content, ToolCall: delta.ToolCall})
			}
			return nil
		})
	} else {
		resp, err = s.LLM.Chat(ctx, req)
	}
	if err != nil {
		return StageAbort, err
	}
	state.recordLLMCall()
	state.LastResponse = resp
	state.Usage.InputTokens += resp.Usage.InputTokens
	state.Usage.OutputTokens += resp.Usage.OutputTokens
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
		})
		// Rejected calls never reach policy, approval, or execution; they are
		// answered now so the model can correct them on its next turn.
		state.Messages = append(state.Messages, rejected...)
	} else if resp.Content != "" {
		state.Messages = append(state.Messages, Message{Role: "assistant", Content: resp.Content})
	}
	return StageContinue, nil
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

func chatMessages(state *RunState) []ports.ChatMessage {
	source := state.Messages
	if state.ContextProjection != nil {
		source = state.ContextProjection.Messages
	}
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
