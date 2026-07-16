package retrypolicy

import (
	"context"
	"time"

	"github.com/eruca/goagents/ocrs"
)

type Config struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Multiplier     float64
}

type Option func(*Config)

func DefaultConfig() Config {
	return Config{
		MaxAttempts:    3,
		InitialBackoff: 200 * time.Millisecond,
		MaxBackoff:     2 * time.Second,
		Multiplier:     2.0,
	}
}

func New(opts ...Option) Config {
	cfg := DefaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

func WithMaxTry(n int) Option {
	return func(c *Config) {
		c.MaxAttempts = n
	}
}

func WithInitialBackoff(d time.Duration) Option {
	return func(c *Config) {
		c.InitialBackoff = d
	}
}

func WithMaxBackoff(d time.Duration) Option {
	return func(c *Config) {
		c.MaxBackoff = d
	}
}

func WithMultiplier(m float64) Option {
	return func(c *Config) {
		c.Multiplier = m
	}
}

func normalize(cfg Config) Config {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 100 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = cfg.InitialBackoff
	}
	if cfg.MaxBackoff < cfg.InitialBackoff {
		cfg.MaxBackoff = cfg.InitialBackoff
	}
	if cfg.Multiplier < 1.0 {
		cfg.Multiplier = 1.0
	}
	return cfg
}

func NewMiddleware[I, O any](shouldRetry func(error) bool, opts ...Option) ocrs.Middleware[I, O] {
	cfg := normalize(New(opts...))

	return ocrs.MiddlewareFunc[I, O](func(next ocrs.Handler[I, O]) ocrs.Handler[I, O] {
		return ocrs.HandlerFunc[I, O](func(ctx context.Context, data I) (O, error) {
			var zero O
			backoff := cfg.InitialBackoff

			for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
				value, err := next.Handle(ctx, data)
				if err == nil {
					return value, nil
				}
				if attempt == cfg.MaxAttempts || shouldRetry == nil || !shouldRetry(err) {
					return zero, err
				}

				timer := time.NewTimer(backoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return zero, ctx.Err()
				case <-timer.C:
				}

				nextBackoff := time.Duration(float64(backoff) * cfg.Multiplier)
				if nextBackoff <= 0 {
					nextBackoff = cfg.MaxBackoff
				}
				if nextBackoff > cfg.MaxBackoff {
					nextBackoff = cfg.MaxBackoff
				}
				backoff = nextBackoff
			}

			return zero, ctx.Err()
		})
	})
}
