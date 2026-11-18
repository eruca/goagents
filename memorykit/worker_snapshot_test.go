package memorykit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type embeddingBacklogStub struct {
	count   int64
	err     error
	queries []EmbeddingBacklogQuery
}

func (s *embeddingBacklogStub) CountPendingEmbeddings(_ context.Context, query EmbeddingBacklogQuery) (int64, error) {
	s.queries = append(s.queries, query)
	return s.count, s.err
}

// countingEmbeddingStore is an EmbeddingStore that can also count its backlog.
type countingEmbeddingStore struct {
	embeddingWorkerStore
	embeddingBacklogStub
}

type extractionBacklogStub struct {
	count   int64
	err     error
	queries []ExtractionBacklogQuery
}

func (s *extractionBacklogStub) CountPendingExtractions(_ context.Context, query ExtractionBacklogQuery) (int64, error) {
	s.queries = append(s.queries, query)
	return s.count, s.err
}

type countingExtractionJobStore struct {
	extractionJobStoreStub
	extractionBacklogStub
}

func TestEmbeddingWorkerSnapshotCountsNonIdleRuns(t *testing.T) {
	store := &embeddingWorkerStore{}
	embedder := &embeddingWorkerEmbedder{}
	backlog := &embeddingBacklogStub{count: 7}
	worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
		Store: store, Backlog: backlog, Embedder: embedder, ProfileID: "test-3d", Dimensions: 3, BatchSize: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWorkerSnapshot(t, embeddingSnapshot(t, worker), 7, 0, 0, "")

	store.pendingInputs = embeddingWorkerInputs()
	embedder.responses = [][][]float32{{{1, 0, 0}, {0, 1, 0}}}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := embeddingSnapshot(t, worker)
	assertWorkerSnapshot(t, snapshot, 7, 1, 0, "")
	if snapshot.LastTransitionTime.IsZero() || snapshot.LastTransitionTime.Location() != time.UTC {
		t.Fatalf("LastTransitionTime = %v, want a UTC time", snapshot.LastTransitionTime)
	}
	if len(backlog.queries) != 2 || backlog.queries[0].ProfileID != "test-3d" {
		t.Fatalf("backlog queries = %#v, want the worker profile", backlog.queries)
	}
}

func TestEmbeddingWorkerSnapshotClassifiesFailures(t *testing.T) {
	private := errors.New("private memory text must not leak")
	tests := []struct {
		name     string
		store    *embeddingWorkerStore
		embedder *embeddingWorkerEmbedder
		ctx      func() context.Context
		want     WorkerErrorClass
	}{
		{
			name:     "pending read",
			store:    &embeddingWorkerStore{pendingErr: private},
			embedder: &embeddingWorkerEmbedder{},
			want:     WorkerErrorPendingReadFailed,
		},
		{
			name:     "provider",
			store:    &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()[:1]},
			embedder: &embeddingWorkerEmbedder{errors: []error{private}},
			want:     WorkerErrorProviderFailed,
		},
		{
			name:     "provider result",
			store:    &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()[:1]},
			embedder: &embeddingWorkerEmbedder{responses: [][][]float32{{{1, 0}}}},
			want:     WorkerErrorProviderResultInvalid,
		},
		{
			name:  "skipped input",
			store: &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()},
			embedder: &embeddingWorkerEmbedder{
				responses: [][][]float32{nil, nil, {{0, 1, 0}}},
				errors:    []error{private, private, nil},
			},
			want: WorkerErrorProviderFailed,
		},
		{
			name:     "write",
			store:    &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()[:1], putErrors: []error{private}},
			embedder: &embeddingWorkerEmbedder{responses: [][][]float32{{{1, 0, 0}}}},
			want:     WorkerErrorEmbeddingWriteFailed,
		},
		{
			name:     "canceled",
			store:    &embeddingWorkerStore{pendingInputs: embeddingWorkerInputs()[:1]},
			embedder: &embeddingWorkerEmbedder{},
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: WorkerErrorContextCanceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
				Store: test.store, Backlog: &embeddingBacklogStub{}, Embedder: test.embedder,
				ProfileID: "test-3d", Dimensions: 3, BatchSize: 8,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if test.ctx != nil {
				ctx = test.ctx()
			}
			if _, err := worker.RunOnce(ctx); err == nil {
				t.Fatal("RunOnce() error = nil, want failure")
			}
			snapshot, _ := worker.Snapshot(context.Background())
			assertWorkerSnapshot(t, snapshot, 0, 0, 1, test.want)
		})
	}
}

