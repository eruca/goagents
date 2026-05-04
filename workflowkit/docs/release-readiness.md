# WorkflowKit Release Readiness

Date: 2026-05-03

## Judgment

`workflowkit` is ready to enter `v0.1.0` preparation.

The first release should be scoped as a host-side workflow baseline:

- sequential workflow execution
- approval wait and resume
- bounded audit and artifact refs
- step records
- transient retry
- memory store
- SQLite store with schema version 1
- optional `agentstep` adapter module
- deterministic examples and E2E verification

Do not expand the release scope to DAGs, workers, cron, UI, artifact storage, or
multi-agent orchestration.

## Verified Release Slice

The release slice is covered by:

```bash
cd /Users/nick/VibeCoding/goagents/workflowkit
./scripts/verify-e2e.sh
```

The script verifies:

- core package tests
- race tests
- basic workflow example
- SQLite resume example
- main module dependency boundary
- optional `agentstep` tests
- `agent-approval` nested module
- `ocr-review` nested module

Adjacent workspace checks for this source tree:

```bash
(cd /Users/nick/VibeCoding/goagents/contextkit && go test ./...)
(cd /Users/nick/VibeCoding/goagents/ocrs && go test ./...)
(cd /Users/nick/VibeCoding/goagents/goagent && make verify)
```

## Current Module Layout

Core module:

- `github.com/eruca/workflowkit`

Optional adapter module:

- `github.com/eruca/workflowkit/agentstep`

Verification/example modules:

- `github.com/eruca/workflowkit/examples/agent-approval`
- `github.com/eruca/workflowkit/examples/ocr-review`

The example modules are not required user-facing release artifacts.

## Blockers Before Tagging

There are no runtime or test blockers for the first release slice.

Operational blockers before an actual public tag:

- Decide where `workflowkit` will live as a git-tracked module. The current
  workspace root `/Users/nick/VibeCoding/goagents` is not a git repository; only
  `/Users/nick/VibeCoding/goagents/goagent` is currently git-tracked.
- Decide whether `workflowkit` and `workflowkit/agentstep` receive matching
  `v0.1.0` tags from the same repository snapshot.
- Remove or replace local `replace` directives before publishing any consumer
  module outside this workspace.

## Acceptable Known Limitations

These are documented and do not block `v0.1.0`:

- SQLite schema compatibility is limited to `SchemaVersion = 1` until migration
  helpers are introduced.
- Retry is process-local and immediate; it is not a delayed job scheduler.
- `StepRecords` stores refs and bounded metadata, not raw prompts, tool inputs,
  OCR payloads, model messages, or full tool output.
- `agentstep` is optional host glue and intentionally separate from the core.
- Examples use local replace directives for workspace verification.

## Recommended Tag Sequence

After moving or adding the module to the intended git repository:

1. Run `./scripts/verify-e2e.sh`.
2. Run adjacent checks if releasing from this workspace.
3. Tag the core module as `v0.1.0`.
4. Tag `workflowkit/agentstep` as `v0.1.0` if releasing the adapter in the same
   snapshot.
5. Re-test a clean consumer module with released versions instead of local
   replace directives.
