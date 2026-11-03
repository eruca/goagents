package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/eruca/goagents/ocrs"
)

var (
	ErrNoEndpoint = errors.New("no endpoint configured")
	ErrClosed     = errors.New("dispatcher closed")
)

// Dispatcher runs calls on a pool of workers shared across providers. Every
// worker takes the next call from one queue as soon as it is free, so load
// follows each provider's capacity (its Workers count and speed) and a call
// never waits behind a busy provider while another one is idle.
type Dispatcher[I, O any] struct {
	jobs   chan request[I, O]
	closed chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

type request[I, O any] struct {
	ctx   context.Context
	data  I
	reply chan response[O]
}

type response[O any] struct {
	value O
	err   error
}

func NewDispatcher[I, O any](providers []ocrs.ProviderConfig[I, O]) (*Dispatcher[I, O], error) {
	if len(providers) == 0 {
		return nil, ErrNoEndpoint
	}
	for i, provider := range providers {
		if provider.Provider == nil {
			return nil, fmt.Errorf("provider at index %d is nil", i)
		}
	}

	d := &Dispatcher[I, O]{
		// Unbuffered: Handle blocks until a worker takes the call, so a
		// cancelled caller never leaves a stale call queued.
		jobs:   make(chan request[I, O]),
		closed: make(chan struct{}),
	}
	for _, provider := range providers {
		for range max(provider.Workers, 1) {
			d.wg.Add(1)
			go d.worker(provider.Provider)
		}
	}
	return d, nil
}

// Close stops the workers after their current calls; pending and later Handle
// calls return ErrClosed.
func (d *Dispatcher[I, O]) Close() {
	d.once.Do(func() {
		close(d.closed)
		d.wg.Wait()
	})
}

func (d *Dispatcher[I, O]) Handle(ctx context.Context, data I) (O, error) {
	var zero O
	reply := make(chan response[O], 1)
	select {
	case d.jobs <- request[I, O]{ctx: ctx, data: data, reply: reply}:
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-d.closed:
		return zero, ErrClosed
	}
	select {
	case out := <-reply:
		return out.value, out.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func (d *Dispatcher[I, O]) worker(provider ocrs.Provider[I, O]) {
	defer d.wg.Done()
	for {
		select {
		case <-d.closed:
			return
		case req := <-d.jobs:
			if err := req.ctx.Err(); err != nil {
				req.reply <- response[O]{err: err}
				continue
			}
			value, err := provider.Handle(req.ctx, req.data)
			req.reply <- response[O]{value: value, err: err}
		}
	}
}
