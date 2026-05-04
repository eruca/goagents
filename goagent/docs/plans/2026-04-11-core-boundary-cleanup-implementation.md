# Core Boundary Cleanup Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Remove unused summarization from the core memory contract and clarify that compression, summaries, and durable memory are extension concerns.

**Architecture:** Keep `ports.MemoryProvider` aligned with the default Agent pipeline: load session messages and save final message history. Delete no-op summary code from `memory.WindowMemory` and test doubles, then update docs to describe the narrower core boundary.

**Tech Stack:** Go standard library, `agentcore`, `ports`, `memory`, README/docs, `make verify`.

---

## Task 1: Remove Summarize From Core Memory Contract

**Files:**
- Modify: `ports/memory.go`
- Modify: `memory/window.go`
- Modify: `memory/window_test.go`
- Modify: `agentcore/agent_test.go`

**Step 1: Remove the interface method**

In `ports/memory.go`, change:

```go
type MemoryProvider interface {
	Load(ctx context.Context, sessionID string) ([]MemoryMessage, error)
	Save(ctx context.Context, sessionID string, messages []MemoryMessage) error
	Summarize(ctx context.Context, sessionID string) (string, error)
}
```

to:

```go
type MemoryProvider interface {
	Load(ctx context.Context, sessionID string) ([]MemoryMessage, error)
	Save(ctx context.Context, sessionID string, messages []MemoryMessage) error
}
```

**Step 2: Remove no-op implementation and test**

Delete `WindowMemory.Summarize` from `memory/window.go`.

Delete `TestWindowMemorySummarizeReturnsEmptySummary` from `memory/window_test.go`.

Delete `mockMemoryProvider.Summarize` from `agentcore/agent_test.go`.

**Step 3: Verify no active core references remain**

Run:

```bash
rg -n "Summarize|summarize" ports memory agentcore examples README.md
```

Expected: no output.

**Step 4: Run targeted tests**

Run:

```bash
go test ./ports ./memory ./agentcore -count=1
```

Expected: PASS.

**Step 5: Commit**

```bash
git add ports/memory.go memory/window.go memory/window_test.go agentcore/agent_test.go
git commit -m "refactor: remove summary from memory contract"
```

## Task 2: Clarify Documentation Boundary

**Files:**
- Modify: `README.md`
- Modify: `memory/doc.go`
- Modify: `docs/plans/2026-04-11-memory-contract-design.md`
- Modify: `docs/plans/2026-04-11-memory-contract-implementation.md`

**Step 1: Update README**

In the Session Memory section:

- Keep load/save behavior.
- Keep `WindowMemory` as in-process bounded session memory.
- Replace the `Summarize` sentence with:

```markdown
Summarization, compaction, vector retrieval, and durable storage are extension concerns, not core memory behavior.
```

**Step 2: Update package docs**

In `memory/doc.go`, replace the final sentence about `Summarize` with:

```go
// process restart. Summarization, compaction, vector retrieval, and durable
// storage are extension concerns, not core memory behavior.
```

**Step 3: Update memory design docs**

In `docs/plans/2026-04-11-memory-contract-design.md`:

- Replace "Keep the `ports.MemoryProvider` interface unchanged" with the two-method interface.
- Remove bullets that describe `Summarize` as a future-facing extension point.
- Keep non-goals for durable storage, vector memory, token compaction, and summary LLM.

In `docs/plans/2026-04-11-memory-contract-implementation.md`:

- Replace references to keeping the interface unchanged.
- Remove the task that pinned `WindowMemory.Summarize`.
- Update README snippet text so it matches current docs.

**Step 4: Verify remaining references are intentional**

Run:

```bash
rg -n "Summarize|summarize|CompactStage|compaction|PostgreSQL|Redis|vector" README.md memory docs/plans
```

Expected:

- No `Summarize` references in active docs.
- Historical broad design docs may still mention future `CompactStage`, PostgreSQL, Redis, or vector memory as non-goals or extensions.

**Step 5: Run docs-adjacent tests**

Run:

```bash
go test ./... -count=1
```

Expected: PASS.

**Step 6: Commit**

```bash
git add README.md memory/doc.go docs/plans/2026-04-11-memory-contract-design.md docs/plans/2026-04-11-memory-contract-implementation.md
git commit -m "docs: clarify core memory boundary"
```

## Task 3: Final Verification

**Files:**
- No new code changes expected.

**Step 1: Run full verification**

Run:

```bash
make verify
```

Expected: PASS.

**Step 2: Inspect status**

Run:

```bash
git status --short --branch
```

Expected: clean working tree on `codex/core-boundary-cleanup`.

**Step 3: Report**

Summarize:

- `MemoryProvider` now only owns load/save.
- `WindowMemory` remains in-process bounded session memory.
- Summarization, compaction, vector retrieval, and durable storage are documented as extension concerns.
- Verification passed.
