package retrypolicy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eruca/goagents/ocrs"
)

func TestRetryThenSuccess(t *testing.T) {
	t.Parallel()

	attempts := 0
	handler := ocrs.HandlerFunc[string, string](func(ctx context.Context, data string) (string, error) {
		attempts++
		if attempts < 3 {
			return "", errors.New("temporary")
		}
		return "ok:" + data, nil
	})
	mw := NewMiddleware[string, string](func(err error) bool { return true },
		WithMaxTry(3),
		WithInitialBackoff(time.Millisecond),
		WithMaxBackoff(time.Millisecond),
		WithMultiplier(1),
	)

	got, err := mw.Wrap(handler).Handle(context.Background(), "x")
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got != "ok:x" {
		t.Fatalf("unexpected output: %s", got)
	}
	if attempts != 3 {
		t.Fatalf("unexpected attempts: got=%d want=3", attempts)
	}
}

func TestDoesNotRetryNonRetryableError(t *testing.T) {
	t.Parallel()

	attempts := 0
	wantErr := errors.New("permanent")
	handler := ocrs.HandlerFunc[string, string](func(ctx context.Context, data string) (string, error) {
		attempts++
		return "", wantErr
	})
	mw := NewMiddleware[string, string](func(err error) bool { return false },
		WithMaxTry(3),
		WithInitialBackoff(time.Millisecond),
	)

	_, err := mw.Wrap(handler).Handle(context.Background(), "x")
	if !errors.Is(err, wantErr) {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("unexpected attempts: got=%d want=1", attempts)
	}
}

func TestContextCancelStopsBackoff(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	handler := ocrs.HandlerFunc[string, string](func(ctx context.Context, data string) (string, error) {
		attempts++
		cancel()
		return "", errors.New("temporary")
	})
	mw := NewMiddleware[string, string](func(err error) bool { return true },
		WithMaxTry(3),
		WithInitialBackoff(time.Hour),
	)

	_, err := mw.Wrap(handler).Handle(ctx, "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("unexpected attempts: got=%d want=1", attempts)
	}
}
