package ocrs

import (
	"context"
	"io"
)

type OCR[T any] struct {
	handler Handler[[]byte, T]
	closer  io.Closer
}

func New[T any](handler Handler[[]byte, T], middlewares ...Middleware[[]byte, T]) *OCR[T] {
	return &OCR[T]{
		handler: Chain[[]byte, T](middlewares...).Wrap(handler),
	}
}

func NewWithCloser[T any](handler Handler[[]byte, T], closer io.Closer, middlewares ...Middleware[[]byte, T]) *OCR[T] {
	return &OCR[T]{
		handler: Chain[[]byte, T](middlewares...).Wrap(handler),
		closer:  closer,
	}
}

func (o *OCR[T]) Handle(ctx context.Context, data []byte) (T, error) {
	return o.handler.Handle(ctx, data)
}

func (o *OCR[T]) Close() error {
	if o.closer == nil {
		return nil
	}
	return o.closer.Close()
}
