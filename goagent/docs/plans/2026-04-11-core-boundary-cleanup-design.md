# Core Boundary Cleanup Design

> Date: 2026-04-11
> Goal: Keep `agentcore` focused on the Agent runtime contract and move context compression, summarization, and persistence concerns out of the core memory API.

## Context

The current core has a small session memory contract and an in-process `memory.WindowMemory` implementation. The default Agent pipeline only uses memory for two operations:

1. Load session messages before appending the current user input.
2. Save the final message history after a successful run.

`ports.MemoryProvider.Summarize` exists, but the pipeline never calls it. `memory.WindowMemory.Summarize` returns an empty string only to satisfy the interface. This makes summarization look like a core responsibility even though context compression and summary quality are separate product and infrastructure concerns.

## Boundary Decision

Remove summarization from the core `MemoryProvider` interface.

The core memory contract should be:

```go
type MemoryProvider interface {
	Load(ctx context.Context, sessionID string) ([]MemoryMessage, error)
	Save(ctx context.Context, sessionID string, messages []MemoryMessage) error
}
```

This keeps the core aligned with what the default pipeline actually does. Hosts that need summarization, compaction, vector retrieval, durable storage, or profile memory can build those as extension packages around this contract or define richer host-side interfaces.

## Core Responsibilities

- Own Agent lifecycle and stage execution.
- Load and save bounded session transcript memory when configured.
- Preserve deterministic failure behavior: memory saves only after a final answer.
- Expose budget denial, policy denial, tool errors, and max-iteration failures as runtime outcomes.
- Keep provider, persistence, compaction, and retrieval strategies outside `agentcore`.

## Non-Core Responsibilities

- Context compression.
- Summary generation and summary storage.
- Token estimation and prompt pruning.
- Persistent memory backends such as PostgreSQL or Redis.
- Vector memory, embedding, or retrieval ranking.
- Long-term user profile memory.
- Cost accounting or budget ledgers.

## Implementation Impact

- Remove `Summarize` from `ports.MemoryProvider`.
- Remove the no-op `WindowMemory.Summarize` method.
- Remove the `WindowMemory` summary test.
- Remove `Summarize` from agent test doubles.
- Update README and memory package docs to state that summarization and persistence are extension concerns.
- Update previous memory design/implementation docs so they no longer recommend pinning an unused summary contract.

## Compatibility Note

This is a breaking interface cleanup for any custom `MemoryProvider` implementation that added `Summarize` only to satisfy the old interface. Those implementations can delete the unused method. If a host already uses `Summarize` directly, it should keep that method on its own concrete type or define a host-specific extension interface.

## Success Criteria

- No `Summarize` symbol remains in core packages, examples, or active docs except historical references in the original broad design.
- `memory.WindowMemory` still loads, saves, and bounds session messages.
- `agentcore` memory behavior is unchanged.
- `make verify` passes.
