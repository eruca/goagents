package agentcore

import (
	"context"

	"github.com/eruca/goagent/ports"
	"github.com/eruca/goagent/tools"
)

type ThinkStage struct {
	LLM          ports.LLMClient
	ToolRegistry ports.ToolRegistry
}

func (s ThinkStage) Name() string {
	return "think"
}

func (s ThinkStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	req := ports.ChatRequest{
		Messages: chatMessages(state),
		Tools:    chatToolSpecs(s.ToolRegistry),
	}
	resp, err := s.LLM.Chat(ctx, req)
	if err != nil {
		return StageAbort, err
	}
	state.recordLLMCall()
	state.LastResponse = resp
	state.Usage.InputTokens += resp.Usage.InputTokens
	state.Usage.OutputTokens += resp.Usage.OutputTokens
	state.PendingCalls = state.PendingCalls[:0]
	for _, call := range resp.ToolCalls {
		state.PendingCalls = append(state.PendingCalls, tools.Call{ID: call.ID, Name: call.Name, Input: call.Input})
	}
	if len(resp.ToolCalls) > 0 {
		state.Messages = append(state.Messages, Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: append([]ports.ToolCall(nil), resp.ToolCalls...),
		})
	} else if resp.Content != "" {
		state.Messages = append(state.Messages, Message{Role: "assistant", Content: resp.Content})
	}
	return StageContinue, nil
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
