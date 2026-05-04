package main

import (
	"context"
	"fmt"

	"github.com/eruca/ocrs"
	"github.com/eruca/ocrs/chunking"
)

type demoSplitter struct{}

func (demoSplitter) Split(ctx context.Context, pdf []byte, pagesPerChunk int) ([]chunking.Chunk, error) {
	return []chunking.Chunk{
		{Index: 0, StartPage: 1, EndPage: 1, Data: []byte("page-1")},
		{Index: 1, StartPage: 2, EndPage: 2, Data: []byte("page-2")},
	}, nil
}

func main() {
	inner := ocrs.HandlerFunc[[]byte, ocrs.OCRResult](func(ctx context.Context, data []byte) (ocrs.OCRResult, error) {
		return ocrs.OCRResult{Provider: "demo", Raw: data}, nil
	})
	merge := func(results []ocrs.OCRResult) (ocrs.OCRResult, error) {
		var out []byte
		for _, result := range results {
			out = append(out, result.Raw...)
			out = append(out, '\n')
		}
		return ocrs.OCRResult{Provider: "demo", Raw: out}, nil
	}

	handler := chunking.NewCutAwareHandler(chunking.CutAwareConfig{
		PagesPerChunk:    1,
		ChunkConcurrency: 2,
		Splitter:         demoSplitter{},
		Inner:            inner,
		Merge:            merge,
	})

	result, err := handler.Handle(context.Background(), []byte("%PDF-demo"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("provider=%s raw=%q\n", result.Provider, string(result.Raw))
}
