package tools

import (
	"errors"
	"fmt"

	"github.com/eruca/goagents/goagent/ports"
)

var (
	ErrToolInputInvalid    = ports.ErrToolInputInvalid
	ErrToolSchemaInvalid   = ports.ErrToolSchemaInvalid
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
	return fmt.Errorf("%w: %w", ErrToolExecutionFailed, err)
}

func isToolInputInvalid(err error) bool {
	return errors.Is(err, ErrToolInputInvalid)
}

func isToolSchemaInvalid(err error) bool {
	return errors.Is(err, ErrToolSchemaInvalid)
}
