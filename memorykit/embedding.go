package memorykit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

type EmbeddingWorkerConfig struct {
	Store EmbeddingStore
	// Backlog counts the durable backlog for Snapshot. When nil, Store is
	// used if it implements EmbeddingBacklogReader.
	Backlog    EmbeddingBacklogReader
	Embedder   Embedder
	ProfileID  string
	Dimensions int
	BatchSize  int
	// RunGate, when set, makes RunOnce run its batch only while holding the
	// gate, so that replicas sharing one store do not embed the same batch
	// at once. A round in which another holder has the gate is skipped. Nil
	// runs every round, as before.
	RunGate RunGate
}

// Validate rejects incomplete worker configuration before background work starts.
func (c EmbeddingWorkerConfig) Validate() error {
	if nilInterface(c.Store) || nilInterface(c.Embedder) ||
		strings.TrimSpace(c.ProfileID) == "" || c.ProfileID != strings.TrimSpace(c.ProfileID) ||
		c.Dimensions <= 0 || c.BatchSize <= 0 {
		return fmt.Errorf("%w: invalid embedding worker configuration", ErrInvalidMemory)
	}
	return nil
}

type EmbeddingWorker struct {
	store      EmbeddingStore
	backlog    EmbeddingBacklogReader
	embedder   Embedder
	profileID  string
	dimensions int
	batchSize  int
	gate       RunGate
	stats      workerStats
}

func NewEmbeddingWorker(config EmbeddingWorkerConfig) (*EmbeddingWorker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	backlog := config.Backlog
	if nilInterface(backlog) {
		backlog, _ = config.Store.(EmbeddingBacklogReader)
	}
	gate := config.RunGate
	if nilInterface(gate) {
		gate = nil
	}
	return &EmbeddingWorker{
		store:      config.Store,
		backlog:    backlog,
		embedder:   config.Embedder,
		profileID:  config.ProfileID,
		dimensions: config.Dimensions,
		batchSize:  config.BatchSize,
		gate:       gate,
	}, nil
}

// RunOnce processes at most one Store-ordered batch. Host owns scheduling and retries.
// A run that finds nothing pending is idle and is not counted by Snapshot;
// any other run counts as one success or, if it returns an error, one failure.
//
// With a RunGate, the batch runs inside RunGate.TryRun. A round skipped
// because another holder has the gate returns (0, nil) and, like an idle run,
// is not counted. An error from the gate itself is returned and counted as a
// failure classed like a store read (WorkerErrorPendingReadFailed), unless
// the context was canceled or timed out.
func (w *EmbeddingWorker) RunOnce(ctx context.Context) (int, error) {
	stored, idle, err := w.gatedBatch(ctx)
	if err == nil && idle {
		return 0, nil
	}
	if err != nil {
		w.stats.recordFailure(classifyWorkerError(ctx, err, WorkerErrorProviderFailed), time.Now())
	} else {
		w.stats.recordSuccess(time.Now())
	}
	return stored, err
}

// gatedBatch runs one batch, inside the RunGate when there is one. idle
// reports a round that did no work: nothing pending, or skipped by the gate.
func (w *EmbeddingWorker) gatedBatch(ctx context.Context) (stored int, idle bool, err error) {
	if w.gate == nil {
		return w.batch(ctx)
	}
	called := false
	var runErr error
	_, err = w.gate.TryRun(ctx, func(runCtx context.Context) error {
		called = true
		stored, idle, runErr = w.batch(runCtx)
		return runErr
	})
	if err != nil {
		if !called || runErr == nil {
			// The gate failed to acquire or to release.
			err = withWorkerErrorClass(WorkerErrorPendingReadFailed, err)
		}
		return stored, false, err
	}
	if !called {
		return 0, true, nil
	}
	return stored, idle, nil
}

func (w *EmbeddingWorker) batch(ctx context.Context) (stored int, idle bool, err error) {
	inputs, err := w.pending(ctx)
	if err != nil {
		return 0, false, err
	}
	if len(inputs) == 0 {
		return 0, true, nil
	}
	stored, err = w.process(ctx, inputs)
	return stored, false, err
}

// Snapshot returns the worker's content-free status. The process-local
// counters are read before the durable backlog, so the two are not one atomic
// view. When the backlog cannot be read, the counters come back with the error.
func (w *EmbeddingWorker) Snapshot(ctx context.Context) (WorkerSnapshot, error) {
	snapshot := w.stats.snapshot()
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	if nilInterface(w.backlog) {
		return snapshot, ErrWorkerBacklogUnavailable
	}
	backlog, err := w.backlog.CountPendingEmbeddings(ctx, EmbeddingBacklogQuery{ProfileID: w.profileID})
	if err != nil {
		return snapshot, err
	}
	if backlog < 0 {
		return snapshot, fmt.Errorf("%w: negative embedding backlog", ErrInvalidMemory)
	}
	snapshot.Backlog = backlog
	return snapshot, nil
}

