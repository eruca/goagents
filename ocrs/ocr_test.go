package ocrs

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type closerFunc func() error

func (f closerFunc) Close() error {
	return f()
}

func TestHandle(t *testing.T) {
	h := HandlerFunc[[]byte, string](func(ctx context.Context, data []byte) (string, error) {
		return "ok:" + string(data), nil
	})
	o := New[string](h)

	got, err := o.Handle(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok:x" {
		t.Fatalf("unexpected output: %s", got)
	}
}

func TestHandleWithMiddlewares(t *testing.T) {
	var calls []string
	base := HandlerFunc[[]byte, string](func(ctx context.Context, data []byte) (string, error) {
		calls = append(calls, "base")
		return "ok:" + string(data), nil
	})
	mwA := MiddlewareFunc[[]byte, string](func(next Handler[[]byte, string]) Handler[[]byte, string] {
		return HandlerFunc[[]byte, string](func(ctx context.Context, data []byte) (string, error) {
			calls = append(calls, "A.before")
			out, err := next.Handle(ctx, data)
			calls = append(calls, "A.after")
			return out, err
		})
	})
	mwB := MiddlewareFunc[[]byte, string](func(next Handler[[]byte, string]) Handler[[]byte, string] {
		return HandlerFunc[[]byte, string](func(ctx context.Context, data []byte) (string, error) {
			calls = append(calls, "B.before")
			out, err := next.Handle(ctx, data)
			calls = append(calls, "B.after")
			return out, err
		})
	})

	o := New[string](base, mwA, mwB)
	got, err := o.Handle(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok:x" {
		t.Fatalf("unexpected output: %s", got)
	}
	want := []string{"A.before", "B.before", "base", "B.after", "A.after"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unexpected call order: got=%v want=%v", calls, want)
	}
}

func TestCloseUsesConfiguredCloser(t *testing.T) {
	wantErr := errors.New("close failed")
	called := false
	o := NewWithCloser[string](
		HandlerFunc[[]byte, string](func(ctx context.Context, data []byte) (string, error) {
			return "ok", nil
		}),
		closerFunc(func() error {
			called = true
			return wantErr
		}),
	)

	err := o.Close()
	if !errors.Is(err, wantErr) {
		t.Fatalf("unexpected close error: %v", err)
	}
	if !called {
		t.Fatalf("expected closer to be called")
	}
}
