package tools

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/eruca/goagents/goagent/ports"
)

type Executor struct {
	registry interface {
		MustGet(name string) (Tool, error)
	}
}

func NewExecutor(registry interface {
	MustGet(name string) (Tool, error)
}) *Executor {
	return &Executor{registry: registry}
}

func (e *Executor) Execute(ctx context.Context, calls []Call, env Env) ([]Execution, error) {
	results := make([]Execution, len(calls))
	errs := make([]error, len(calls))
	resolved := make([]Tool, len(calls))
	for i, call := range calls {
		tool, err := e.registry.MustGet(call.Name)
		if err != nil {
			return nil, err
		}
		resolved[i] = tool
	}
	for start := 0; start < len(calls); {
		mode := executionMode(resolved[start])
		if mode == ports.ExecutionModeParallel {
			end := start + 1
			for end < len(calls) && executionMode(resolved[end]) == ports.ExecutionModeParallel {
				end++
			}
			executeParallel(ctx, calls[start:end], resolved[start:end], env, results[start:end], errs[start:end], start)
			start = end
			continue
		}
		executeOne(ctx, calls[start], resolved[start], env, results, errs, start)
		start++
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func executeParallel(ctx context.Context, calls []Call, resolved []Tool, env Env, results []Execution, errs []error, offset int) {
	var wg sync.WaitGroup
	for i, call := range calls {
		tool := resolved[i]
		wg.Add(1)
		go func(i int, call Call, tool Tool) {
			defer wg.Done()
			result, err := runTool(ctx, tool, call, env)
			if err != nil {
				errs[i] = classifyToolError(err)
				return
			}
			results[i] = Execution{Index: offset + i, Call: call, Result: result}
		}(i, call, tool)
	}
	wg.Wait()
}

func executeOne(ctx context.Context, call Call, tool Tool, env Env, results []Execution, errs []error, index int) {
	result, err := runTool(ctx, tool, call, env)
	if err != nil {
		errs[index] = classifyToolError(err)
		return
	}
	results[index] = Execution{Index: index, Call: call, Result: result}
}

func runTool(ctx context.Context, tool Tool, call Call, env Env) (*Result, error) {
	toolCtx, cancel := toolContext(ctx, tool)
	defer cancel()
	return toolHandler(tool)(toolCtx, call.Input, env)
}

func executionMode(tool Tool) ports.ExecutionMode {
	spec := tool.Spec()
	switch spec.ExecutionMode {
	case ports.ExecutionModeParallel, ports.ExecutionModeSequential, ports.ExecutionModeExclusive:
		return spec.ExecutionMode
	}
	if spec.Permission == ports.PermissionRead {
		return ports.ExecutionModeParallel
	}
	return ports.ExecutionModeSequential
}

func toolHandler(tool Tool) Handler {
	handler := func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
		return tool.Execute(ctx, input, env)
	}
	middlewares := []Middleware{SchemaValidator(tool.Spec().Schema)}
	if provider, ok := tool.(MiddlewareProvider); ok {
		middlewares = append(middlewares, provider.Middleware()...)
	}
	return Chain(middlewares...)(handler)
}

func toolContext(ctx context.Context, tool Tool) (context.Context, context.CancelFunc) {
	timeout := tool.Spec().Timeout
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
