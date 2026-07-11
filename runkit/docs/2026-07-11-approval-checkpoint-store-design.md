# Approval Checkpoint Store Design

## Goal

Provide a `runkit` persistence boundary that atomically leases one encrypted
approval checkpoint to one resumer, records the approving operator, and never
automatically retries a lease whose external side effect may be unknown.

## Boundary

`runkit` stores an opaque ciphertext only. It does not import `goagent`, decode
`RunCheckpoint`, encrypt data, manage keys, or authorize the caller. The host
must encrypt before `CreateCheckpoint`, decrypt after a successful lease, and
authenticate the approver before calling the store.

## Contract

An `ApprovalCheckpoint` has a host-generated ID, run ID, tenant ID, definition
hash, ciphertext, status, expiry, optional lease, and optional approval audit.
Terminal failures also retain a bounded failure code, so operators can tell a
decryption failure from a continuation persistence failure without exposing the
decrypted checkpoint or raw error text.
The status flow is:

```text
pending --approve and lease--> leased --complete--> consumed
   |                                  \--fail/expire--> failed or expired
   \--reject-----------------------------------------> rejected
```

`ApproveAndLease` performs a conditional `pending` update and approval-audit
insert in one transaction. It checks tenant, definition hash, and expiry. A
second caller cannot receive the same ciphertext. `FailLease` is terminal;
there is no automatic retry or lease stealing. This prevents a possible
external write from being replayed after a crashed worker.

## PostgreSQL Adapter

`runkit/pgstore` uses `database/sql` with `pgx` and creates two tables:

- `approval_checkpoints`: opaque payload plus lifecycle and lease columns;
- `approval_decisions`: one immutable decision per checkpoint.

The package exposes `Open(ctx, dsn)`, migration, and the checkpoint-store
contract. Integration tests run only with `RUNKIT_POSTGRES_TEST_DSN`; they make
and remove a temporary database so normal workspace verification stays local
and deterministic.

## Verification

The memory contract tests prove atomic leasing, tenant/config binding,
expiration, terminal rejection, and defensive ciphertext copies. PostgreSQL
integration tests prove the same lifecycle against a temporary database.

## Local SQLite Adapter

`sqlitestore.Store` implements the same `CheckpointStore` contract in the
existing `agent-runs.db` connection. It uses conditional updates inside SQLite
transactions, so only one caller can move a pending checkpoint to `leased`.
The SQLite schema stores ciphertext, lifecycle fields, failure code, and the
immutable approval decision; it never stores decrypted agent state.
