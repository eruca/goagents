package chunking

import (
	"bytes"
	"context"
	"fmt"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

func init() {
	api.DisableConfigDir()
}

type PDFCPUSplitter struct{}

func (s PDFCPUSplitter) Split(ctx context.Context, pdf []byte, pagesPerChunk int) ([]Chunk, error) {
	if pagesPerChunk <= 0 {
		return nil, fmt.Errorf("pagesPerChunk must be > 0")
	}
	if len(pdf) == 0 {
		return nil, fmt.Errorf("empty pdf bytes")
	}

	pageCount, err := api.PageCount(bytes.NewReader(pdf), nil)
	if err != nil {
		return nil, fmt.Errorf("count pdf pages failed: %w", err)
	}
	if pageCount == 0 {
		return nil, fmt.Errorf("pdf has no pages")
	}

	chunks := make([]Chunk, 0, (pageCount+pagesPerChunk-1)/pagesPerChunk)
	chunkIdx := 0
	for start := 1; start <= pageCount; start += pagesPerChunk {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		end := min(start+pagesPerChunk-1, pageCount)
		selection := []string{fmt.Sprintf("%d-%d", start, end)}

		var buf bytes.Buffer
		if err := api.Trim(bytes.NewReader(pdf), &buf, selection, nil); err != nil {
			return nil, fmt.Errorf("split pages %d-%d failed: %w", start, end, err)
		}

		chunks = append(chunks, Chunk{
			Index:     chunkIdx,
			StartPage: start,
			EndPage:   end,
			Data:      buf.Bytes(),
		})
		chunkIdx++
	}

	return chunks, nil
}
