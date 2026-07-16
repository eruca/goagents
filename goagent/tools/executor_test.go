package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/policy"
)

func TestExecutorPassesRawJSONAndSeparatesOutputs(t *testing.T) {
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{Name: "echo"},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			if string(input) != `{"value":"secret"}` {
				t.Fatalf("input = %s", input)
			}
			return &Result{ForLLM: "redacted", ForUser: "secret"}, nil
		},
	})

	executor := NewExecutor(registry)
	results, err := executor.Execute(context.Background(), []Call{
		{Name: "echo", Input: json.RawMessage(`{"value":"secret"}`)},
	}, Env{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d", len(results))
	}
	if results[0].Result.ForLLM != "redacted" || results[0].Result.ForUser != "secret" {
		t.Fatalf("result = %#v", results[0].Result)
	}
}

func TestExecutorPreservesResultReferenceAndMetadata(t *testing.T) {
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{Name: "query"},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			return &Result{
				ForLLM:   "query rows: 10",
				ForUser:  "full query output",
				Ref:      "artifact:query-123",
				Metadata: map[string]any{"row_count": 10},
			}, nil
		},
	})

	results, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "query"}}, Env{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if results[0].Result.Ref != "artifact:query-123" {
		t.Fatalf("Ref = %q", results[0].Result.Ref)
	}
	if results[0].Result.Metadata["row_count"] != 10 {
		t.Fatalf("Metadata = %#v", results[0].Result.Metadata)
	}
}

func TestParallelExecutorPreservesCallOrder(t *testing.T) {
	secondStarted := make(chan struct{})
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{Name: "first", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			select {
			case <-secondStarted:
				return &Result{ForLLM: "first"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})
	registry.Register(testTool{
		spec: Spec{Name: "second", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			close(secondStarted)
			return &Result{ForLLM: "second"}, nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	results, err := NewExecutor(registry).Execute(ctx, []Call{{Name: "first"}, {Name: "second"}}, Env{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := []string{results[0].Result.ForLLM, results[1].Result.ForLLM}; got[0] != "first" || got[1] != "second" {
		t.Fatalf("results = %v", got)
	}
}

func TestExecutorRunsWriteToolsSequentiallyByDefault(t *testing.T) {
	events := make([]string, 0, 4)
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{Name: "first_write", Permission: policy.PermissionWrite},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			events = append(events, "first_start")
			events = append(events, "first_end")
			return &Result{ForLLM: "first"}, nil
		},
	})
	registry.Register(testTool{
		spec: Spec{Name: "second_write", Permission: policy.PermissionWrite},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			events = append(events, "second_start")
			events = append(events, "second_end")
			return &Result{ForLLM: "second"}, nil
		},
	})

	_, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "first_write"}, {Name: "second_write"}}, Env{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	want := []string{"first_start", "first_end", "second_start", "second_end"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v", events)
		}
	}
}

func TestExecutorRunsExplicitParallelWriteToolsInParallel(t *testing.T) {
	secondStarted := make(chan struct{})
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{Name: "first_write", Permission: policy.PermissionWrite, ExecutionMode: ExecutionModeParallel},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			select {
			case <-secondStarted:
				return &Result{ForLLM: "first"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})
	registry.Register(testTool{
		spec: Spec{Name: "second_write", Permission: policy.PermissionWrite, ExecutionMode: ExecutionModeParallel},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			close(secondStarted)
			return &Result{ForLLM: "second"}, nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	results, err := NewExecutor(registry).Execute(ctx, []Call{{Name: "first_write"}, {Name: "second_write"}}, Env{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := []string{results[0].Result.ForLLM, results[1].Result.ForLLM}; got[0] != "first" || got[1] != "second" {
		t.Fatalf("results = %v", got)
	}
}

func TestExecutorAppliesToolMiddleware(t *testing.T) {
	registry := NewRegistry()
	registry.Register(middlewareTool{
		testTool: testTool{
			spec: Spec{Name: "masked"},
			run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
				return &Result{ForLLM: "secret", ForUser: "secret"}, nil
			},
		},
		middleware: []Middleware{
			OutputMask(func(result *Result) *Result {
				result.ForLLM = "redacted"
				return result
			}),
		},
	})

	results, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "masked"}}, Env{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if results[0].Result.ForLLM != "redacted" {
		t.Fatalf("ForLLM = %q", results[0].Result.ForLLM)
	}
	if results[0].Result.ForUser != "secret" {
		t.Fatalf("ForUser = %q", results[0].Result.ForUser)
	}
}

func TestExecutorStopsBeforeToolBodyWhenMiddlewareFails(t *testing.T) {
	called := false
	registry := NewRegistry()
	registry.Register(middlewareTool{
		testTool: testTool{
			spec: Spec{Name: "validated"},
			run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
				called = true
				return &Result{ForLLM: "should not run"}, nil
			},
		},
		middleware: []Middleware{
			SchemaValidator(Schema{
				Validate: func(input json.RawMessage) error {
					return fmt.Errorf("invalid input")
				},
			}),
		},
	})

	_, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "validated"}}, Env{})
	if err == nil || !errors.Is(err, ErrToolInputInvalid) {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("tool body was called")
	}
}

