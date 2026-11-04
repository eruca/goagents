package ports

import "context"

type MemoryMessage struct {
	Role    string
	Content string
}

// MemoryProvider stores a session's conversation history.
type MemoryProvider interface {
	// Load returns the session's stored messages, oldest first.
	Load(ctx context.Context, sessionID string) ([]MemoryMessage, error)
	// Append adds the messages one successful run produced, in order, after
	// those already stored for the session. It receives only the run's new
	// messages, never the history it loaded, so concurrent runs on one
	// session each add their own turn instead of replacing the other's.
	Append(ctx context.Context, sessionID string, messages []MemoryMessage) error
}
