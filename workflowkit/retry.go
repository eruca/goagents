package workflowkit

import (
	"errors"
	"time"
)

type RetryPolicy struct {
	MaxAttempts int
	Delay       time.Duration
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
