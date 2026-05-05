# GoAgents Module Layout

This repository is intended to be a workspace monorepo with independent Go
modules. Modules share one source tree for local development, but each module
keeps its own `go.mod`, dependency boundary, verification command, and release
tag.

## Modules

Core modules:

- `github.com/eruca/goagent` in `goagent/`
- `github.com/eruca/artifactkit` in `artifactkit/`
- `github.com/eruca/contextkit` in `contextkit/`
- `github.com/eruca/ocrs` in `ocrs/`
- `github.com/eruca/workflowkit` in `workflowkit/`

Optional adapter/capability modules:

- `github.com/eruca/workflowkit/agentstep` in `workflowkit/agentstep/`
- `github.com/eruca/llmkit` in `llmkit/` for LLM routing, account/model policy,
  and audit contracts. It is optional host-side capability, not part of
  `goagent` core.

Verification/example modules:

- `github.com/eruca/goagents/examples/host-runtime`
- `github.com/eruca/workflowkit/examples/agent-approval`
- `github.com/eruca/workflowkit/examples/ocr-review`

Example modules are used for workspace verification and should not be treated as
required runtime dependencies for users.

## Dependency Rules

Keep core modules independent:

```text
goagent       must not import workflowkit, contextkit, ocrs, or llmkit
workflowkit   must not import goagent, contextkit, ocrs, or workflowkit/agentstep
contextkit    must not import goagent, workflowkit, or ocrs
ocrs          must not import goagent, workflowkit, or contextkit
llmkit        must not import goagent from its core routing package
artifactkit   must not import goagent, workflowkit, llmkit, contextkit, or ocrs
```

Adapter and composition modules may depend on multiple core modules:

```text
workflowkit/agentstep        may import workflowkit + goagent
llmkit adapters              may import llmkit + goagent
examples/host-runtime        may import workflowkit + agentstep + goagent + llmkit + artifactkit
examples/agent-approval      may import workflowkit + agentstep + goagent
examples/ocr-review          may import workflowkit + agentstep + goagent + contextkit + ocrs
host applications            compose whatever modules they need
```

## Local Development

The root `go.work` file is for local development only. It lets the modules use
workspace sources without publishing intermediate versions.

Do not rely on `go.work` for external consumers. Published consumers should use
tagged module versions and no local `replace` directives.

## Tagging

Use Go subdirectory module tags:

```text
goagent/v0.1.0
artifactkit/v0.1.0
contextkit/v0.1.0
ocrs/v0.1.0
workflowkit/v0.1.0
workflowkit/agentstep/v0.1.0
llmkit/v0.1.0
```

Only tag modules that changed. If `workflowkit/agentstep` changes without a core
`workflowkit` change, tag only `workflowkit/agentstep`.

## Verification

Run the whole workspace:

```bash
./scripts/verify-all.sh
```

Module-specific checks:

```bash
(cd contextkit && go test ./...)
(cd artifactkit && go test ./...)
(cd ocrs && go test ./...)
(cd llmkit && go test ./...)
(cd examples/host-runtime && go test ./...)
(cd goagent && make verify)
(cd workflowkit && ./scripts/verify-e2e.sh)
```

## Repository Layout Note

The root repository tracks module contents directly. Keep feature work in
isolated branches or worktrees, and keep generated worktree directories ignored
by git.