func TestExecutorValidatesToolSpecSchemaBeforeToolBody(t *testing.T) {
	called := false
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{
			Name: "schema",
			Schema: Schema{
				Validate: func(input json.RawMessage) error {
					return fmt.Errorf("schema failed")
				},
			},
		},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			called = true
			return &Result{ForLLM: "should not run"}, nil
		},
	})

	_, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "schema"}}, Env{})
	if err == nil || !errors.Is(err, ErrToolInputInvalid) {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("tool body was called")
	}
}

func TestExecutorValidatesJSONSchemaBeforeToolBody(t *testing.T) {
	called := false
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{
			Name: "json_schema",
			Schema: Schema{
				JSONSchema: json.RawMessage(`{
					"type":"object",
					"properties":{"account":{"type":"string"}},
					"required":["account"],
					"additionalProperties":false
				}`),
			},
		},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			called = true
			return &Result{ForLLM: "should not run"}, nil
		},
	})

	_, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "json_schema", Input: json.RawMessage(`{"other":"demo"}`)}}, Env{})
	if err == nil {
		t.Fatal("Execute returned nil error")
	}
	if !errors.Is(err, ErrToolInputInvalid) {
		t.Fatalf("err = %v, want ErrToolInputInvalid", err)
	}
	if called {
		t.Fatal("tool body was called")
	}
}

func TestExecutorRunsCustomValidateAfterJSONSchema(t *testing.T) {
	events := make([]string, 0, 2)
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{
			Name: "json_schema_then_validate",
			Schema: Schema{
				JSONSchema: json.RawMessage(`{
					"type":"object",
					"properties":{"account":{"type":"string"}},
					"required":["account"],
					"additionalProperties":false
				}`),
				Validate: func(input json.RawMessage) error {
					events = append(events, "validate")
					return nil
				},
			},
		},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			events = append(events, "tool")
			return &Result{ForLLM: "ok"}, nil
		},
	})

	_, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "json_schema_then_validate", Input: json.RawMessage(`{"account":"demo"}`)}}, Env{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	want := []string{"validate", "tool"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v", events)
		}
	}
}

func TestExecutorRejectsInvalidJSONSchemaBeforeToolBody(t *testing.T) {
	called := false
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{
			Name: "invalid_json_schema",
			Schema: Schema{
				JSONSchema: json.RawMessage(`{"type":42}`),
			},
		},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			called = true
			return &Result{ForLLM: "should not run"}, nil
		},
	})

	_, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "invalid_json_schema", Input: json.RawMessage(`{}`)}}, Env{})
	if err == nil {
		t.Fatal("Execute returned nil error")
	}
	if !errors.Is(err, ErrToolSchemaInvalid) {
		t.Fatalf("err = %v, want ErrToolSchemaInvalid", err)
	}
	if called {
		t.Fatal("tool body was called")
	}
}

func TestExecutorClassifiesToolExecutionError(t *testing.T) {
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{Name: "fails"},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			return nil, fmt.Errorf("boom")
		},
	})

	_, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "fails"}}, Env{})
	if err == nil {
		t.Fatal("Execute returned nil error")
	}
	if !errors.Is(err, ErrToolExecutionFailed) {
		t.Fatalf("err = %v, want ErrToolExecutionFailed", err)
	}
}

func TestExecutorRunsSpecSchemaBeforeCustomMiddleware(t *testing.T) {
	events := make([]string, 0, 3)
	registry := NewRegistry()
	registry.Register(middlewareTool{
		testTool: testTool{
			spec: Spec{
				Name: "ordered",
				Schema: Schema{
					Validate: func(input json.RawMessage) error {
						events = append(events, "schema")
						return nil
					},
				},
			},
			run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
				events = append(events, "tool")
				return &Result{ForLLM: "ok"}, nil
			},
		},
		middleware: []Middleware{
			func(next Handler) Handler {
				return func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
					events = append(events, "custom")
					return next(ctx, input, env)
				}
			},
		},
	})

	_, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "ordered"}}, Env{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	want := []string{"schema", "custom", "tool"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v", events)
		}
	}
}

func TestExecutorEnforcesToolTimeout(t *testing.T) {
	registry := NewRegistry()
	registry.Register(testTool{
		spec: Spec{Name: "slow", Timeout: 10 * time.Millisecond},
		run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
			select {
			case <-time.After(250 * time.Millisecond):
				return &Result{ForLLM: "too late"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})

	_, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "slow"}}, Env{})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline exceeded", err)
	}
	if !errors.Is(err, ErrToolTimeout) {
		t.Fatalf("err = %v, want ErrToolTimeout", err)
	}
}

func TestExecutorMiddlewareUsesToolTimeoutContext(t *testing.T) {
	registry := NewRegistry()
	registry.Register(middlewareTool{
		testTool: testTool{
			spec: Spec{Name: "guarded", Timeout: 10 * time.Millisecond},
			run: func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
				return &Result{ForLLM: "should not run"}, nil
			},
		},
		middleware: []Middleware{
			func(next Handler) Handler {
				return func(ctx context.Context, input json.RawMessage, env Env) (*Result, error) {
					select {
					case <-time.After(250 * time.Millisecond):
						return next(ctx, input, env)
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			},
		},
	})

	_, err := NewExecutor(registry).Execute(context.Background(), []Call{{Name: "guarded"}}, Env{})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline exceeded", err)
	}
	if !errors.Is(err, ErrToolTimeout) {
		t.Fatalf("err = %v, want ErrToolTimeout", err)
	}
}

type middlewareTool struct {
	testTool
	middleware []Middleware
}

func (t middlewareTool) Middleware() []Middleware {
	return t.middleware
}
