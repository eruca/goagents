package agentcore

import "context"

type EventType string

const (
	EventStageStarted   EventType = "stage.started"
	EventStageCompleted EventType = "stage.completed"
	EventStageFailed    EventType = "stage.failed"
	EventToolStarted    EventType = "tool.started"
	EventToolCompleted  EventType = "tool.completed"
	EventToolFailed     EventType = "tool.failed"
	EventMemoryLoaded   EventType = "memory.loaded"
	EventMemorySaved    EventType = "memory.saved"
	EventFinalized      EventType = "finalized"

	EventApprovalRequested EventType = "approval.requested"
	EventApprovalCompleted EventType = "approval.completed"
	EventApprovalDenied    EventType = "approval.denied"
)

type Event struct {
	RunID     RunID
	Type      EventType
	Stage     string
	Iteration int
	Message   string
	Metadata  map[string]any
}

type EventSink interface {
	Emit(ctx context.Context, event Event) error
}
