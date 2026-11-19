# trajectory-eval

Grades the stored trajectories of host-api workflows with evalkit's
trajectory graders, without running anything again. Use it as a regression
gate in CI or on a schedule against production traces.

It lists workflows through `GET /workflows` (or takes `--workflows`), reads
each one's content-free trace through `GET /workflows/{id}/trace`, grades it
with `evalkit.RecordedHarness`, and prints a JSON report.

```bash
go run ./examples/trajectory-eval \
  --host http://127.0.0.1:8080 \
  --project project-a \
  --status succeeded \
  --limit 50 \
  --suite suite.json
```

| Flag | Meaning |
|---|---|
| `--host` | host-api base URL (required) |
| `--suite` | suite JSON file (required) |
| `--project` | `project_id` for listing; required when the host enables memory |
| `--status` | only grade workflows in this status |
| `--limit` | how many of the newest workflows to grade (default 50, host caps at 100) |
| `--workflows` | comma-separated workflow IDs to grade instead of listing |

A bearer token for a memory-enabled host is read from
`TRAJECTORY_EVAL_TOKEN`, so it does not appear in process listings.

Exit codes: `0` every trajectory passed, `1` at least one failed, `2` a usage,
suite, or host error.

## Suite file

```json
{
  "name": "prod-regression",
  "graders": [
    {"type": "outcome_status", "statuses": ["succeeded"]},
    {"type": "require_tools", "tools": ["search"]},
    {"type": "forbid_tools", "tools": ["delete_account"]},
    {"type": "tool_order", "tools": ["search", "write"]},
    {"type": "no_failed_steps", "step_types": ["tool_call"]},
    {"type": "budget", "max_tool_calls": 6, "max_model_calls": 4, "max_tokens": 20000, "max_duration": "2m"}
  ]
}
```

Each grader maps to the evalkit grader of the same name; see
[`evalkit/README.md`](../../evalkit/README.md#trajectory-graders). Unknown
fields, unknown grader types, graders without their tools or statuses, and a
budget without any limit are rejected before any request is made. The graders
read only step types, names, statuses, timestamps, usage, and the outcome
status, so they work on the content-free traces the host returns.

The report lists each workflow with its grades and the failed assertions:

```json
{
  "suite": "prod-regression",
  "total": 2,
  "passed": 1,
  "failed": 1,
  "workflows": [
    {"workflow_id": "wf-2", "passed": false, "grades": [
      {"name": "trajectory.forbid_tools", "passed": false, "score": 0,
       "failures": ["did not call delete_account: tool \"delete_account\" was called 1 time(s)"]}
    ]}
  ]
}
```
