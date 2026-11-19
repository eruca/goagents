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
        return &evalkit.RunResult{
            Output: result.Content,
            Outcome: evalkit.Outcome{
                Status:    "succeeded",
                OutputRef: "artifact:run-output",
            },
        }, nil
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

A trial passes when its harness run succeeds and every grade passes. A grade
with assertions passes exactly when all of them pass; its own `Passed` and
`Score` are then not consulted, so a grader that must fail regardless adds a
failing assertion. A suite without graders passes every trial whose harness
run succeeds, which suits a smoke check but measures nothing else.

It does not own:

- model calls
- tool execution
- durable storage
- online production monitoring
- hosted dashboards
- model-as-judge implementations

## Outcome and sanitized traces

`RunResult.Outcome` separates the evaluated system's domain result from a
Harness execution error. Use `Status`, `OutputRef`, and a stable `ErrorCode` for
graders; keep raw provider errors in neither Outcome nor Trace. Outcome metadata
is defensively copied into each Trial.

Host adapters decide how persisted runtime data becomes `TraceStep` values.
They should use fixed metadata and label allowlists, store only host-owned
artifact references, and exclude prompts, tool inputs/outputs, credentials,
absolute trust paths, and arbitrary event messages. The host-api example maps
its existing SQLite workflow and run stores this way without adding a production
HTTP endpoint.

Use `runkit` or `artifactkit` when a host wants durable trace or artifact
storage. Use a host adapter to map `goagent.RunDetailed`, workflow runs, or
service endpoints into `evalkit.Harness`.

## Trajectory graders

Trace adapters should use the shared step types `StepWorkflow`,
`StepToolCall`, `StepModelCall`, `StepApproval`, and `StepAgentEvent`, and the
statuses `succeeded`, `failed`, `rejected`, `waiting_approval`, and `running`.
The trajectory graders read only those fields, the trial's `Outcome.Status`,
and `Usage`; they never read `Output` or step messages, so they work on
content-free production traces.

| Grader | Passes when |
|---|---|
| `RequireTools(names...)` | every named tool was called |
| `ForbidTools(names...)` | none of the named tools was called |
| `ToolOrder(names...)` | the tool calls contain the names as an ordered subsequence |
| `NoFailedSteps(types...)` | no step of those types (all types when none given) failed or was rejected |
| `Budget{...}.Grader()` | tool calls, model calls, tokens, and trace duration stay within the non-zero limits |
| `OutcomeStatus(statuses...)` | `Outcome.Status` is one of the statuses |

Each grader reports one assertion per check, and its score is the fraction of
assertions that passed. A grader configured without names fails every trial
instead of passing silently. Budget counts tool and model calls as the larger
of the matching steps and the `Usage` counter.

`RecordedHarness` maps task IDs to already recorded `RunResult` values, so a
suite can grade stored production runs without executing them again:

```go
report, err := evalkit.Runner{Harness: evalkit.RecordedHarness{
    "wf-1": recordedResult,
}}.Run(ctx, evalkit.Suite{
    Name:  "production-regression",
    Tasks: []evalkit.Task{{ID: "wf-1"}},
    Graders: []evalkit.Grader{
        evalkit.OutcomeStatus("succeeded"),
        evalkit.ForbidTools("delete_account"),
        evalkit.Budget{MaxModelCalls: 4}.Grader(),
    },
})
```

A task without a recording fails its trial. Runner rejects `RecordedHarness`
with more than one trial per task.

See `../examples/evalkit-goagent-regression` for a runnable `goagent` regression
evaluation that uses a mock LLM, repeated trials, and graders.

## Verify

```bash
go test ./...
```
