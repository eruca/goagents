package memorykit

import (
	"context"
	"errors"
	"testing"
)

// fakeRunGate runs run when open is true and otherwise reports another
// holder. acquireErr fails the gate before run; releaseErr fails it after.
type fakeRunGate struct {
	open       bool
	acquireErr error
	releaseErr error
	calls      int
}

func (g *fakeRunGate) TryRun(ctx context.Context, run func(context.Context) error) (bool, error) {
	g.calls++
	if g.acquireErr != nil {
		return false, g.acquireErr
	}
	if !g.open {
		return false, nil
	}
	err := run(ctx)
	if g.releaseErr != nil {
		return true, errors.Join(err, g.releaseErr)
	}
	return true, err
}

func gatedEmbeddingWorker(t *testing.T, gate RunGate) (*EmbeddingWorker, *embeddingWorkerStore, *embeddingWorkerEmbedder) {
	t.Helper()
	store := &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()}
	embedder := &embeddingWorkerEmbedder{responses: [][][]float32{{{1, 0, 0}, {0, 1, 0}}}}
	worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
		Store: store, Backlog: &embeddingBacklogStub{}, Embedder: embedder,
		ProfileID: "test-3d", Dimensions: 3, BatchSize: 8, RunGate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker, store, embedder
}

func TestEmbeddingWorkerRunGateHeldElsewhereSkipsRound(t *testing.T) {
	gate := &fakeRunGate{}
	worker, store, embedder := gatedEmbeddingWorker(t, gate)
	stored, err := worker.RunOnce(context.Background())
	if stored != 0 || err != nil {
		t.Fatalf("RunOnce() = %d, %v; want 0, nil", stored, err)
	}
	if gate.calls != 1 || len(store.pendingCalls) != 0 || len(embedder.requests) != 0 || len(store.putRequests) != 0 {
		t.Fatalf("skipped round touched the store or embedder: gate=%d pending=%d embed=%d put=%d",
			gate.calls, len(store.pendingCalls), len(embedder.requests), len(store.putRequests))
	}
	assertWorkerSnapshot(t, embeddingSnapshot(t, worker), 0, 0, 0, "")
}

func TestEmbeddingWorkerRunGateOpenRunsBatch(t *testing.T) {
	gate := &fakeRunGate{open: true}
	worker, store, embedder := gatedEmbeddingWorker(t, gate)
	stored, err := worker.RunOnce(context.Background())
	if stored != 2 || err != nil {
		t.Fatalf("RunOnce() = %d, %v; want 2, nil", stored, err)
	}
	if len(embedder.requests) != 1 || len(store.putRequests) != 2 {
		t.Fatalf("embed=%d put=%d, want 1 and 2", len(embedder.requests), len(store.putRequests))
	}
	assertWorkerSnapshot(t, embeddingSnapshot(t, worker), 0, 1, 0, "")

	// An idle round inside the gate is still idle.
	store.pendingInputs = nil
	if stored, err := worker.RunOnce(context.Background()); stored != 0 || err != nil {
		t.Fatalf("idle RunOnce() = %d, %v", stored, err)
	}
	assertWorkerSnapshot(t, embeddingSnapshot(t, worker), 0, 1, 0, "")
}

func TestEmbeddingWorkerRunGateErrorsAreFailures(t *testing.T) {
	acquireErr := &BackendError{Op: "gate", Recoverable: true, Err: errors.New("down")}
	gate := &fakeRunGate{open: true, acquireErr: acquireErr}
	worker, _, embedder := gatedEmbeddingWorker(t, gate)
	if _, err := worker.RunOnce(context.Background()); !errors.Is(err, acquireErr) || !IsRecoverable(err) {
		t.Fatalf("RunOnce() error = %v, want the gate error", err)
	}
	if len(embedder.requests) != 0 {
		t.Fatal("embedder called although the gate failed")
	}
	assertWorkerSnapshot(t, embeddingSnapshot(t, worker), 0, 0, 1, WorkerErrorPendingReadFailed)

	// A failed release after a good batch is still reported.
	releaseErr := errors.New("unlock failed")
	gate.acquireErr, gate.releaseErr = nil, releaseErr
	stored, err := worker.RunOnce(context.Background())
	if stored != 2 || !errors.Is(err, releaseErr) {
		t.Fatalf("RunOnce() = %d, %v; want 2 and the release error", stored, err)
	}
	assertWorkerSnapshot(t, embeddingSnapshot(t, worker), 0, 0, 2, WorkerErrorPendingReadFailed)

	// A batch error inside the gate keeps its own class.
	gate.releaseErr = nil
	embedder.errors = []error{nil, &BackendError{Op: "embed", Recoverable: true, Err: errors.New("provider")}}
	if _, err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want the embedder error")
	}
	assertWorkerSnapshot(t, embeddingSnapshot(t, worker), 0, 0, 3, WorkerErrorProviderFailed)

	// A canceled context is classed as cancellation, not as a gate failure.
	gate.acquireErr = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := worker.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce() error = %v, want context.Canceled", err)
	}
	assertWorkerSnapshot(t, embeddingSnapshot(t, worker), 0, 0, 4, WorkerErrorContextCanceled)
}

func TestEmbeddingWorkerTypedNilRunGateIsIgnored(t *testing.T) {
	var gate *fakeRunGate
	worker, _, embedder := gatedEmbeddingWorker(t, gate)
	if stored, err := worker.RunOnce(context.Background()); stored != 2 || err != nil || len(embedder.requests) != 1 {
		t.Fatalf("RunOnce() = %d, %v with %d embeds", stored, err, len(embedder.requests))
	}
}
