package workflowkit

import (
	"errors"
	"math/rand/v2"
	"time"
)

// RetryPolicy retries a step that fails with a TransientError. Attempt n
// waits a random delay in [0, min(MaxDelay, BaseDelay*2^(n-1))] ("full
// jitter"), so retries back off exponentially without retrying in lockstep.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts per step, counting the
	// first; <= 1 disables retries.
	MaxAttempts int
	BaseDelay   time.Duration
	// MaxDelay caps the backoff; it defaults to 32 x BaseDelay.
	MaxDelay time.Duration
}

// Backoff returns the delay before retry number retry (1 for the first).
func (p RetryPolicy) Backoff(retry int) time.Duration {
	if p.BaseDelay <= 0 || retry <= 0 {
		return 0
	}
	ceiling := p.MaxDelay
	if ceiling < p.BaseDelay {
		ceiling = 32 * p.BaseDelay
	}
	delay := p.BaseDelay
	for i := 1; i < retry && delay < ceiling; i++ {
		delay *= 2
	}
	return rand.N(min(delay, ceiling) + 1)
}

type TransientError struct {
	Err error
}

func (e TransientError) Error() string {
	if e.Err == nil {
		return "transient error"
	}
	return e.Err.Error()
}

func (e TransientError) Unwrap() error {
	return e.Err
}

func IsTransient(err error) bool {
	var transient TransientError
	return errors.As(err, &transient)
}
