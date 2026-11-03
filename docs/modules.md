# GoAgents Package Layout

The repository is one Go module, `github.com/eruca/goagents`. Each top-level
directory is a kit: a group of packages that owns one boundary. Kits are
versioned and released together, but each kit's packages stay importable on
their own, so a host only builds the kits it uses.

## Kits

Core kits, which import no other kit:

- `goagent/`: agent loop, ports, tools, policy, prompt, memory window, and the
  OpenAI-compatible provider.
- `artifactkit/`: artifact references and durable stores.
- `contextkit/`: context windows, projections, and tool budgets.
- `evalkit/`: reproducible evaluation traces and graders.
- `hostkit/`: standard-library lifecycle coordination for one host service.
- `llmkit/`: model routing, provider health, audit, and outcome statistics.
- `memorykit/`: governed project memory, recall, and its stores.
- `ocrs/`: OCR parsing, chunking, scheduling, and retry.
- `skillkit/`: Skill discovery, gating, and activation.
- `workflowkit/`: durable workflow lifecycle, retries, approvals, and queues.

Kits with an allowed dependency:

- `runkit/` may import `goagent`: durable agent runs, events, and approval
  checkpoints.
- `mcpkit/` may import `goagent`: MCP tool descriptors as agent tools.

Adapter packages that connect a kit to `goagent`:

- `llmkit/adapters/goagent` may import `llmkit` and `goagent`.
- `memorykit/agentadapter` may import `memorykit` and `goagent`.
- `skillkit/agentadapter` may import `skillkit` and `goagent`.
- `workflowkit/agentstep` may import `workflowkit` and `goagent`.
- `mcpkit/officialsdk` may import `mcpkit`, `goagent`, and the official MCP Go
  SDK.

Example programs live in `examples/` directories, at the root or inside a kit,
and may import anything. They double as smoke tests and are not runtime
dependencies.

## Dependency Rules

`internal/boundary` fails the test suite when a package depends, directly or
transitively, on a kit its rule does not allow. Its `allowedDeps` table is the
source of truth for the rules above; a new kit or adapter package needs an
entry there.

## Verification

```bash
./scripts/verify-all.sh
./scripts/verify-external-consumer.sh
```

## Release

Tag the module as a whole with `vX.Y.Z`. Earlier releases tagged each kit as
its own module (for example `goagent/v0.1.2`); those tags are historical.
