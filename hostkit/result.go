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
	exitCode int
	code     string
	err      error
}

// ExitCode returns the process exit code selected by hostkit.
func (r Result) ExitCode() int { return r.exitCode }

// Code returns the normalized lifecycle failure code.
func (r Result) Code() string { return r.code }

// Err returns the classified error. It can be inspected with errors.Is.
func (r Result) Err() error { return r.err }

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
	if err == nil {
		return Result{}
	}

	var classified *failure
	if errors.As(err, &classified) && isKnownCode(classified.code) {
		return Result{
			exitCode: exitCode(classified.code),
			code:     string(classified.code),
			err:      err,
		}
	}

	return Result{
		exitCode: exitCode(CodeInternalError),
		code:     string(CodeInternalError),
		err:      Fail(CodeInternalError, "internal error", err),
	}
}

func isKnownCode(code Code) bool {
	switch code {
	case CodeInternalError,
		CodeConfigFailed,
		CodeInitializationFailed,
		CodeListenFailed,
		CodeServeFailed,
		CodeShutdownTimeout,
		CodeShutdownCleanupTimeout:
		return true
	default:
		return false
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
	if result.exitCode == 0 {
		return nil
	}

	message := ""
	if result.err != nil {
		message = result.err.Error()
	}
	return json.NewEncoder(w).Encode(struct {
		Level   string `json:"level"`
		Event   string `json:"event"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}{
		Level:   "error",
		Event:   "host_exit",
		Code:    result.code,
		Message: message,
	})
}
