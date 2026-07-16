package chunking

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/ocrs"
)

type fakeSplitter struct {
	chunks []Chunk
	err    error
}

func (f fakeSplitter) Split(ctx context.Context, pdf []byte, pagesPerChunk int) ([]Chunk, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.chunks, nil
}

type counterSplitter struct {
	chunks []Chunk
	err    error
	calls  int
}

func (c *counterSplitter) Split(ctx context.Context, pdf []byte, pagesPerChunk int) ([]Chunk, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.chunks, nil
}

func TestProcessPDFKeepsChunkOrderForMerge(t *testing.T) {
	t.Parallel()

	splitter := fakeSplitter{
		chunks: []Chunk{
			{Index: 0, StartPage: 1, EndPage: 20, Data: []byte("c0")},
			{Index: 1, StartPage: 21, EndPage: 40, Data: []byte("c1")},
			{Index: 2, StartPage: 41, EndPage: 60, Data: []byte("c2")},
		},
	}

	handler := ocrs.HandlerFunc[[]byte, ocrs.OCRResult](func(ctx context.Context, data []byte) (ocrs.OCRResult, error) {
		switch string(data) {
		case "c0":
			time.Sleep(30 * time.Millisecond)
		case "c1":
			time.Sleep(5 * time.Millisecond)
		}
		return ocrs.OCRResult{Provider: "paddle", Raw: []byte(string(data))}, nil
	})

	var got []string
	merge := func(results []ocrs.OCRResult) (ocrs.OCRResult, error) {
		for _, r := range results {
			got = append(got, string(r.Raw))
		}
		return ocrs.OCRResult{Provider: "paddle", Raw: []byte("merged")}, nil
	}

	_, err := ProcessPDF(context.Background(), []byte("pdf"), 20, 3, splitter, handler, merge)
	if err != nil {
		t.Fatalf("ProcessPDF() error = %v", err)
	}

	want := []string{"c0", "c1", "c2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected merge order at %d: got=%q want=%q", i, got[i], want[i])
		}
	}
}

func TestProcessPDFReturnsChunkError(t *testing.T) {
	t.Parallel()

	splitter := fakeSplitter{
		chunks: []Chunk{
			{Index: 0, StartPage: 1, EndPage: 20, Data: []byte("ok")},
			{Index: 1, StartPage: 21, EndPage: 40, Data: []byte("bad")},
		},
	}

	handler := ocrs.HandlerFunc[[]byte, ocrs.OCRResult](func(ctx context.Context, data []byte) (ocrs.OCRResult, error) {
		if string(data) == "bad" {
			return ocrs.OCRResult{}, errors.New("boom")
		}
		return ocrs.OCRResult{Provider: "paddle", Raw: []byte("ok")}, nil
	})
	merge := func(results []ocrs.OCRResult) (ocrs.OCRResult, error) {
		return ocrs.OCRResult{}, fmt.Errorf("should not be called")
	}

	_, err := ProcessPDF(context.Background(), []byte("pdf"), 20, 2, splitter, handler, merge)
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestProcessPDFInvalidConcurrency(t *testing.T) {
	t.Parallel()

	handler := ocrs.HandlerFunc[[]byte, ocrs.OCRResult](func(ctx context.Context, data []byte) (ocrs.OCRResult, error) {
		return ocrs.OCRResult{Provider: "paddle", Raw: data}, nil
	})
	merge := func(results []ocrs.OCRResult) (ocrs.OCRResult, error) {
		return ocrs.OCRResult{Provider: "paddle", Raw: []byte("ok")}, nil
	}

	_, err := ProcessPDF(context.Background(), []byte("pdf"), 20, 0, fakeSplitter{}, handler, merge)
	if err == nil {
		t.Fatalf("expected invalid concurrency error")
	}
}

func TestProcessPDFCallsHandlerForAllChunks(t *testing.T) {
	t.Parallel()

	splitter := fakeSplitter{
		chunks: []Chunk{
			{Index: 0, StartPage: 1, EndPage: 1, Data: []byte("a")},
			{Index: 1, StartPage: 2, EndPage: 2, Data: []byte("b")},
		},
	}

	var mu sync.Mutex
	calls := 0
	handler := ocrs.HandlerFunc[[]byte, ocrs.OCRResult](func(ctx context.Context, data []byte) (ocrs.OCRResult, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return ocrs.OCRResult{Provider: "paddle", Raw: data}, nil
	})
	merge := func(results []ocrs.OCRResult) (ocrs.OCRResult, error) {
		return ocrs.OCRResult{Provider: "paddle", Raw: []byte("ok")}, nil
	}

	_, err := ProcessPDF(context.Background(), []byte("pdf"), 20, 2, splitter, handler, merge)
	if err != nil {
		t.Fatalf("ProcessPDF() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("unexpected handler calls: got=%d want=2", calls)
	}
}

func TestCutAwareHandlerBypassNonPDF(t *testing.T) {
	t.Parallel()

	splitter := &counterSplitter{
		chunks: []Chunk{{Index: 0, StartPage: 1, EndPage: 1, Data: []byte("chunk")}},
	}
	innerCalls := 0
	handler := ocrs.HandlerFunc[[]byte, ocrs.OCRResult](func(ctx context.Context, data []byte) (ocrs.OCRResult, error) {
		innerCalls++
		return ocrs.OCRResult{Provider: "paddle", Raw: data}, nil
	})
	merge := func(results []ocrs.OCRResult) (ocrs.OCRResult, error) {
		return ocrs.OCRResult{Provider: "paddle", Raw: []byte("merged")}, nil
	}

	h := NewCutAwareHandler(CutAwareConfig{
		PagesPerChunk:    20,
		ChunkConcurrency: 2,
		Splitter:         splitter,
		Inner:            handler,
		Merge:            merge,
	})
	_, err := h.Handle(context.Background(), []byte("not-a-pdf"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if splitter.calls != 0 {
		t.Fatalf("splitter should not be called for non-pdf")
	}
	if innerCalls != 1 {
		t.Fatalf("inner should be called once, got %d", innerCalls)
	}
}

func TestCutAwareHandlerFallbackWhenSplitFails(t *testing.T) {
	t.Parallel()

	splitter := &counterSplitter{
		err: errors.New("count pdf pages failed: invalid date"),
	}
	innerCalls := 0
	handler := ocrs.HandlerFunc[[]byte, ocrs.OCRResult](func(ctx context.Context, data []byte) (ocrs.OCRResult, error) {
		innerCalls++
		return ocrs.OCRResult{Provider: "paddle", Raw: []byte("direct-ocr")}, nil
	})
	merge := func(results []ocrs.OCRResult) (ocrs.OCRResult, error) {
		return ocrs.OCRResult{}, fmt.Errorf("merge should not be called")
	}

	h := NewCutAwareHandler(CutAwareConfig{
		PagesPerChunk:    20,
		ChunkConcurrency: 2,
		Splitter:         splitter,
		Inner:            handler,
		Merge:            merge,
	})
	out, err := h.Handle(context.Background(), []byte("%PDF-test"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if splitter.calls != 1 {
		t.Fatalf("splitter should be called once, got %d", splitter.calls)
	}
	if innerCalls != 1 {
		t.Fatalf("inner should be called once in fallback, got %d", innerCalls)
	}
	if string(out.Raw) != "direct-ocr" {
		t.Fatalf("unexpected fallback result: %s", out.Raw)
	}
}
