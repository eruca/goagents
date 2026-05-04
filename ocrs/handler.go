package ocrs

import "context"

type Handler[I, O any] interface {
	Handle(ctx context.Context, data I) (O, error)
}

type HandlerFunc[I, O any] func(ctx context.Context, data I) (O, error)

func (f HandlerFunc[I, O]) Handle(ctx context.Context, data I) (O, error) {
	return f(ctx, data)
}

type Middleware[I, O any] interface {
	Wrap(next Handler[I, O]) Handler[I, O]
}

type MiddlewareFunc[I, O any] func(next Handler[I, O]) Handler[I, O]

func (f MiddlewareFunc[I, O]) Wrap(next Handler[I, O]) Handler[I, O] {
	return f(next)
}

func Chain[I, O any](middlewares ...Middleware[I, O]) Middleware[I, O] {
	return MiddlewareFunc[I, O](func(final Handler[I, O]) Handler[I, O] {
		h := final
		for i := len(middlewares) - 1; i >= 0; i-- {
			mw := middlewares[i]
			if mw == nil {
				continue
			}
			h = mw.Wrap(h)
		}
		return h
	})
}
