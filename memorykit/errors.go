package memorykit

import "errors"

var (
	ErrInvalidScope        = errors.New("invalid memory scope")
	ErrInvalidMemory       = errors.New("invalid memory")
	ErrNotFound            = errors.New("memory not found")
	ErrConflict            = errors.New("memory version conflict")
	ErrNoExtractionJob     = errors.New("no claimable extraction job")
	ErrInvalidRecallResult = errors.New("invalid memory recall result")
	ErrRecallDependency    = errors.New("memory recall dependency failure")
)

type BackendError struct {
	Op          string
	Recoverable bool
	Err         error
}

func (e *BackendError) Error() string { return "memory backend " + e.Op + ": " + e.Err.Error() }
func (e *BackendError) Unwrap() error { return e.Err }

func IsRecoverable(err error) bool {
	var backend *BackendError
	return errors.As(err, &backend) && backend.Recoverable
}