func TestEmbeddingWorkerLaterSuccessKeepsLastErrorClass(t *testing.T) {
	store := &embeddingWorkerStore{pendingErr: errors.New("down")}
	embedder := &embeddingWorkerEmbedder{responses: [][][]float32{{{1, 0, 0}}}}
	worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
		Store: store, Backlog: &embeddingBacklogStub{}, Embedder: embedder,
		ProfileID: "test-3d", Dimensions: 3, BatchSize: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = worker.RunOnce(context.Background())
	store.pendingErr = nil
	store.pendingInputs = embeddingWorkerInputs()[:1]
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWorkerSnapshot(t, embeddingSnapshot(t, worker), 0, 1, 1, WorkerErrorPendingReadFailed)
}

func TestEmbeddingWorkerSnapshotBacklogSource(t *testing.T) {
	t.Run("store fallback", func(t *testing.T) {
		store := &countingEmbeddingStore{embeddingBacklogStub: embeddingBacklogStub{count: 4}}
		worker := mustEmbeddingWorker(t, store, &embeddingWorkerEmbedder{})
		if got := embeddingSnapshot(t, worker).Backlog; got != 4 {
			t.Fatalf("Backlog = %d, want 4 from the store", got)
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		worker := mustEmbeddingWorker(t, &embeddingWorkerStore{}, &embeddingWorkerEmbedder{})
		if _, err := worker.Snapshot(context.Background()); !errors.Is(err, ErrWorkerBacklogUnavailable) {
			t.Fatalf("Snapshot() error = %v, want ErrWorkerBacklogUnavailable", err)
		}
	})
	t.Run("read error keeps counters", func(t *testing.T) {
		readErr := errors.New("backlog down")
		worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
			Store: &embeddingWorkerStore{pendingErr: errors.New("down")}, Backlog: &embeddingBacklogStub{err: readErr},
			Embedder: &embeddingWorkerEmbedder{}, ProfileID: "test-3d", Dimensions: 3, BatchSize: 8,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = worker.RunOnce(context.Background())
		snapshot, err := worker.Snapshot(context.Background())
		if !errors.Is(err, readErr) || snapshot.FailureTotal != 1 {
			t.Fatalf("Snapshot() = %#v, %v; want counters and the read error", snapshot, err)
		}
	})
	t.Run("negative count", func(t *testing.T) {
		worker, err := NewEmbeddingWorker(EmbeddingWorkerConfig{
			Store: &embeddingWorkerStore{}, Backlog: &embeddingBacklogStub{count: -1},
			Embedder: &embeddingWorkerEmbedder{}, ProfileID: "test-3d", Dimensions: 3, BatchSize: 8,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := worker.Snapshot(context.Background()); !errors.Is(err, ErrInvalidMemory) {
			t.Fatalf("Snapshot() error = %v, want ErrInvalidMemory", err)
		}
	})
}

func TestExtractionWorkerSnapshotCountsRuns(t *testing.T) {
	jobs := &extractionJobStoreStub{claimErr: ErrNoExtractionJob}
	backlog := &extractionBacklogStub{count: 2}
	config := extractionWorkerTestConfig()
	config.Jobs = jobs
	config.Backlog = backlog
	worker, err := NewExtractionWorker(config)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWorkerSnapshot(t, extractionSnapshot(t, worker), 2, 0, 0, "")

	jobs.claimErr = nil
	jobs.claimJob = extractionWorkerTestJob()
	if worked, err := worker.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("RunOnce() = %v, %v", worked, err)
	}
	snapshot := extractionSnapshot(t, worker)
	assertWorkerSnapshot(t, snapshot, 2, 1, 0, "")
	if want := config.Now(); !snapshot.LastTransitionTime.Equal(want) {
		t.Fatalf("LastTransitionTime = %v, want worker clock %v", snapshot.LastTransitionTime, want)
	}
	if len(backlog.queries) != 2 || !backlog.queries[1].Now.Equal(config.Now()) || backlog.queries[1].MaxAttempts != 3 {
		t.Fatalf("backlog queries = %#v, want worker clock and max attempts", backlog.queries)
	}
}

func TestExtractionWorkerSnapshotClassifiesFailures(t *testing.T) {
	private := errors.New("private source text must not leak")
	tests := []struct {
		name   string
		mutate func(*ExtractionWorkerConfig, *extractionJobStoreStub)
		want   WorkerErrorClass
	}{
		{name: "claim", want: WorkerErrorClaimFailed, mutate: func(_ *ExtractionWorkerConfig, jobs *extractionJobStoreStub) {
			jobs.claimErr = private
		}},
		{name: "source", want: WorkerErrorSourceReadFailed, mutate: func(c *ExtractionWorkerConfig, _ *extractionJobStoreStub) {
			c.Reader = sourceReaderFunc(func(context.Context, Source) (string, error) { return "", private })
		}},
		{name: "extract", want: WorkerErrorCandidateExtractFailed, mutate: func(c *ExtractionWorkerConfig, _ *extractionJobStoreStub) {
			c.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
				return nil, private
			})
		}},
		{name: "validate", want: WorkerErrorCandidateValidateFailed, mutate: func(c *ExtractionWorkerConfig, _ *extractionJobStoreStub) {
			c.ValidateContent = &contentValidatorStub{err: private}
			c.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
				return []CandidateDraft{{Kind: KindLesson, Key: "k", Content: "c", Confidence: 1}}, nil
			})
		}},
		{name: "write", want: WorkerErrorCandidateWriteFailed, mutate: func(c *ExtractionWorkerConfig, _ *extractionJobStoreStub) {
			c.Memories = &extractionLifecycleStoreStub{createErr: private}
			c.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
				return []CandidateDraft{{Kind: KindLesson, Key: "k", Content: "c", Confidence: 1}}, nil
			})
		}},
		{name: "fail transition", want: WorkerErrorSourceReadFailed, mutate: func(c *ExtractionWorkerConfig, jobs *extractionJobStoreStub) {
			jobs.failErr = private
			c.Reader = sourceReaderFunc(func(context.Context, Source) (string, error) { return "", private })
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobs := &extractionJobStoreStub{claimJob: extractionWorkerTestJob()}
			config := extractionWorkerTestConfig()
			config.Jobs = jobs
			config.Backlog = &extractionBacklogStub{}
			test.mutate(&config, jobs)
			worker, err := NewExtractionWorker(config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := worker.RunOnce(context.Background()); err == nil {
				t.Fatal("RunOnce() error = nil, want failure")
			}
			snapshot := extractionSnapshot(t, worker)
			assertWorkerSnapshot(t, snapshot, 0, 0, 1, test.want)
			if strings.Contains(string(snapshot.LastErrorClass), "private") {
				t.Fatalf("LastErrorClass leaked error text: %q", snapshot.LastErrorClass)
			}
			if test.name != "claim" && jobs.failedCode != string(test.want) {
				t.Fatalf("durable failure code = %q, want %q", jobs.failedCode, test.want)
			}
		})
	}
}

