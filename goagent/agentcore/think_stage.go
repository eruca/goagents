package agentcore

import (
	"context"
	"errors"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

var ErrMaxOutputTokensUnsupported = errors.New("LLM does not support per-call max output tokens")

type ThinkStage struct {
	LLM          ports.LLMClient
	ToolRegistry ports.ToolRegistry
}

type maxOutputTokensLLM interface {
	ChatWithMaxOutputTokens(context.Context, ports.ChatRequest, int) (*ports.ChatResponse, error)
}

type thinkStageWithMaxOutputTokens struct {
	ThinkStage
	maxOutputTokens int
}

// NewThinkStageWithMaxOutputTokens 保持 ThinkStage 的 v0.1.0 形状，
// 同时为需要 Provider 侧上限的调用者提供增量入口。
func NewThinkStageWithMaxOutputTokens(llm ports.LLMClient, registry ports.ToolRegistry, maxOutputTokens int) Stage {
	return thinkStageWithMaxOutputTokens{
		ThinkStage:      ThinkStage{LLM: llm, ToolRegistry: registry},
		maxOutputTokens: maxOutputTokens,
	}
}

func (s ThinkStage) Name() string {
	return "think"
}

func (s ThinkStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	return s.run(ctx, state, 0)
}

func (s thinkStageWithMaxOutputTokens) Run(ctx context.Context, state *RunState) (StageResult, error) {
	return s.ThinkStage.run(ctx, state, s.maxOutputTokens)
}

func (s ThinkStage) run(ctx context.Context, state *RunState, maxOutputTokens int) (StageResult, error) {
	req := ports.ChatRequest{
		Messages: chatMessages(state),
		Tools:    chatToolSpecs(s.ToolRegistry),
	}
	var resp *ports.ChatResponse
	var err error
	if maxOutputTokens == 0 {
		resp, err = s.LLM.Chat(ctx, req)
	} else if client, ok := s.LLM.(maxOutputTokensLLM); ok {
		resp, err = client.ChatWithMaxOutputTokens(ctx, req, maxOutputTokens)
	} else {
		return StageAbort, ErrMaxOutputTokensUnsupported
	}
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
