# Tool Contract Design

## Goal

Make tools the clearest extension point in `goagent` by documenting and testing the minimal contract for safe tool implementation.

## Context

The project now has a small ReAct core, module wiring, skills, memory, event sinks, and runnable examples. The most important host-facing extension point is `tools.Tool`: host applications expose business capabilities through tools, and the Agent decides whether and when to call them.

The current tool API already has the right small pieces: `Spec`, `Permission`, `Schema`, `Timeout`, middleware, and separated `ToolResult` fields. The risk is that users do not yet have a crisp contract for what each field means or how failures should behave.

## Design

Keep the API small. Do not add a marketplace, dynamic loading, capability negotiation, retry system, or external integration layer.

Document the tool contract around four boundaries:

1. **Tool responsibility**: a tool is a host-owned typed action. It should perform one concrete operation and return structured output for the Agent and host. It should not do model reasoning, prompt construction, orchestration, or transport management.
2. **Safety gates**: `Permission` declares the kind of operation, `PolicyEngine` approves or rejects requested calls, `Schema` validates model-supplied input before execution, and `Timeout` bounds execution.
3. **Result separation**: `ForLLM` is the observation appended to model context, `ForUser` is host-facing output, `Silent` suppresses observation messages, and `IsError` represents a recoverable tool-level result that the model may use to correct its next step.
4. **Executor errors**: registry misses, schema failures, timeouts, middleware failures, and returned Go errors abort the current run path. Recoverable domain errors should be returned as `ToolResult{IsError: true, ForLLM: ...}` instead of Go errors.

## Example Shape

Add `examples/tools` as the canonical "how to write tools" example. It should use mock LLM responses and deterministic tools to show:

- read-only lookup with schema validation
- separate model-visible and user-visible output
- recoverable domain error through `IsError`
- silent result that does not become an observation

The example should stay local and deterministic. No real network service, database, provider, or secrets.

## Tests

Add focused tests around existing behavior rather than broad new abstractions:

- schema validation failure does not call the tool
- timeout returns an executor error
- `IsError` tool results are appended as observations
- `Silent` tool results are not appended as observations

## Non-Goals

- No real LLM provider.
- No OpenAPI importer.
- No dynamic tool loading or plugin marketplace.
- No rate limiting, retry policy, or circuit breaker.
- No HTTP/SSE transport.
- No PostgreSQL or vector memory.

## Success Criteria

- Existing tool behavior is covered by tests.
- `examples/tools` runs and demonstrates the contract.
- README has a concise `Writing Tools` section.
- `make verify` passes.
