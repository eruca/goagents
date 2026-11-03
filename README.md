# goagents

`goagents` is one Go module, `github.com/eruca/goagents`, for composing
durable agent runtimes from small kits. Each top-level directory is a kit that
owns one boundary; a kit's packages import no other kit except where
[`docs/modules.md`](docs/modules.md) allows it, which `internal/boundary`
enforces in tests. Go compiles only the packages you import, so depending on
the module does not build unrelated kits or their dependencies.

```bash
go get github.com/eruca/goagents@latest
```

## Kits

- [`goagent`](goagent/README.md): agent loop, typed tools, approvals, events, and provider adapters.
- [`workflowkit`](workflowkit/README.md): durable workflow lifecycle, retries, approvals, and queue leases.
- `workflowkit/agentstep`: adapter from agent runs to workflow steps.
- [`runkit`](runkit/README.md): durable agent run records, events, and approval checkpoint storage.
- [`artifactkit`](artifactkit/README.md): artifact references and durable stores.
- [`hostkit`](hostkit/README.md): standard-library lifecycle coordination for one host-owned service.
- [`llmkit`](llmkit/README.md): model routing, provider health, audit, and outcome statistics.
- [`skillkit`](skillkit/README.md): immutable Skill discovery, gating, activation, and agent projection.
- [`memorykit`](memorykit/README.md): governed project memory, hybrid recall, pgvector persistence, and candidate review lifecycle.
- [`contextkit`](contextkit/README.md): context windows, projections, and tool budgets.
- [`evalkit`](evalkit/README.md): reproducible agent evaluation traces and graders.
- [`mcpkit`](mcpkit/README.md): MCP transport and tool integration.
- [`mcpkit/officialsdk`](mcpkit/officialsdk/README.md): official MCP Go SDK transport adapter.
- [`ocrs`](ocrs/README.md): OCR parsing, chunking, scheduling, and retry support.

## Verify

From the repository root:

```bash
bash ./scripts/verify-all.sh
bash ./scripts/verify-external-consumer.sh
```

`verify-all.sh` checks the single-module layout and formatting, then runs
`go vet`, `go test -race ./...` (including the kit dependency boundaries), MCP
smokes, and the runnable examples. `verify-external-consumer.sh` packs the
working tree into a local module proxy and builds and tests a separate module
that depends on it with `GOWORK=off` and no replace directives.

The real PostgreSQL/pgvector gates are separate and fail-closed:

```bash
MEMORYKIT_REQUIRE_POSTGRES=1 \
MEMORYKIT_POSTGRES_TEST_DSN='postgres://postgres:postgres@localhost:5432/memorykit_test?sslmode=disable' \
go test -count=1 -race ./memorykit/...

RUNKIT_POSTGRES_TEST_DSN='postgres://postgres:postgres@localhost:5432/runkit_test?sslmode=disable' \
go test -count=1 -race ./runkit/...

MEMORYKIT_REQUIRE_POSTGRES=1 \
MEMORYKIT_POSTGRES_TEST_DSN='postgres://postgres:postgres@localhost:5432/memorykit_test?sslmode=disable' \
go test -count=1 -race -run '^TestHostMemoryPostgresBlackBox$' ./examples/host-api
```

## Release

Releases are tags of the whole module: `vX.Y.Z` on a commit where CI passed.
After pushing a tag, confirm that the published version resolves and builds:

```bash
bash ./scripts/verify-external-consumer.sh vX.Y.Z
```

Before the single-module layout, each kit was its own module with tags such as
`goagent/v0.1.2`. Those versions still exist in the module proxy. A consumer
that starts with `go get github.com/eruca/goagents@vX.Y.Z` resolves every kit
from this module; requiring an old per-kit module path at the same time would
make imports ambiguous.

## Host API MVP

The runnable single-host MVP is documented in
[`examples/host-api`](examples/host-api/README.md). On an interactive macOS
login session with CGO and an unlocked login Keychain, run its complete
process-level acceptance suite:

```bash
cd examples/host-api
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPBlackBoxClosure$' \
  -count=1 ./...
```

All three subtests must report `PASS`. A `SKIP` means the local environment is
blocked and is not evidence of a completed MVP acceptance run. The smoke uses
loopback test providers, synthetic credentials, isolated temporary runtime
state, and separate `.smoke.` Keychain service/account pairs for its three
scenarios. Each scenario removes only its exact pair.
The suite never accesses the production-default Keychain item.

The Host API README also documents a separately tagged real
OpenAI-compatible Provider gate. That gate requires caller-supplied local
credentials; a missing configuration is blocked, not replaced by a fake
Provider.

The longer single-host stability gate runs three waves of 50 queued workflows,
with 10 concurrent HTTP submitters and a real Host process restart:

```bash
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPStability$' \
  -count=1 ./...
```

This target-specific gate is documented in the Host API README. It checks the
current single-slot worker for convergence and resource growth; it is not a
production throughput SLO. A `SKIP` is not a passing acceptance result.

The historical MVP GO decision and its accepted P2 items remain recorded in
the [v0.1.0 final acceptance record](docs/superpowers/specs/2026-07-15-mvp-final-acceptance.md).
Both lifecycle P2 items are now implemented. The last per-kit module release
is recorded in the
[v0.1.1 publication record](docs/superpowers/specs/2026-07-18-v0.1.1-publication.md);
releases are now whole-module tags as described under [Release](#release).

## License

GoAgents is licensed under the [Apache License 2.0](LICENSE).
