# EvalKit

`evalkit` is a small host-side evaluation harness for Go agent applications. It
defines task, trial, trace, grader, and summary contracts without importing
`goagent` or any provider package.

It is intentionally not an agent runtime. Hosts adapt their own agent, workflow,
or service call into the `Harness` interface, then run repeatable evaluation
suites around that boundary.

## Use

```go
runner := evalkit.Runner{
    Harness: evalkit.HarnessFunc(func(ctx context.Context, task evalkit.Task) (*evalkit.RunResult, error) {
        result, err := agent.RunDetailed(ctx, agentcore.RunRequest{Input: task.Input})
        if err != nil {
            return nil, err
        }
        return &evalkit.RunResult{Output: result.Content}, nil
    }),
    TrialsPerTask: 3,
}

suite := evalkit.Suite{
    Name: "tool-selection-smoke",
    Tasks: []evalkit.Task{{
        ID: "lookup-account",
        Input: "Look up account A-123 and summarize the status.",
        SuccessCriteria: "Uses the account lookup tool and returns a concise status.",
    }},
    Graders: []evalkit.Grader{
        evalkit.GraderFunc{
            GraderName: "contains-status",
            Fn: func(ctx context.Context, req evalkit.GradeRequest) (*evalkit.GradeResult, error) {
                passed := strings.Contains(req.Trial.Output, "status")
                return &evalkit.GradeResult{Passed: passed, Score: 1}, nil
            },
        },
    },
}

report, err := runner.Run(ctx, suite)
```

## Boundary

`evalkit` owns:

- task and suite definitions
- repeated trials for non-deterministic agent behavior
- grader contracts and assertion aggregation
- trace DTOs suitable for host adapters
- suite-level pass/fail summaries

It does not own:

- model calls
- tool execution
- durable storage
- online production monitoring
- hosted dashboards
- model-as-judge implementations

Use `runkit` or `artifactkit` when a host wants durable trace or artifact
storage. Use a host adapter to map `goagent.RunDetailed`, workflow runs, or
service endpoints into `evalkit.Harness`.

## Verify

```bash
go test ./...
```
