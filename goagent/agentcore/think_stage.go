package agentcore

import (
	"context"
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
	resp, err := s.LLM.Chat(ctx, ports.ChatRequest{
		Messages:        chatMessages(state),
		Tools:           chatToolSpecs(s.ToolRegistry),
		MaxOutputTokens: s.MaxOutputTokens,
	})
	if err != nil {
		return StageAbort, err
	}
	state.recordLLMCall()
	state.LastResponse = resp
	state.Usage.InputTokens += resp.Usage.InputTokens
	state.Usage.OutputTokens += resp.Usage.OutputTokens
	state.PendingCalls = state.PendingCalls[:0]
	var rejected []Message
	for _, call := range resp.ToolCalls {
		if reason := s.rejectToolCall(call); reason != "" {
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
