package memorykit

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	if len(inputs) == 0 {
		return 0, nil
	}

	texts := make([]string, len(inputs))
	for index := range inputs {
		texts[index] = inputs[index].Content
	}
	vectors, err := w.embedder.Embed(ctx, EmbedRequest{
		ProfileID: w.profileID,
		Texts:     texts,
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	if err != nil {
		return 0, err
	}
	if err := validateEmbeddingBatch(vectors, len(inputs), w.dimensions); err != nil {
		return 0, err
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
	return stored, nil
}

func validateEmbeddingBatch(vectors [][]float32, count, dimensions int) error {
	if len(vectors) != count {
		return fmt.Errorf("%w: invalid embedding batch result", ErrInvalidMemory)
	}
	for _, vector := range vectors {
		if len(vector) != dimensions {
			return fmt.Errorf("%w: invalid embedding batch result", ErrInvalidMemory)
		}
		for _, value := range vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return fmt.Errorf("%w: invalid embedding batch result", ErrInvalidMemory)
			}
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
