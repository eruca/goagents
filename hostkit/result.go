package hostkit

import (
	"encoding/json"
	"errors"
	"io"
)

// Code identifies the host lifecycle phase that failed.
type Code string

const (
	CodeInternalError          Code = "internal_error"
	CodeConfigFailed           Code = "config_failed"
	CodeInitializationFailed   Code = "initialization_failed"
	CodeListenFailed           Code = "listen_failed"
	CodeServeFailed            Code = "serve_failed"
	CodeShutdownTimeout        Code = "shutdown_timeout"
	CodeShutdownCleanupTimeout Code = "shutdown_cleanup_timeout"
)

// Result is the normalized host process outcome.
type Result struct {
	ExitCode int
	Code     string
	Err      error
}

// failure keeps the host-safe message separate from the original cause.
type failure struct {
	code        Code
	safeMessage string
	cause       error
}

func (f *failure) Error() string { return f.safeMessage }

func (f *failure) Unwrap() error { return f.cause }

// Fail classifies a host failure without exposing its original cause in Error.
func Fail(code Code, safeMessage string, cause error) error {
	return &failure{code: code, safeMessage: safeMessage, cause: cause}
}

func resultFromError(err error) Result {
	code := CodeInternalError
	var classified *failure
	if errors.As(err, &classified) {
		code = classified.code
	}

	return Result{
		ExitCode: exitCode(code),
		Code:     string(code),
		Err:      err,
	}
}

func exitCode(code Code) int {
	switch code {
	case CodeConfigFailed, CodeInitializationFailed:
		return 2
	case CodeListenFailed:
		return 3
	case CodeServeFailed:
		return 4
	case CodeShutdownTimeout, CodeShutdownCleanupTimeout:
		return 5
	case CodeInternalError:
		return 1
	default:
		return 1
	}
}

// WriteError emits one JSON error line for a non-successful host result.
func WriteError(w io.Writer, result Result) error {
	if result.ExitCode == 0 {
		return nil
	}

	message := ""
	if result.Err != nil {
		message = result.Err.Error()
	}
	return json.NewEncoder(w).Encode(struct {
		Level   string `json:"level"`
		Event   string `json:"event"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}{
		Level:   "error",
		Event:   "host_exit",
		Code:    result.Code,
		Message: message,
	})
}