func TestExtractionWorkerSnapshotBacklogSource(t *testing.T) {
	t.Run("jobs fallback", func(t *testing.T) {
		config := extractionWorkerTestConfig()
		config.Jobs = &countingExtractionJobStore{extractionBacklogStub: extractionBacklogStub{count: 5}}
		worker, err := NewExtractionWorker(config)
		if err != nil {
			t.Fatal(err)
		}
		if got := extractionSnapshot(t, worker).Backlog; got != 5 {
			t.Fatalf("Backlog = %d, want 5 from the job store", got)
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		worker, err := NewExtractionWorker(extractionWorkerTestConfig())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := worker.Snapshot(context.Background()); !errors.Is(err, ErrWorkerBacklogUnavailable) {
			t.Fatalf("Snapshot() error = %v, want ErrWorkerBacklogUnavailable", err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		config := extractionWorkerTestConfig()
		config.Backlog = &extractionBacklogStub{}
		worker, err := NewExtractionWorker(config)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := worker.Snapshot(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Snapshot() error = %v, want context.Canceled", err)
		}
	})
}

func embeddingSnapshot(t *testing.T, worker *EmbeddingWorker) WorkerSnapshot {
	t.Helper()
	snapshot, err := worker.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	return snapshot
}

func extractionSnapshot(t *testing.T, worker *ExtractionWorker) WorkerSnapshot {
	t.Helper()
	snapshot, err := worker.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	return snapshot
}

func assertWorkerSnapshot(
	t *testing.T,
	got WorkerSnapshot,
	backlog int64,
	successes, failures uint64,
	class WorkerErrorClass,
) {
	t.Helper()
	if got.Backlog != backlog || got.SuccessTotal != successes || got.FailureTotal != failures || got.LastErrorClass != class {
		t.Fatalf("snapshot = %#v, want backlog %d, successes %d, failures %d, class %q",
			got, backlog, successes, failures, class)
	}
	if (successes+failures == 0) != got.LastTransitionTime.IsZero() {
		t.Fatalf("LastTransitionTime = %v with %d transitions", got.LastTransitionTime, successes+failures)
	}
}
