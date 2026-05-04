package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

type Handler func(ctx context.Context, input json.RawMessage, env Env) (*Result, error)

type Middleware func(Handler) Handler

type MiddlewareProvider interface {
	Middleware() []Middleware
}

func Chain(middlewares ...Middleware) Middleware {
	return func(next Handler) Handler {
		for i := len(middlewares) - 1; i >= 0; i-- {
			next = middlewares[i](next)
		}
		return next
	}
}

func SchemaValidator(schema Schema) Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			if err := schema.ValidateInput(input); err != nil {
				return nil, classifySchemaError(err)
			}
			return next(ctx, input, env)
		}
	}
}

func classifySchemaError(err error) error {
	if err == nil {
		return nil
	}
	classified := classifyToolError(err)
	if classified != nil && (isToolSchemaInvalid(classified) || isToolInputInvalid(classified)) {
		return classified
	}
	return fmt.Errorf("%w: %w", ErrToolInputInvalid, err)
}

func OutputMask(mask func(*Result) *Result) Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			result, err := next(ctx, input, env)
			if err != nil || result == nil || mask == nil {
				return result, err
			}
			return mask(result), nil
		}
	}
}
