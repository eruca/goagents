package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/eruca/ocrs"
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
	}, Config{QueueSize: 8})
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
	}, Config{QueueSize: 16, PollInterval: 5 * time.Millisecond})
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

	_, err := NewDispatcher[[]byte, string](nil, Config{})
	if err != ErrNoEndpoint {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestNewDispatcherNilProvider(t *testing.T) {
	t.Parallel()

	_, err := NewDispatcher[[]byte, string]([]ocrs.ProviderConfig[[]byte, string]{
		{Provider: nil, Workers: 1},
	}, Config{})
	if err == nil {
		t.Fatalf("expected error for nil provider")
	}
}

func TestCloseWhileHandleIsEnqueueingDoesNotPanicOrHang(t *testing.T) {
	p := &fakeProvider{name: "paddle", delay: 20 * time.Millisecond}
	d, err := NewDispatcher[[]byte, string]([]ocrs.ProviderConfig[[]byte, string]{
		{Provider: p, Workers: 1},
	}, Config{QueueSize: 1, PollInterval: time.Millisecond})
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