func (w *EmbeddingWorker) pending(ctx context.Context) ([]EmbeddingInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inputs, err := w.store.PendingEmbeddings(ctx, PendingEmbeddingQuery{
		ProfileID: w.profileID,
		Limit:     w.batchSize,
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, withWorkerErrorClass(WorkerErrorPendingReadFailed, err)
	}
	if len(inputs) > w.batchSize {
		return nil, withWorkerErrorClass(WorkerErrorPendingResultInvalid,
			fmt.Errorf("%w: pending embedding batch exceeds configured limit", ErrInvalidMemory))
	}
	return inputs, nil
}

func (w *EmbeddingWorker) process(ctx context.Context, inputs []EmbeddingInput) (int, error) {
	vectors, err := w.embed(ctx, inputs)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	var skipped error
	if err != nil {
		if IsRecoverable(err) || len(inputs) == 1 {
			return 0, err
		}
		// The batch is the oldest pending memories, so one input the embedder
		// rejects would fail every run and nothing behind it would ever be
		// embedded. Embed the inputs one by one and skip those that fail.
		inputs, vectors, skipped = w.embedEach(ctx, inputs)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
	}

	embeddedAt := time.Now().UTC()
	stored := 0
	for index, input := range inputs {
		if err := ctx.Err(); err != nil {
			return stored, err
		}
		err := w.store.PutEmbedding(ctx, PutEmbeddingRequest{
			MemoryID:    input.MemoryID,
			Scope:       input.Scope,
			ProfileID:   w.profileID,
			ContentHash: input.ContentHash,
			Dimensions:  w.dimensions,
			Vector:      append([]float32(nil), vectors[index]...),
			EmbeddedAt:  embeddedAt,
		})
		if ctxErr := ctx.Err(); ctxErr != nil {
			if err == nil {
				stored++
			}
			return stored, ctxErr
		}
		if err == nil {
			stored++
			continue
		}
		if errors.Is(err, ErrConflict) {
			continue
		}
		return stored, withWorkerErrorClass(WorkerErrorEmbeddingWriteFailed, err)
	}
	return stored, skipped
}

func (w *EmbeddingWorker) embed(ctx context.Context, inputs []EmbeddingInput) ([][]float32, error) {
	texts := make([]string, len(inputs))
	for index := range inputs {
		texts[index] = inputs[index].Content
	}
	vectors, err := w.embedder.Embed(ctx, EmbedRequest{ProfileID: w.profileID, Texts: texts})
	if err != nil {
		return nil, withWorkerErrorClass(WorkerErrorProviderFailed, err)
	}
	if err := validateEmbeddingBatch(vectors, len(inputs), w.dimensions); err != nil {
		return nil, withWorkerErrorClass(WorkerErrorProviderResultInvalid, err)
	}
	return vectors, nil
}

// embedEach embeds inputs one at a time and returns those that succeeded, with
// an error naming how many were skipped. It stops on a recoverable error,
// which says the embedder itself is failing.
func (w *EmbeddingWorker) embedEach(ctx context.Context, inputs []EmbeddingInput) ([]EmbeddingInput, [][]float32, error) {
	var kept []EmbeddingInput
	var vectors [][]float32
	var firstErr error
	failed := 0
	for index, input := range inputs {
		if ctx.Err() != nil {
			break
		}
		vector, err := w.embed(ctx, []EmbeddingInput{input})
		if err == nil {
			kept = append(kept, input)
			vectors = append(vectors, vector[0])
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
		if IsRecoverable(err) {
			failed += len(inputs) - index
			break
		}
		failed++
	}
	if failed == 0 {
		return kept, vectors, nil
	}
	return kept, vectors, fmt.Errorf("%d of %d pending memories could not be embedded: %w", failed, len(inputs), firstErr)
}

func validateEmbeddingBatch(vectors [][]float32, count, dimensions int) error {
	if len(vectors) != count {
		return fmt.Errorf("%w: invalid embedding batch result", ErrInvalidMemory)
	}
	for _, vector := range vectors {
		if len(vector) != dimensions || !validVector(vector) {
			return fmt.Errorf("%w: invalid embedding batch result", ErrInvalidMemory)
		}
	}
	return nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
