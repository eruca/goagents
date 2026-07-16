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
	pending := make(chan RunStreamEvent, runStreamBuffer)
	done := make(chan runStreamDone, 1)
	stream := &RunStream{Events: events, done: done}
	sink := streamEventSink{
		events: pending,
		next:   a.eventSink,
	}

	go relayRunStreamEvents(pending, events)
	go func() {
		defer close(pending)
		result, err := a.runWithEventSink(ctx, req, true, sink)
		sendRunStreamEvent(pending, RunStreamEvent{Result: result, Error: err, Done: true})
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
	events <- event
}

// relayRunStreamEvents decouples execution from the public stream consumer.
// The queue preserves event order without silently dropping tail events when
// the public channel is full; Wait can still finish before Events is consumed.
func relayRunStreamEvents(pending <-chan RunStreamEvent, events chan<- RunStreamEvent) {
	defer close(events)

	queue := make([]RunStreamEvent, 0, runStreamBuffer)
	for pending != nil || len(queue) > 0 {
		var output chan<- RunStreamEvent
		var next RunStreamEvent
		if len(queue) > 0 {
			output = events
			next = queue[0]
		}

		select {
		case event, ok := <-pending:
			if !ok {
				pending = nil
				continue
			}
			queue = append(queue, event)
		case output <- next:
			queue[0] = RunStreamEvent{}
			queue = queue[1:]
		}
	}
}
