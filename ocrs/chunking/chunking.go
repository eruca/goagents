package chunking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/eruca/goagents/ocrs"
)

var ErrInvalidConcurrency = errors.New("invalid chunk concurrency")

type Chunk struct {
	Index     int
	StartPage int
	EndPage   int
	Data      []byte
}

type Splitter interface {
	Split(ctx context.Context, pdf []byte, pagesPerChunk int) ([]Chunk, error)
}

type MergeFunc func(results []ocrs.OCRResult) (ocrs.OCRResult, error)

type CutAwareConfig struct {
	PagesPerChunk    int
	ChunkConcurrency int
	Splitter         Splitter
	Inner            ocrs.Handler[[]byte, ocrs.OCRResult]
	Merge            MergeFunc
}

type CutAwareHandler struct {
	pagesPerChunk    int
	chunkConcurrency int
	splitter         Splitter
	inner            ocrs.Handler[[]byte, ocrs.OCRResult]
	merge            MergeFunc
}

func NewCutAwareHandler(cfg CutAwareConfig) *CutAwareHandler {
	return &CutAwareHandler{
		pagesPerChunk:    cfg.PagesPerChunk,
		chunkConcurrency: cfg.ChunkConcurrency,
		splitter:         cfg.Splitter,
		inner:            cfg.Inner,
		merge:            cfg.Merge,
	}
}

func (h *CutAwareHandler) Handle(ctx context.Context, data []byte) (ocrs.OCRResult, error) {
	var zero ocrs.OCRResult
	if h.inner == nil {
		return zero, fmt.Errorf("inner handler is nil")
	}
	if !isPDFBytes(data) {
		return h.inner.Handle(ctx, data)
	}
	if h.pagesPerChunk <= 0 || h.chunkConcurrency <= 0 || h.splitter == nil || h.merge == nil {
		return zero, fmt.Errorf("invalid cut-aware config")
	}

	chunks, err := h.splitter.Split(ctx, data, h.pagesPerChunk)
	if err != nil {
		slog.Warn("split pdf failed, fallback to direct ocr", "error", err)
		return h.inner.Handle(ctx, data)
	}
	if len(chunks) == 0 {
		return zero, fmt.Errorf("splitter returned no chunks")
	}
	if len(chunks) == 1 {
		return h.inner.Handle(ctx, data)
	}
	return processChunks(ctx, chunks, h.chunkConcurrency, h.inner, h.merge)
}

func ProcessPDF(
	ctx context.Context,
	pdf []byte,
	pagesPerChunk int,
	concurrency int,
	splitter Splitter,
	handler ocrs.Handler[[]byte, ocrs.OCRResult],
	merge MergeFunc,
) (ocrs.OCRResult, error) {
	var zero ocrs.OCRResult
	if pagesPerChunk <= 0 {
		return zero, fmt.Errorf("pagesPerChunk must be > 0")
	}
	if concurrency <= 0 {
		return zero, ErrInvalidConcurrency
	}
	if splitter == nil {
		return zero, fmt.Errorf("splitter is nil")
	}
	if handler == nil {
		return zero, fmt.Errorf("handler is nil")
	}
	if merge == nil {
		return zero, fmt.Errorf("merge is nil")
	}

	chunks, err := splitter.Split(ctx, pdf, pagesPerChunk)
	if err != nil {
		return zero, err
	}
	if len(chunks) == 0 {
		return zero, fmt.Errorf("splitter returned no chunks")
	}
	if len(chunks) == 1 {
		return handler.Handle(ctx, pdf)
	}

	return processChunks(ctx, chunks, concurrency, handler, merge)
}

func processChunks(
	ctx context.Context,
	chunks []Chunk,
	concurrency int,
	handler ocrs.Handler[[]byte, ocrs.OCRResult],
	merge MergeFunc,
) (ocrs.OCRResult, error) {
	var zero ocrs.OCRResult
	sort.Slice(chunks, func(i, j int) bool {
		return chunks[i].Index < chunks[j].Index
	})

	results := make([]ocrs.OCRResult, len(chunks))
	sem := make(chan struct{}, concurrency)
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex

TOP:
	for i := range chunks {
		ch := chunks[i]

		select {
		case <-ctx2.Done():
			break TOP
		default:
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(pos int, c Chunk) {
			defer wg.Done()
			defer func() { <-sem }()

			res, callErr := handler.Handle(ctx2, c.Data)
			if callErr != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("chunk %d pages %d-%d: %w", c.Index, c.StartPage, c.EndPage, callErr)
					cancel()
				}
				errMu.Unlock()
				return
			}
			results[pos] = res
		}(i, ch)
	}

	wg.Wait()
	if firstErr != nil {
		return zero, firstErr
	}
	if err := ctx2.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return zero, err
	}

	return merge(results)
}

func isPDFBytes(data []byte) bool {
	return len(data) >= 5 && string(data[:5]) == "%PDF-"
}
