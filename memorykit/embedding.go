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
	Store      EmbeddingStore
	Embedder   Embedder
	ProfileID  string
	Dimensions int
	BatchSize  int
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
	embedder   Embedder
	profileID  string
	dimensions int
	batchSize  int
}

func NewEmbeddingWorker(config EmbeddingWorkerConfig) (*EmbeddingWorker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &EmbeddingWorker{
		store:      config.Store,
		embedder:   config.Embedder,
		profileID:  config.ProfileID,
		dimensions: config.Dimensions,
		batchSize:  config.BatchSize,
	}, nil
}

// RunOnce processes at most one Store-ordered batch. Host owns scheduling and retries.
func (w *EmbeddingWorker) RunOnce(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	inputs, err := w.store.PendingEmbeddings(ctx, PendingEmbeddingQuery{
		ProfileID: w.profileID,
		Limit:     w.batchSize,
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	if err != nil {
		return 0, err
	}
	if len(inputs) > w.batchSize {
		return 0, fmt.Errorf("%w: pending embedding batch exceeds configured limit", ErrInvalidMemory)
	}
	if len(inputs) == 0 {
		return 0, nil
	}

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
		return stored, err
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
		return nil, err
	}
	if err := validateEmbeddingBatch(vectors, len(inputs), w.dimensions); err != nil {
		return nil, err
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
