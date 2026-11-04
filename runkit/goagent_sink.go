package runkit

import (
	"context"
	"errors"

	"github.com/eruca/goagents/goagent/agentcore"
)

type GoagentRunRecordBuilder func(agentcore.Event) RunRecord

type GoagentEventSink struct {
	store   Store
	builder GoagentRunRecordBuilder
}

func NewGoagentEventSink(store Store, builder GoagentRunRecordBuilder) GoagentEventSink {
	return GoagentEventSink{store: store, builder: builder}
}

func (s GoagentEventSink) Emit(ctx context.Context, event agentcore.Event) error {
	if s.store == nil {
		return nil
	}
	runID := event.RunID.String()
	// Only a missing run is created. Any other read error is returned: Create
	// would otherwise reset a finished run to running and drop its summary.
	if _, err := s.store.Get(ctx, runID); errors.Is(err, ErrRunNotFound) {
		record := RunRecord{RunID: runID, Status: StatusRunning}
		if s.builder != nil {
			record = s.builder(event)
			if record.RunID == "" {
				record.RunID = runID
			}
			if record.Status == "" {
				record.Status = StatusRunning
			}
		}
		if err := s.store.Create(ctx, record); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return s.store.AppendEvent(ctx, RunEvent{
		RunID:     runID,
		Type:      string(event.Type),
		Stage:     event.Stage,
		Iteration: event.Iteration,
		Message:   event.Message,
		Metadata:  event.Metadata,
	})
}
