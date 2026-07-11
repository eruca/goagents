# Approval Checkpoint Store Implementation Plan

**Goal:** Add an opaque encrypted approval checkpoint store with atomic lease and audit semantics.

**Architecture:** `runkit` owns a database-agnostic contract and memory proof; `pgstore` implements it with PostgreSQL transactions. The host owns encryption, key management, authorization, and conversion to `goagent.RunCheckpoint`.

## Tasks

1. Write failing memory-contract tests for create, approve-and-lease, duplicate rejection, tenant/config mismatch, expiry, rejection, and completion.
2. Add `runkit/checkpoint.go` and `runkit/checkpoint_test.go`; run `go test . -run Checkpoint`.
3. Write a failing PostgreSQL integration test gated by `RUNKIT_POSTGRES_TEST_DSN`.
4. Add `runkit/pgstore` with pgx-backed migration and transactional implementation; run the integration test against a temporary database.
5. Add README usage/boundary documentation, then run `go test ./...`, `go test -race ./...`, `bash ./scripts/verify-all.sh`, and `git diff --check`.
