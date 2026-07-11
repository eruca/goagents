# Local SQLite and Keychain Approval Checkpoints Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Persist encrypted tool-approval checkpoints in the existing local SQLite runtime and protect the data key with the local macOS Keychain.

**Architecture:** `runkit/sqlitestore.Store` gains the existing `CheckpointStore` contract, so approval records live beside `agent_runs` in `agent-runs.db`. `runkit/approvalcrypto` supplies versioned AES-256-GCM envelopes through a key-provider interface. The goagent adapter passes stable checkpoint identity as AEAD associated data; a Keychain-backed provider is local-host-only and never writes key material to SQLite.

**Tech Stack:** Go 1.26, modernc SQLite, Go standard `crypto/aes`, `crypto/cipher`, macOS Keychain through `github.com/99designs/go-keychain`.

## Global Constraints

- Reuse `agent-runs.db`; do not add PostgreSQL or another runtime database file.
- Keep `runkit` checkpoint lifecycle semantics identical to the PostgreSQL store: atomic lease, immutable decision, terminal failure, no replay.
- Encrypt `agentcore.RunCheckpoint` only with AES-256-GCM and a fresh random nonce.
- Authenticate `checkpoint_id`, `tenant_id`, and `definition_hash` as AEAD associated data.
- Store only `{version, key_id, nonce, ciphertext}` in SQLite; never key bytes or raw checkpoint state.
- Keychain is the only allowed local secret backend; no insecure fallback to files or environment variables.

---

### Task 1: Add SQLite checkpoint persistence

**Files:**

- Create: `runkit/sqlitestore/checkpoint.go`
- Modify: `runkit/sqlitestore/store.go`
- Modify: `runkit/sqlitestore/store_test.go`

**Consumes:** `runkit.CheckpointStore`, `ApprovalCheckpoint`, lease requests, and lease completion values.

**Produces:** `*sqlitestore.Store` implementing `runkit.CheckpointStore` in the same `agent-runs.db` connection.

- [x] Write a lifecycle test that creates, approves-and-leases, fails with a failure code, closes/reopens SQLite, and asserts ciphertext, decision audit, status, and failure code persist.
- [x] Run `go test ./sqlitestore -run TestCheckpoint -count=1`; observe compile failure because checkpoint methods do not exist.
- [x] Add `approval_checkpoints` and `approval_decisions` migration tables plus conditional SQLite updates for approve/reject/complete/fail/expire. Map duplicate, missing, and non-claimable transitions to the runkit sentinel errors.
- [x] Add a concurrent approve test and run `go test ./sqlitestore -count=1` successfully.

### Task 2: Add versioned AES-GCM checkpoint encryption

**Files:**

- Create: `runkit/approvalcrypto/aesgcm.go`
- Create: `runkit/approvalcrypto/aesgcm_test.go`
- Modify: `runkit/goagentapproval/adapter.go`
- Modify: `runkit/goagentapproval/adapter_test.go`

**Consumes:** a `KeyProvider` returning an active 32-byte key and resolving a key by key ID.

**Produces:** a `Cipher` compatible with `goagentapproval` that encrypts a versioned envelope and refuses altered ciphertext or mismatched associated data.

- [x] Write failing tests for a plaintext round-trip, ciphertext tampering, and an AAD change from one tenant/definition binding to another.
- [x] Run `go test ./approvalcrypto -count=1`; observe failure because the package does not exist.
- [x] Implement AES-256-GCM with a random nonce, JSON envelope `{version,key_id,nonce,ciphertext}`, exact 32-byte key validation, and a key-provider interface.
- [x] Change the goagent adapter cipher interface to accept AAD and derive canonical AAD from checkpoint ID, tenant ID, and definition hash; update adapter tests and run `go test ./goagentapproval ./approvalcrypto -count=1`.

### Task 3: Add a macOS Keychain data-key provider

**Files:**

- Create: `runkit/approvalcrypto/keychain.go`
- Create: `runkit/approvalcrypto/keychain_test.go`
- Modify: `runkit/go.mod`
- Modify: `runkit/go.sum`
- Modify: `runkit/README.md`

**Consumes:** the `KeyProvider` contract and the native macOS Security.framework binding from `github.com/99designs/go-keychain`.

**Produces:** a local provider that creates a random active 32-byte data key only in macOS Keychain, resolves older key IDs for unexpired checkpoints, and fails if Keychain is unavailable.

- [x] Write failing tests with an injected in-memory secret-store fake proving create-once, key-ID resolution, and no cross-ID substitution.
- [x] Run `go test ./approvalcrypto -run TestKeychain -count=1`; observe failure because the provider does not exist.
- [x] Implement the provider with a testable secret-store wrapper and a Darwin-only Security.framework adapter; never enable file/env fallbacks.
- [x] Add the dependency, update local-host documentation, and run `go test ./... && go test -race ./...` from `runkit`.

### Task 4: Verify the local persistence boundary

- [x] Run `bash ./scripts/verify-all.sh && git diff --check` from repository root.
- [x] Do not invoke the real Keychain in tests; tests use the injected fake store.
