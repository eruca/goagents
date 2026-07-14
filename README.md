# goagents

`goagents` is a Go workspace for composing durable agent runtimes from small,
independent kits. Each top-level module owns one boundary and can be used or
tested independently.

## Modules

- [`goagent`](goagent/README.md): agent loop, typed tools, approvals, events, and provider adapters.
- [`workflowkit`](workflowkit/README.md): durable workflow lifecycle, retries, approvals, and queue leases.
- [`runkit`](runkit/README.md): durable agent run records, events, and approval checkpoint storage.
- [`artifactkit`](artifactkit/README.md): artifact references and durable stores.
- [`llmkit`](llmkit/README.md): model routing, provider health, audit, and outcome statistics.
- [`skillkit`](skillkit/README.md): immutable Skill discovery, gating, activation, and agent projection.
- [`contextkit`](contextkit/README.md): context windows, projections, and tool budgets.
- [`evalkit`](evalkit/README.md): reproducible agent evaluation traces and graders.
- [`mcpkit`](mcpkit/README.md): MCP transport and tool integration.
- [`ocrs`](ocrs/README.md): OCR parsing, chunking, scheduling, and retry support.

## Verify the workspace

From the repository root:

```bash
bash ./scripts/verify-all.sh
```

This runs module tests, race checks for the core execution paths, MCP smokes,
and runnable examples.

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
