package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrToolInputInvalid    = errors.New("tool input invalid")
	ErrToolSchemaInvalid   = errors.New("tool schema invalid")
	ErrToolExecutionFailed = errors.New("tool execution failed")
	ErrToolTimeout         = errors.New("tool timeout")
)

func classifyToolError(err error) error {
	if err == nil {
		return nil
	}
	if isToolInputInvalid(err) || isToolSchemaInvalid(err) || errors.Is(err, ErrToolExecutionFailed) || errors.Is(err, ErrToolTimeout) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrToolTimeout, err)
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "invalid tool JSON schema"):
		return fmt.Errorf("%w: %w", ErrToolSchemaInvalid, err)
	case strings.Contains(message, "invalid tool input JSON"),
		strings.Contains(message, "tool input failed JSON schema validation"):
		return fmt.Errorf("%w: %w", ErrToolInputInvalid, err)
	default:
		return fmt.Errorf("%w: %w", ErrToolExecutionFailed, err)
	}
}

func isToolInputInvalid(err error) bool {
	return errors.Is(err, ErrToolInputInvalid)
}

func isToolSchemaInvalid(err error) bool {
	return errors.Is(err, ErrToolSchemaInvalid)
}
