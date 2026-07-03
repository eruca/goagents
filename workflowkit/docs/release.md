# WorkflowKit Release Notes

This workspace currently contains multiple Go modules:

- `github.com/eruca/workflowkit`
- `github.com/eruca/workflowkit/agentstep`
- `github.com/eruca/workflowkit/examples/agent-approval`
- `github.com/eruca/workflowkit/examples/ocr-review`

The examples are executable verification modules. They are not required runtime
dependencies for users.

## Release Boundary

Release the core module first:

```bash
cd /Users/nick/VibeCoding/goagents/workflowkit
./scripts/verify-e2e.sh
```

The E2E script checks that the core module does not depend on `goagent` or the
optional `agentstep` module.

Release the optional adapter only after the core module is tagged and usable:

```bash
cd /Users/nick/VibeCoding/goagents/workflowkit/agentstep
go test ./...
```

## Local Development Replaces

Local workspace development uses `replace` directives:

- `agentstep` replaces `github.com/eruca/goagent` and `github.com/eruca/workflowkit`
- `examples/agent-approval` replaces `goagent`, `workflowkit`, and `agentstep`
- `examples/ocr-review` replaces `contextkit`, `ocrs`, `goagent`, `workflowkit`, and `agentstep`

Before publishing a consumer module, replace directives must point at released
versions or be removed.

## Suggested Tagging

Use the same version number for the core and optional adapter when they are
released from the same source snapshot:

```text
workflowkit: v0.1.0
workflowkit/agentstep: v0.1.0
```

If the adapter changes without a core change, it may be tagged independently
after its own tests and the full workspace E2E script pass.

## Pre-Release Checklist

Run from the core module:

```bash
./scripts/verify-e2e.sh
```

Confirm:

- `go test ./...` passes
- `go test -race ./...` passes
- `examples/basic` reaches `waiting_approval` and then `succeeded`
- `examples/sqlite-resume` persists and resumes from SQLite
- main module does not depend on `goagent` or `workflowkit/agentstep`
- `agentstep` tests pass
- `examples/agent-approval` reaches approval wait and then succeeds
- `examples/ocr-review` composes OCR, context projection, agent review, and workflow approval

Also run adjacent module checks when this workspace is the release source:

```bash
(cd /Users/nick/VibeCoding/goagents/contextkit && go test ./...)
(cd /Users/nick/VibeCoding/goagents/ocrs && go test ./...)
(cd /Users/nick/VibeCoding/goagents/goagent && make verify)
```

## Compatibility Statement

The intended stable API surface is listed in `README.md` and
`docs/contracts.md`.

`sqlitestore` currently records `SchemaVersion = 2`. Until versioned migration
helpers are added, treat SQLite schema compatibility as limited to the current
schema. Future releases that change schema shape must bump the version and add
explicit migration tests.

## Not In Scope For The First Release

Do not block the first release on:

- DAG execution
- distributed workers
- delayed retry scheduling
- cron
- approval inbox UI
- artifact storage
- OpenTelemetry integration
- multi-agent team orchestration
