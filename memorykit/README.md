# memorykit

`memorykit` provides governed, caller-owned long-term memory for GoAgents. V1
shares memory between Agents only inside one authorized project Scope. It keeps
storage, authorization, model providers, worker scheduling, and shutdown under
the Host's control; it is not a standalone service.

## Scope and trust boundary

A Scope is `{tenant_id, subject_type, subject_id}`. The Host V1 constructs only
`subject_type=project` from an authenticated tenant identity and a trusted
project route. HTTP bodies and memory tool schemas cannot supply or override
tenant, subject, or write permission.

`SubjectUser` exists only as a future-shaped storage/domain value. The Host V1
does not expose user-Scope routes or composition and rejects attempts to inject
`subject_type=user`.

Retrieved memory is untrusted contextual data. Only active, currently effective,
same-Scope records may enter model messages or memory tool results. Memory text
cannot add tools, grant write permission, or change Scope.

## Required configuration

There are no permissive production defaults. A Host must explicitly provide:

- validated `Limits`, including a version and positive bounds for keys, content,
  metadata, sources, and list size;
- a versioned `RecallPolicy` with exact, full-text, and vector budgets, RRF
  parameters, item/token/query limits, a deadline, and the embedding profile
  and dimension;
- a deterministic contract-compatible `Embedder` for the configured profile;
- a fail-closed project authorizer at the Host boundary;
- a content validator for direct writes and extracted candidates;
- caller-owned lifecycle, recall, embedding, and extraction-job Stores;
- explicit clocks, worker identities, lease/retry limits, and worker scheduling.

Do not run a production composition with a nil authorizer, an allow-all fallback,
or a partially initialized policy.

## PostgreSQL and pgvector

`pgstore.Open` uses the pgx driver, takes a DSN, acquires a schema advisory lock,
applies the versioned migration, runs `CREATE EXTENSION IF NOT EXISTS vector`,
and checks a vector value round trip. The database role therefore needs
permission to install/use pgvector during initial setup. V1 is verified against
PostgreSQL 16 and pgvector 0.8.2.

One local setup is:

```bash
docker run --name memorykit-postgres --rm \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=memorykit_test \
  -p 5432:5432 \
  pgvector/pgvector:0.8.2-pg16-bookworm
```

The Store is caller-owned. `pgstore.Open` owns and closes only the pool it
creates; Host shutdown must stop intake/workers before closing it.

## Recall paths

Automatic recall uses the Agent context projector. It searches before a model
call, enforces one configured deadline and item/token budget, fuses exact,
PostgreSQL full-text, and pgvector ranks, and inserts a bounded memory view
immediately before the current user message.

On-demand recall uses request-scoped `search_memory` and `read_memory` tools.
Their schemas contain no Scope fields. `remember_project_memory` is registered
only when trusted Host metadata grants explicit write intent.

A typed recoverable embedding/vector failure degrades to the remaining
exact/full-text channels and records only content-free channel/ID metadata.
Invalid Scope, policy, candidate shape, or Store integrity fails closed. Raw
query text, content, vectors, bearer headers, provider payloads, and Artifact
bodies are not valid log/event/error fields.

## Write paths

- A trusted explicit user/tool write creates an active record immediately after
  authorization and content validation.
- A deterministic trusted projection may create active memory without a model.
- Model extraction can only propose candidate records. Candidates remain hidden
  from recall until an authorized reviewer activates them.
- Candidate extraction jobs are durable and leased with bounded retries.
  Deterministic job/candidate identities make retries idempotent.

Explicit write failure is truthful: the tool returns `项目记忆未保存`, marks the
result as an error, and never returns success wording or the rejected content.
An extraction enqueue failure does not roll back an already successful Agent
run; it records a content-free degradation event for operator follow-up.

## Lifecycle and operations

- **Migration:** opening `pgstore` applies only the supported schema version and
  fails closed on an unavailable extension or incompatible version.
- **Embedding rebuild:** delete the affected embedding rows (or let changed
  content hashes become pending), then run `EmbeddingWorker` until
  `PendingEmbeddings` is empty. Exact/full-text recall remains available while
  vector rows are absent.
- **Activation:** changes a reviewed candidate to active and supersedes an
  existing active record with the same Scope/kind/key.
- **Correction:** creates a new revision, replaces content/sources, and
  invalidates the old embedding by content hash.
- **Forget:** changes an active record to inactive. It immediately leaves all
  recall paths but retains governed content, sources, and revision audit.
- **Erase:** keeps an inactive identity/audit tombstone while scrubbing content,
  content hash, sources, embeddings, and content-bearing revision snapshots.
- **Audit:** revisions record lifecycle action, actor, reason, version, and
  timestamps. Extraction-job completion/final failure retains only a
  content-free idempotency/operational tombstone.

## Verification

Unit tests:

```bash
go test -race ./...
go vet ./...
```

The PostgreSQL suite is mandatory evidence only when the required flag is set;
missing DSN, database, or pgvector then fails instead of skipping:

```bash
MEMORYKIT_REQUIRE_POSTGRES=1 \
MEMORYKIT_POSTGRES_TEST_DSN='postgres://postgres:postgres@localhost:5432/memorykit_test?sslmode=disable' \
go test -count=1 -race ./...

cd ../examples/host-api
MEMORYKIT_REQUIRE_POSTGRES=1 \
MEMORYKIT_POSTGRES_TEST_DSN='postgres://postgres:postgres@localhost:5432/memorykit_test?sslmode=disable' \
go test -count=1 -race -run '^TestHostMemoryPostgresBlackBox$' ./...
```

## Explicit non-goals

V1 has no network memory service, production memory UI, approximate-nearest-
neighbor index, live Agent-state copying, automatic candidate promotion, or
universal latency SLA. Recall enforces the caller's configured deadline/budget;
deployment-specific latency objectives remain a Host responsibility.
