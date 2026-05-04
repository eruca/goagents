package ocrs

import (
	"context"
	"encoding/json"
)

type Provider[I, O any] interface {
	Family() string
	Name() string
	Handler[I, O]
}

type ProviderConfig[I, O any] struct {
	Provider Provider[I, O]
	Workers  int
}

type OCRResult struct {
	Provider string          `json:"provider"`
	Raw      json.RawMessage `json:"raw"`
}

type ProcessedChunk struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

type UnprocessedChunk struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Skipped bool   `json:"skipped"`
}

type Scheduler[I, O any] interface {
	Acquire(ctx context.Context) (Lease[I, O], error)
}

type Lease[I, O any] interface {
	Endpoint() ProviderConfig[I, O]
	Done(success bool)
}
