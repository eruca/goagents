package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eruca/ocrs"
)

var ErrNoEndpoint = errors.New("no endpoint configured")

type Config struct {
	QueueSize    int
	PollInterval time.Duration
}

type Dispatcher[I, O any] struct {
	states []*endpointState[I, O]
	next   atomic.Uint64
	closed chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
	cfg    Config
}

type endpointState[I, O any] struct {
	ep   ocrs.ProviderConfig[I, O]
	jobs chan request[I, O]
}

type request[I, O any] struct {
	ctx   context.Context
	data  I
	reply chan response[I, O]
}

type response[I, O any] struct {
	value O
	err   error
}

func NewDispatcher[I, O any](providers []ocrs.ProviderConfig[I, O], cfg Config) (*Dispatcher[I, O], error) {
	if len(providers) == 0 {
		return nil, ErrNoEndpoint
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 64
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 50 * time.Millisecond
	}

	d := &Dispatcher[I, O]{
		closed: make(chan struct{}),
		cfg:    cfg,
	}

	for i, provider := range providers {
		if provider.Provider == nil {
			return nil, fmt.Errorf("provider at index %d is nil", i)
		}
		workers := provider.Workers
		if workers <= 0 {
			workers = 1
		}

		st := &endpointState[I, O]{
			ep:   provider,
			jobs: make(chan request[I, O], cfg.QueueSize),
		}
		d.states = append(d.states, st)

		for range workers {
			d.wg.Add(1)
			go d.worker(st)
		}
	}

	return d, nil
}

func (d *Dispatcher[I, O]) Close() {
	d.once.Do(func() {
		close(d.closed)
		d.wg.Wait()
	})
}

func (d *Dispatcher[I, O]) Handle(ctx context.Context, data I) (O, error) {
	var zero O
	reply := make(chan response[I, O], 1)
	req := request[I, O]{
		ctx:   ctx,
		data:  data,
		reply: reply,
	}

	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	for {
		if st, ok := d.pickEndpointForEnqueue(); ok {
			select {
			case st.jobs <- req:
				select {
				case out := <-reply:
					return out.value, out.err
				case <-ctx.Done():
					return zero, ctx.Err()
				case <-d.closed:
					return zero, errors.New("dispatcher closed")
				}
			case <-d.closed:
				return zero, errors.New("dispatcher closed")
			default:
			}
		}

		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-d.closed:
			return zero, errors.New("dispatcher closed")
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher[I, O]) pickEndpointForEnqueue() (*endpointState[I, O], bool) {
	if len(d.states) == 0 {
		return nil, false
	}
	idx := (d.next.Add(1) - 1) % uint64(len(d.states))
	return d.states[idx], true
}

func (d *Dispatcher[I, O]) worker(st *endpointState[I, O]) {
	defer d.wg.Done()

	for {
		select {
		case <-d.closed:
			return
		case req := <-st.jobs:
			d.handleRequest(st, req)
		}
	}
}

func (d *Dispatcher[I, O]) handleRequest(st *endpointState[I, O], req request[I, O]) {
	select {
	case <-req.ctx.Done():
		req.reply <- response[I, O]{err: req.ctx.Err()}
		return
	case <-d.closed:
		req.reply <- response[I, O]{err: errors.New("dispatcher closed")}
		return
	default:
	}

	value, err := st.ep.Provider.Handle(req.ctx, req.data)
	req.reply <- response[I, O]{value: value, err: err}
}
