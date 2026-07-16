# Extension Guide

`goagent` is the agent runtime core. It owns orchestration, not every capability
an agent might use.

Keep OCR, context compression, retrieval, durable memory, file parsing, and
domain actions outside `agentcore` unless they are small generic contracts that
the runtime needs in order to compose a run.

## Dependency Direction

Use this dependency shape:

```text
application
  imports github.com/eruca/goagents/goagent
  imports capability modules such as github.com/eruca/goagents/ocrs
  wires capability modules into goagent through tools, providers, or memory

github.com/eruca/goagents/goagent does not import sibling capability modules
sibling capability modules do not import github.com/eruca/goagents/goagent
```

This keeps `goagent` reusable for many hosts and keeps capability modules usable
outside agent applications.

## What Belongs In Core

Core packages should stay focused on stable orchestration primitives:

- `agentcore`: agent API, run state, ReAct pipeline, stages, events, budgets,
  skills, modules, and run lifecycle.
- `ports`: DTOs and interfaces that cross package boundaries.
- `prompt`: deterministic prompt block compilation.
- `tools`: tool contracts, registry, execution, schema validation, middleware,
  and result separation.
- `policy`: framework-default permission checks.
- `memory`: simple reference memory implementations.

Add a new core contract only when the runtime must understand that concept to
execute runs correctly. Concrete provider logic should remain outside core.

## What Belongs In Sibling Modules

Sibling modules under the broader `goagents` workspace should own concrete
capabilities:

- `ocrs`: PaddleOCR client, OCR response parsing, PDF chunk orchestration.
- `contextkit`: context compression and summarization utilities.
- `ragkit`: retrieval, reranking, and document index helpers.
- `memories`: durable or semantic memory implementations.

Those modules should document how an application can connect them to `goagent`.
They should not require `goagent` unless their only purpose is an adapter.

## Tools Are The Capability Boundary

Most domain capabilities should enter `goagent` as tools owned by the
application:

```go
type DomainTool struct {
	// Dependencies from sibling modules, databases, or host services.
}

func (t DomainTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "domain_action",
		Description: "Performs one bounded host-owned action.",
		Permission:  policy.PermissionRead,
	}
}

func (t DomainTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	// Validate model input, call host dependencies, and return bounded output.
	return &tools.Result{
		ForLLM:  "small model-visible observation",
		ForUser: "larger user-visible payload or external reference",
	}, nil
}
```

The model can request a tool call, but host code owns execution, authorization,
I/O, and result shaping.

## Large Outputs

Capabilities such as OCR and retrieval can produce outputs larger than the model
context window. Do not put full raw outputs in `ForLLM`.

Prefer this pattern:

- `ForLLM`: title, counts, short preview, IDs, or a compact summary.
- `ForUser`: full raw result, structured JSON, or a pointer to stored output.
- `Ref`: a stable host-owned artifact reference such as `ocr:run-1` or
  `query:result-1`.
- `Metadata`: small structured facts such as row count, chunk count, or MIME
  type.
- Host storage: full documents, OCR chunks, embeddings, and audit records.

If the model needs more content later, expose a second bounded read tool such as
`read_ocr_chunk` or `search_document_chunks`.

For model-facing history, prefer projecting tool observations into a compact
shape such as `tool=<name>`, `status=<status>`, `ref=<id>`, and a bounded
`result=<preview>`. This keeps tool names and outcomes while removing noisy raw
payloads and unnecessary intermediate explanation.

## Context Compression

Context compression is different from OCR. OCR is a model-requested capability,
so it fits naturally as a tool. Compression controls what the runtime sends to
the model, so it should be a runtime extension point.

The recommended direction is:

1. Build compression algorithms in a sibling module such as `contextkit`.
2. Keep concrete summarizers and storage outside `agentcore`.
3. Add a small `goagent` port only when the runtime needs to compress messages
   before `ThinkStage`.

The optional projection stage sits before model calls:

```text
MemoryLoad
Context
SystemPrompt
Skills
ToolProvider
Prompt
ContextProjection
Think
Budget
Policy
Act
Observe
Finalize
```

Applications can provide `WithContextProjector` to compress or reshape the
model-facing message view. `agentcore` owns the abstract port and stage; concrete
algorithms remain in sibling modules or host code.

When the application configures budget with `WithBudget`, the projection request
receives the same `Budget` values so compressors can make budget-aware
decisions. When the application provides a custom `BudgetGuard`, the core treats
that guard as opaque and sends a zero-value projection budget.

Deep compression should be configured by the capability module or host
application, not by `agentcore`. `contextkit` uses
`CONTEXT_DEEP_COMPRESSION=1` to enable its deeper reversible-collapse and
auto-compact layers.

## Documentation Pattern

Each sibling module should include a README section named `Use With goagent`.
That section should show application-level wiring instead of adding adapters to
`goagent` core.

For example, `ocrs` documents how to wrap `paddleocr.Client` and
`chunking.CutAwareHandler` as a `goagent` tool in application code.
