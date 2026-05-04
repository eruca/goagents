# ContextKit Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build `github.com/eruca/contextkit` with standard and deep context compression profiles.

**Architecture:** The root package defines messages, budgets, profiles, levels, and compressor contracts. `toolbudget` implements level 1 truncation helpers. `window` implements a deterministic compressor for levels 1-5, using `CONTEXT_DEEP_COMPRESSION=1` to choose deep behavior.

**Tech Stack:** Go 1.26.1, standard library only.

---

### Task 1: Core Contracts And Profile Parsing

**Files:**
- Create: `/Users/nick/VibeCoding/goagents/contextkit/go.mod`
- Create: `/Users/nick/VibeCoding/goagents/contextkit/message.go`
- Create: `/Users/nick/VibeCoding/goagents/contextkit/profile.go`
- Create: `/Users/nick/VibeCoding/goagents/contextkit/compressor.go`
- Test: `/Users/nick/VibeCoding/goagents/contextkit/profile_test.go`

**Steps:**
1. Write failing tests for default profile, deep profile from `CONTEXT_DEEP_COMPRESSION=1`, disabled flag fallback, and level selection.
2. Run tests and verify failure.
3. Implement core types and profile parsing.
4. Run tests and verify pass.

### Task 2: Tool Result Budget

**Files:**
- Create: `/Users/nick/VibeCoding/goagents/contextkit/toolbudget/toolbudget.go`
- Test: `/Users/nick/VibeCoding/goagents/contextkit/toolbudget/toolbudget_test.go`

**Steps:**
1. Write failing tests for unchanged short output and truncated long output.
2. Run tests and verify failure.
3. Implement bounded truncation.
4. Run tests and verify pass.

### Task 3: Window Compressor

**Files:**
- Create: `/Users/nick/VibeCoding/goagents/contextkit/window/window.go`
- Test: `/Users/nick/VibeCoding/goagents/contextkit/window/window_test.go`

**Steps:**
1. Write failing tests for system preservation, recent-message retention, summary placeholder, projection semantics, and deep collapse metadata.
2. Run tests and verify failure.
3. Implement deterministic window compressor.
4. Run tests and verify pass.

### Task 4: README And Verification

**Files:**
- Create: `/Users/nick/VibeCoding/goagents/contextkit/README.md`

**Steps:**
1. Document the five compression levels.
2. Document `CONTEXT_DEEP_COMPRESSION=1`.
3. Document how to use with `goagent` without importing `goagent`.
4. Run `gofmt -w`, `go mod tidy`, and `go test ./...`.
