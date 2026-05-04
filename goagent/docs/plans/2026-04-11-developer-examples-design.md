# Developer Examples Design

## Goal

Make the library easier to adopt by turning the current examples into a small learning path for embedding `goagent` in a host application.

## Context

The core now has typed agent runs, prompt blocks, skills, module wiring, tools, policy, memory, and runtime events. The API surface is intentionally library-shaped: users bring their own LLM client, storage, logging, transport, and business tools.

The next risk is usability, not missing infrastructure. A new user can run `examples/basic`, but the repository does not yet show how a host application observes runs or how the module pattern fits together in one place.

## Design

Add an `examples/events` package that runs a mock ReAct flow with an `EventSink`. The sink should print compact event lines with event type, stage, iteration, and bounded metadata. The example must avoid raw prompt dumps, raw tool input, and full tool output so it reinforces the privacy boundary.

Enhance `examples/module` enough to act as the main business integration example. It should show one module providing system prompts, skills, and request-scoped tools together. It should remain small and use a mock LLM. The module is host glue, not a dynamic plugin system.

Update `README.md` with a learning path:

1. `examples/basic` for the smallest agent run.
2. `examples/skills` for model-facing instructions.
3. `examples/module` for host module wiring.
4. `examples/events` for runtime observability.

## Non-Goals

- No real LLM provider.
- No HTTP, SSE, CLI framework, or background server.
- No PostgreSQL, vector memory, or persistent event storage.
- No new tracing/logging dependency.
- No filesystem skill loader.

## Success Criteria

- `go run ./examples/events` prints stable, readable event output.
- `go run ./examples/module` remains minimal and demonstrates module composition.
- `README.md` gives a clear path for users to learn the library.
- `make verify` passes.
