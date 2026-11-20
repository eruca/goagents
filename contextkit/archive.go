package contextkit

import (
	"context"
	"errors"
)

// ErrArchiveNotFound reports a reference the archive does not hold in the
// current scope.
var ErrArchiveNotFound = errors.New("contextkit: archived message not found")

// Archive keeps what compression removes from a conversation, so the model
// can read it back by reference or search it. As an OffloadStore it receives
// tool results compression truncates or clears and steps it drops.
//
// An Archive is scoped: Offload, Read, and Search use the scope in the
// context (see WithArchiveScope), so one conversation never reads another's
// messages. Archived messages are the conversation's own content, including
// tool output, and must be shown to the model as data, not instructions.
type Archive interface {
	OffloadStore
	Read(ctx context.Context, ref string) (Message, error)
	Search(ctx context.Context, query string, limit int) ([]ArchiveHit, error)
}

// ArchiveHit is one search result, best first.
type ArchiveHit struct {
	Ref     string
	Message Message
	Score   float64
}

type archiveScopeKey struct{}

// WithArchiveScope returns a context whose archive operations use scope, for
// example a conversation or workflow ID.
func WithArchiveScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, archiveScopeKey{}, scope)
}

// ArchiveScope returns the scope WithArchiveScope set, or "".
func ArchiveScope(ctx context.Context) string {
	scope, _ := ctx.Value(archiveScopeKey{}).(string)
	return scope
}
