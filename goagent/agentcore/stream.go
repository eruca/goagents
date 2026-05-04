package agentcore

import "context"

const runStreamBuffer = 64

type RunStream struct {
	Events <-chan RunStreamEvent
	done   <-chan runStreamDone
}

type RunStreamEvent struct {
	Event  Event
	Result *RunResult
	Error  error
	Done   bool
}

type runStreamDone struct {
	result *RunResult
	err    error
}

func (a *Agent) Stream(ctx context.Context, req RunRequest) *RunStream {
	events := make(chan RunStreamEvent, runStreamBuffer)
	done := make(chan runStreamDone, 1)
	stream := &RunStream{Events: events, done: done}
	sink := streamEventSink{
		events: events,
		next:   a.eventSink,
	}

	go func() {
		defer close(events)
		result, err := a.runWithEventSink(ctx, req, true, sink)
		sendRunStreamEvent(events, RunStreamEvent{Result: result, Error: err, Done: true})
		done <- runStreamDone{result: result, err: err}
	}()

	return stream
}

func (s *RunStream) Wait() (*RunResult, error) {
	done := <-s.done
	return done.result, done.err
}

type streamEventSink struct {
	events chan<- RunStreamEvent
	next   EventSink
}

func (s streamEventSink) Emit(ctx context.Context, event Event) error {
	sendRunStreamEvent(s.events, RunStreamEvent{Event: event})
	if s.next != nil {
		_ = s.next.Emit(ctx, event)
	}
	return nil
}

func sendRunStreamEvent(events chan<- RunStreamEvent, event RunStreamEvent) {
	select {
	case events <- event:
	default:
	}
}
