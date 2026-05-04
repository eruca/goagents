package memory

import (
	"context"
	"sync"

	"github.com/eruca/goagent/ports"
)

type WindowMemory struct {
	limit    int
	mu       sync.Mutex
	messages map[string][]ports.MemoryMessage
}

func NewWindowMemory(limit int) *WindowMemory {
	return &WindowMemory{
		limit:    limit,
		messages: make(map[string][]ports.MemoryMessage),
	}
}

func (m *WindowMemory) Load(ctx context.Context, sessionID string) ([]ports.MemoryMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	messages := m.messages[sessionID]
	return append([]ports.MemoryMessage(nil), messages...), nil
}

func (m *WindowMemory) Save(ctx context.Context, sessionID string, messages []ports.MemoryMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := append([]ports.MemoryMessage(nil), messages...)
	if m.limit > 0 && len(copied) > m.limit {
		copied = copied[len(copied)-m.limit:]
	}
	m.messages[sessionID] = copied
	return nil
}
