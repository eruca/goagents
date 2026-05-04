# Memory Contract Design

## Goal

Make the memory extension contract explicit so host applications understand when memory is loaded, when it is saved, and what the built-in window memory does.

## Context

The Agent already accepts a `MemoryProvider`, loads memory by `RunRequest.SessionID`, prepends loaded messages before the current input, and saves final message history after a successful run. The built-in `memory.WindowMemory` keeps a bounded in-process message window by session.

The current behavior is useful but under-documented. Users need to know that this is short-term session memory, not durable storage, summary memory, vector search, or a user profile system.

## Design

Keep the `ports.MemoryProvider` interface focused on default pipeline behavior:

- `Load(ctx, sessionID)` returns previous messages for the session.
- `Save(ctx, sessionID, messages)` persists the post-run message window.

Summarization, compaction, vector retrieval, and durable storage are extension concerns. They should not be required by the core memory interface.

Clarify the default pipeline contract:

1. Memory is active only when both `WithMemoryProvider` and `RunRequest.SessionID` are set.
2. Memory loads before the current user input is appended.
3. Memory is loaded at most once per run, even if the ReAct loop takes multiple iterations.
4. Memory saves only after the run reaches a final answer.
5. Memory is not saved on load errors, tool errors, policy errors, or max-iteration failures.
6. Saved messages include the loaded messages, current user input, tool observations that were not silent, and final assistant output.

Document `WindowMemory` as an in-process bounded implementation:

- It is safe for concurrent use.
- It stores messages by session ID.
- It drops older messages past the configured limit.
- It does not survive process restart.

## Example Shape

Add `examples/memory` to show two runs using the same `SessionID` and one shared `memory.WindowMemory`.

The first run returns a deterministic final answer and saves it. The second run should receive the previous answer in its LLM request, proving that session memory was loaded. The example should print the final answer from each run and a compact line showing how many messages the second LLM call saw.

Use mock LLMs only. Do not add a real provider, database, vector store, summary model, or external persistence.

## Tests

Add focused tests for behavior not already pinned:

- Memory loads once when a tool call requires multiple ReAct iterations.
- Memory is not saved when the run ends with `ErrMaxIterations`.

Existing tests already cover:

- no load/save without `SessionID`
- successful load and save
- load error
- save error
- bounded window truncation

## Non-Goals

- No PostgreSQL or durable storage.
- No vector memory or retrieval ranking.
- No token-based compaction.
- No summary LLM.
- No long-term user profile memory.

## Success Criteria

- Memory behavior is covered by tests.
- `examples/memory` runs and demonstrates session continuity.
- README and memory package docs explain the memory contract.
- `make verify` passes.
