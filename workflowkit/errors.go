package workflowkit

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidTransition = errors.New("invalid workflow status transition")
	ErrInvalidStatus     = errors.New("invalid workflow status")
)

type InvalidTransitionError struct {
	From Status
	To   Status
	Op   string
}

func (e InvalidTransitionError) Error() string {
	return fmt.Sprintf("%s: %s cannot transition from %q to %q", ErrInvalidTransition, e.Op, e.From, e.To)
}

func (e InvalidTransitionError) Unwrap() error {
	return ErrInvalidTransition
}

type InvalidStatusError struct {
	Status Status
	Field  string
}

func (e InvalidStatusError) Error() string {
	return fmt.Sprintf("%s: %s has unsupported status %q", ErrInvalidStatus, e.Field, e.Status)
}

func (e InvalidStatusError) Unwrap() error {
	return ErrInvalidStatus
}
