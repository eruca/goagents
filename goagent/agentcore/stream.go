package agentcore

import (
	"context"
	"sync"
)

const runStreamBuffer = 64

type RunStream struct {
	Events      <-chan RunStreamEvent
	done        <-chan runStreamDone
	discard     chan struct{}
	discardOnce sync.Once
	relayDone   <-chan struct{}
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
	events := make(chan RunStreamEvent)
	pending := make(chan RunStreamEvent, runStreamBuffer)
	done := make(chan runStreamDone, 1)
	discard := make(chan struct{})
	relayDone := make(chan struct{})
	stream := &RunStream{
		Events:    events,
		done:      done,
		discard:   discard,
		relayDone: relayDone,
	}
	sink := streamEventSink{
		events: pending,
		next:   a.eventSink,
	}

	go relayRunStreamEvents(pending, events, discard, relayDone)
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

// DiscardEvents stops public event delivery without canceling the Agent run.
// Call it when Events will not be drained; repeated calls are safe.
func (s *RunStream) DiscardEvents() {
	s.discardOnce.Do(func() {
		close(s.discard)
	})
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
// It preserves FIFO delivery while a consumer is active. After discard it
// clears queued events but keeps draining pending so the Agent cannot block.
func relayRunStreamEvents(
	pending <-chan RunStreamEvent,
	events chan<- RunStreamEvent,
	discard <-chan struct{},
	relayDone chan<- struct{},
) {
	eventsOpen := true
	defer func() {
		if eventsOpen {
			close(events)
		}
		close(relayDone)
	}()

	queue := make([]RunStreamEvent, 0, runStreamBuffer)
	discarding := false
	for pending != nil || (!discarding && len(queue) > 0) {
		if discarding {
			if _, ok := <-pending; !ok {
				pending = nil
			}
			continue
		}

		var output chan<- RunStreamEvent
		var next RunStreamEvent
		if len(queue) > 0 {
			output = events
			next = queue[0]
		}

		select {
		case <-discard:
			clear(queue)
			queue = nil
			discarding = true
			close(events)
			eventsOpen = false
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
