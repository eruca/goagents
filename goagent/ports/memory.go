package ports

import "context"

type MemoryMessage struct {
	Role    string
	Content string
}

type MemoryProvider interface {
	Load(ctx context.Context, sessionID string) ([]MemoryMessage, error)
	Save(ctx context.Context, sessionID string, messages []MemoryMessage) error
}
