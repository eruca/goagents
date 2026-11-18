package pgstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
)

const runGateMemoryID = "63000000-0000-4000-8000-000000000001"

func TestEmbeddingRunGateExcludesOtherStores(t *testing.T) {
	stores := openIsolatedSchemaStores(t, 2)
	gateA, gateB := stores[0].EmbeddingRunGate(), stores[1].EmbeddingRunGate()

	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ran, err := gateA.TryRun(context.Background(), func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
		if err == nil && !ran {
			err = errors.New("gate A did not run")
		}
		done <- err
	}()
	<-entered

	ran, err := gateB.TryRun(context.Background(), func(context.Context) error {
		t.Error("gate B ran while gate A held the lock")
		return nil
	})
	if ran || err != nil {
		t.Fatalf("gate B TryRun() = %v, %v; want false, nil", ran, err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("gate A TryRun() error = %v", err)
	}
	assertRunGateRuns(t, gateB)
}

func TestEmbeddingRunGateIsScopedToSchema(t *testing.T) {
	gateA := openIsolatedSchemaStore(t).EmbeddingRunGate()
	gateB := openIsolatedSchemaStore(t).EmbeddingRunGate()
	ran, err := gateA.TryRun(context.Background(), func(context.Context) error {
		assertRunGateRuns(t, gateB)
		return nil
	})
	if !ran || err != nil {
		t.Fatalf("gate A TryRun() = %v, %v", ran, err)
	}
}

func TestEmbeddingRunGateReleasesAfterCancelPanicAndError(t *testing.T) {
	stores := openIsolatedSchemaStores(t, 2)
	gateA, gateB := stores[0].EmbeddingRunGate(), stores[1].EmbeddingRunGate()

	ctx, cancel := context.WithCancel(context.Background())
	ran, err := gateA.TryRun(ctx, func(runCtx context.Context) error {
		cancel()
		<-runCtx.Done()
		return runCtx.Err()
	})
	if !ran || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled TryRun() = %v, %v; want true, context.Canceled", ran, err)
	}
	assertRunGateRuns(t, gateB)

	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recover() = %v, want the run panic", recovered)
			}
		}()
		_, _ = gateA.TryRun(context.Background(), func(context.Context) error { panic("boom") })
	}()
	assertRunGateRuns(t, gateB)

	runErr := errors.New("run failed")
	if ran, err := gateA.TryRun(context.Background(), func(context.Context) error { return runErr }); !ran || !errors.Is(err, runErr) {
		t.Fatalf("failing TryRun() = %v, %v; want true and the run error", ran, err)
	}
	assertRunGateRuns(t, gateB)

	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if ran, err := gateA.TryRun(canceled, func(context.Context) error {
		t.Error("run called with a canceled context")
		return nil
	}); ran || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled TryRun() = %v, %v", ran, err)
	}
	assertRunGateRuns(t, gateB)
}

func TestEmbeddingWorkersShareRunGate(t *testing.T) {
	stores := openIsolatedSchemaStores(t, 2)
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	scope := memorykit.Scope{TenantID: conformanceTenantID, SubjectType: memorykit.SubjectProject, SubjectID: "project-run-gate"}
	createQueryMemory(t, stores[0], queryCreateRequest(runGateMemoryID, scope, memorykit.StatusActive, "run.gate", "run gate content", now, 70))

	embedder := &blockingEmbedder{entered: make(chan struct{}, 2), release: make(chan struct{})}
	workers := make([]*memorykit.EmbeddingWorker, len(stores))
	for index, store := range stores {
		worker, err := memorykit.NewEmbeddingWorker(memorykit.EmbeddingWorkerConfig{
			Store: store, Embedder: embedder, ProfileID: "test-3d", Dimensions: 3, BatchSize: 8,
			RunGate: store.EmbeddingRunGate(),
		})
		if err != nil {
			t.Fatal(err)
		}
		workers[index] = worker
	}

	type result struct {
		stored int
		err    error
	}
	results := make(chan result, len(workers))
	var start sync.WaitGroup
	start.Add(1)
	for _, worker := range workers {
		go func() {
			start.Wait()
			stored, err := worker.RunOnce(context.Background())
			results <- result{stored, err}
		}()
	}
	start.Done()

	// One worker reaches the embedder and blocks there; the other must
	// return a skipped round without calling it.
	<-embedder.entered
	skipped := <-results
	if skipped.stored != 0 || skipped.err != nil {
		t.Fatalf("second worker RunOnce() = %d, %v; want 0, nil", skipped.stored, skipped.err)
	}
	close(embedder.release)
	winner := <-results
	if winner.stored != 1 || winner.err != nil {
		t.Fatalf("winning worker RunOnce() = %d, %v; want 1, nil", winner.stored, winner.err)
	}
	if calls := embedder.calls.Load(); calls != 1 {
		t.Fatalf("embedder calls = %d, want 1", calls)
	}

	var successes, failures uint64
	for _, worker := range workers {
		snapshot, err := worker.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		successes += snapshot.SuccessTotal
		failures += snapshot.FailureTotal
		if snapshot.Backlog != 0 {
			t.Fatalf("backlog = %d, want 0", snapshot.Backlog)
		}
	}
	if successes != 1 || failures != 0 {
		t.Fatalf("successes, failures = %d, %d; want 1, 0 (a skipped round is not counted)", successes, failures)
	}
}

func assertRunGateRuns(t *testing.T, gate memorykit.RunGate) {
	t.Helper()
	called := false
	ran, err := gate.TryRun(context.Background(), func(context.Context) error {
		called = true
		return nil
	})
	if !ran || err != nil || !called {
		t.Fatalf("TryRun() = %v, %v (called %v); want the lock to be free", ran, err, called)
	}
}

type blockingEmbedder struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (e *blockingEmbedder) Embed(ctx context.Context, request memorykit.EmbedRequest) ([][]float32, error) {
	e.calls.Add(1)
	e.entered <- struct{}{}
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	vectors := make([][]float32, len(request.Texts))
	for index := range vectors {
		vectors[index] = []float32{1, 0, 0}
	}
	return vectors, nil
}
