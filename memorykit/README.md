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

Full-text recall splits the query with `memorykit.SearchTerms` into words and,
for runs of Chinese or Japanese characters, overlapping two-character grams
matched as substrings. A memory matches if it shares any term; only
memories matching more than half as many terms as the best match are kept, and
results rank by matched terms. `memorystore` and `pgstore` apply the same rule,
checked by `storetest.RunFullTextRecallConformance`. `pgstore` stores each
memory's words (`memorykit.IndexWords`) and grams (`memorykit.IndexGrams`) in
indexed `search_words` and `search_grams` columns, split the same way as the
query, so a word inside a dotted key such as `storage.vectors` matches in both
stores. Candidates come from those GIN indexes, so only memories sharing a term
are scored.

Full-text recall is lexical: it cannot tell apart a memory that shares the
query's words but answers a different question. In the v2 evaluation corpus,
"Which production UI color theme was approved" returns the draft memory
`ui.theme_draft` through the full-text channel, because both contain "ui" and
"theme". A minimum score cutoff does not fix this: on the same corpus, any
cutoff on the share of query terms that keeps this memory out also drops at
least four correct results, some of which match only one or two of the query's
terms. The evaluation tests pin this case as a known injection.

The vector channel does not separate it either. With bge-m3 (see
[`docs/evals/memorykit-v2-ollama-bge-m3.md`](../docs/evals/memorykit-v2-ollama-bge-m3.md)),
the draft is more similar to the query (0.584) than several correct answers
are to theirs, so no `MinVectorSimilarity` keeps it out without losing them.
Telling a draft from an approved decision needs a Host-side reranker. The same
evaluation shows what the vector channel does add: fusing all three channels
at a 0.5 threshold recalls 22 of 22 expected memories, against 19 for exact
plus full text, including all four paraphrase cases, with no new injection.
Run `examples/memory-embedding-eval` to measure another model; thresholds do
not carry over between models.

On-demand recall uses request-scoped `search_memory` and `read_memory` tools.
Their schemas contain no Scope fields. `remember_project_memory` is registered
only when trusted Host metadata grants explicit write intent. A model write
takes effect immediately only for a new kind/key or to replace the model's own
untouched earlier write. If the current active record was written, reviewed,
or corrected by anyone else, the write is stored as a candidate for review and
the existing record stays in effect. The check and the write are atomic: the
create carries `CreateRequest.ExpectedActive`, the active record the check saw,
and a store fails it with `ErrActiveMismatch` if a person wrote or reviewed one
in between, after which the decision is made again.

A typed recoverable embedding/vector failure degrades to the remaining
exact/full-text channels and records only content-free channel/ID metadata.
If the entire Recall Store returns a typed recoverable failure, the Agent
Projector injects no memory, records a content-free degradation marker, and
continues through the next projector/main task path. The same degradation
applies when Recall's own deadline expires while the caller's context is still
alive. The Projector keeps only the last `MaxQueryRunes` runes of an oversized
query instead of failing the turn. Invalid Scope or
authorization, policy, candidate shape, and Store integrity still fail closed.
Raw query text, content, vectors, bearer headers, provider payloads, and
Artifact bodies are not valid log/event/error fields.

## Write paths

- A trusted explicit user/tool write creates an active record immediately after
  authorization and content validation.
- A deterministic trusted projection may create active memory without a model.
- Model extraction can only propose candidate records. Candidates remain hidden
  from recall until an authorized reviewer activates them.
- Candidate extraction jobs are durable and leased with bounded retries. A
  failed job waits before it can be claimed again, `pgstore.Config`'s
  `ExtractionRetryDelay` (default 5s) doubling per failure up to
  `ExtractionMaxRetryDelay` (default 5m), so a short provider outage does not
  use up every attempt.
  Deterministic job/candidate identities make retries idempotent.

Explicit write failure is truthful: the tool returns `项目记忆未保存`, marks the
result as an error, and sets a request-scoped content-free failure flag. Hosts
compose the ToolProvider's output validator so any later model success claim is
rejected with the same fixed error before an output is persisted. A later
successful write in that run does not clear the failure flag. An extraction
enqueue failure does not roll back an already successful Agent run; it records
a content-free degradation event for operator follow-up.

## Lifecycle and operations

- **Migration:** opening `pgstore` upgrades older schema versions in place
  (version 2 adds and backfills `search_grams`; version 3 adds and backfills
  `search_words` and adds the extraction job `next_attempt_at`) and
  fails closed on an
  unavailable extension or a newer, unsupported version.
- **Embedding rebuild:** delete the affected embedding rows (or let changed
  content hashes become pending), then run `EmbeddingWorker` until
  `PendingEmbeddings` is empty. Exact/full-text recall remains available while
  vector rows are absent.
- **One embedding worker across replicas:** set
  `EmbeddingWorkerConfig.RunGate` to a `memorykit.RunGate`, whose
  `TryRun(ctx, run) (ran, err)` runs `run` only while no other holder is
  running. `RunOnce` then runs its batch inside `TryRun`; a round in which
  another holder has the gate returns `(0, nil)` without reading the backlog
  or calling the embedder, and, like an idle run, is not counted by
  `Snapshot`. An error from the gate itself is returned and counted as a
  failure with class `pending_read_failed` (or the cancellation class). A nil
  `RunGate` keeps the old behavior: every `RunOnce` runs.
  `pgstore.Store.EmbeddingRunGate()` implements it with a non-blocking
  `pg_try_advisory_lock` on a dedicated pooled connection, always followed by
  `pg_advisory_unlock` (with a context that ignores the caller's cancellation)
  even if `run` panics or is canceled; if the unlock cannot be confirmed, the
  connection is discarded so the session, and its lock, ends. Advisory locks
  are per database, not per schema, so the lock key hashes
  `memorykit.embedding`, the connection's current schema, and the embedding
  profile: Stores sharing a database, schema and profile share one lock;
  different schemas or profiles do not block each other.
- **Activation:** changes a reviewed candidate to active and supersedes an
  existing active record with the same Scope/kind/key.
- **Correction:** creates a new revision, replaces content/sources, and
  invalidates the old embedding by content hash. An inactive record (forgotten,
  dismissed, superseded, or erased) cannot be corrected and returns
  `ErrConflict`.
- **Forget:** changes an active record to inactive. It immediately leaves all
  recall paths but retains governed content, sources, and revision audit.
- **Erase:** keeps an inactive identity/audit tombstone while scrubbing content,
  content hash, sources, embeddings, and content-bearing revision snapshots.
- **Audit:** revisions record lifecycle action, actor, reason, version, and
  timestamps. Extraction-job completion/final failure retains only a
  content-free idempotency/operational tombstone.
- **Worker status:** `EmbeddingWorker.Snapshot` and
  `ExtractionWorker.Snapshot` return a content-free `WorkerSnapshot`: the
  exact durable backlog (via `EmbeddingBacklogReader` /
  `ExtractionBacklogReader`, which `memorystore` and `pgstore` implement), and
  process-local success/failure counts, last `WorkerErrorClass` and last
  transition time. Idle runs are not counted. The backlog reader comes from the
  worker config's `Backlog` field, or from the store itself when it implements
  the reader; without either, `Snapshot` returns
  `ErrWorkerBacklogUnavailable`.

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
