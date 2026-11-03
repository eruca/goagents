package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eruca/goagents/ocrs"
)

type fakeProvider struct {
	name  string
	delay time.Duration

	mu        sync.Mutex
	active    int
	maxActive int
}

func (f *fakeProvider) Family() string { return "fake" }
func (f *fakeProvider) Name() string   { return f.name }

func (f *fakeProvider) Handle(ctx context.Context, data []byte) (string, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()

	select {
	case <-ctx.Done():
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
		return "", ctx.Err()
	case <-time.After(f.delay):
	}

	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return f.name + ":" + string(data), nil
}

func TestHandleSuccess(t *testing.T) {
	p := &fakeProvider{name: "paddle", delay: 5 * time.Millisecond}
	d, err := NewDispatcher[[]byte, string]([]ocrs.ProviderConfig[[]byte, string]{
		{Provider: p, Workers: 1},
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	defer d.Close()

	got, err := d.Handle(context.Background(), []byte("pdf"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got != "paddle:pdf" {
		t.Fatalf("unexpected result: %q", got)
	}
}

func TestWorkersOneMeansSerialExecution(t *testing.T) {
	p := &fakeProvider{name: "paddle", delay: 40 * time.Millisecond}
	d, err := NewDispatcher[[]byte, string]([]ocrs.ProviderConfig[[]byte, string]{
		{Provider: p, Workers: 1},
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	defer d.Close()

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.Handle(context.Background(), []byte("doc"))
		}()
	}
	wg.Wait()

	p.mu.Lock()
	maxActive := p.maxActive
	p.mu.Unlock()

	if maxActive != 1 {
		t.Fatalf("expected max concurrent workers = 1, got %d", maxActive)
	}
}

func TestNewDispatcherNoProvider(t *testing.T) {
	t.Parallel()

	_, err := NewDispatcher[[]byte, string](nil)
	if err != ErrNoEndpoint {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestNewDispatcherNilProvider(t *testing.T) {
	t.Parallel()

	_, err := NewDispatcher[[]byte, string]([]ocrs.ProviderConfig[[]byte, string]{
		{Provider: nil, Workers: 1},
	})
	if err == nil {
		t.Fatalf("expected error for nil provider")
	}
}

func TestCloseWhileHandleIsEnqueueingDoesNotPanicOrHang(t *testing.T) {
	p := &fakeProvider{name: "paddle", delay: 20 * time.Millisecond}
	d, err := NewDispatcher[[]byte, string]([]ocrs.ProviderConfig[[]byte, string]{
		{Provider: p, Workers: 1},
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, _ = d.Handle(ctx, []byte("doc"))
		}()
	}

	time.Sleep(5 * time.Millisecond)
	d.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Handle calls did not return after Close")
	}
}

type gatedProvider struct {
	name    string
	active  atomic.Int32
	release chan struct{}
}

func (g *gatedProvider) Family() string { return "fake" }
func (g *gatedProvider) Name() string   { return g.name }

func (g *gatedProvider) Handle(ctx context.Context, data []byte) (string, error) {
	g.active.Add(1)
	defer g.active.Add(-1)
	select {
	case <-g.release:
		return g.name, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestBusyProviderNeverHoldsCallsWhileAnotherIsIdle(t *testing.T) {
	slow := &gatedProvider{name: "slow", release: make(chan struct{})}
	fast := &fakeProvider{name: "fast"}
	d, err := NewDispatcher([]ocrs.ProviderConfig[[]byte, string]{
		{Provider: slow, Workers: 1},
		{Provider: fast, Workers: 1},
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	defer d.Close()
	defer close(slow.release)

	// Keep calling until the slow provider holds one call.
	deadline := time.Now().Add(5 * time.Second)
	for slow.active.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("slow provider never took a call")
		}
		go func() { _, _ = d.Handle(context.Background(), []byte("occupy")) }()
		time.Sleep(time.Millisecond)
	}

	for i := range 10 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		got, err := d.Handle(ctx, []byte("doc"))
		cancel()
		if err != nil || got != "fast:doc" {
			t.Fatalf("call %d = %q, %v; want served by the idle fast provider", i, got, err)
		}
	}
}

func TestLoadFollowsProviderCapacity(t *testing.T) {
	wide := &fakeProvider{name: "wide", delay: 10 * time.Millisecond}
	narrow := &fakeProvider{name: "narrow", delay: 10 * time.Millisecond}
	d, err := NewDispatcher([]ocrs.ProviderConfig[[]byte, string]{
		{Provider: wide, Workers: 4},
		{Provider: narrow, Workers: 1},
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	defer d.Close()

	var wideCalls, narrowCalls atomic.Int32
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := d.Handle(context.Background(), []byte("doc"))
			if err != nil {
				t.Error(err)
				return
			}
			if got == "wide:doc" {
				wideCalls.Add(1)
			} else {
				narrowCalls.Add(1)
			}
		}()
	}
	wg.Wait()
	// Four workers against one: round-robin would split 20/20.
	if wideCalls.Load() < 2*narrowCalls.Load() {
		t.Fatalf("wide/narrow calls = %d/%d, want load to follow worker count", wideCalls.Load(), narrowCalls.Load())
	}
}
