package agentcore

import (
	"context"

	"github.com/eruca/goagents/goagent/ports"
)

type MemoryLoadStage struct {
	Provider ports.MemoryProvider
}

func (s MemoryLoadStage) Name() string {
	return "memory_load"
}

func (s MemoryLoadStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Provider == nil || state.Input.SessionID == "" || state.stageDone(memoryLoadedKey) {
		return StageContinue, nil
	}
	loaded, err := s.Provider.Load(ctx, state.Input.SessionID)
	if err != nil {
		return StageAbort, err
	}
	history := memoryStateMessages(loaded)
	state.Messages = append(history, state.Messages...)
	state.memoryMessages = len(history)
	state.markStageDone(memoryLoadedKey)
	state.Emit(ctx, Event{
		Type: EventMemoryLoaded,
		Metadata: map[string]any{
			"session_id":    state.Input.SessionID,
			"message_count": len(loaded),
		},
	})
	return StageContinue, nil
}

const memoryLoadedKey = "agentcore.memory.loaded"

func memoryStateMessages(messages []ports.MemoryMessage) []Message {
	converted := make([]Message, 0, len(messages))
	for _, message := range messages {
		// MemoryMessage has no tool call ID, so a stored tool message would reach the provider orphaned.
		if message.Role == "tool" {
			continue
		}
		converted = append(converted, Message{Role: message.Role, Content: message.Content})
	}
	return converted
}

func memoryMessages(messages []Message) []ports.MemoryMessage {
	converted := make([]ports.MemoryMessage, 0, len(messages))
	for _, message := range messages {
		if len(message.ToolCalls) > 0 || message.Role == "tool" {
			continue
		}
		converted = append(converted, ports.MemoryMessage{Role: message.Role, Content: message.Content})
	}
	return converted
}
