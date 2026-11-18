# Private project file ingestion

Caller-owned CLI example for **UTF-8 text → registered evidence → candidate →
explicit review/activation → recall**. It reuses memorykit without changing its
core contracts, schema, or project-only Host boundary. OCR/PDF extraction happens
upstream: put completed `.txt` files in a flat inbox, using atomic rename after
writing. This command scans once; scheduling belongs to the caller.

## Trust and storage

This is a local operator CLI, not an authenticated network endpoint. OS access
to the executable, configuration, registry and database is its authorization
boundary. Tenant/project/actor configuration comes from the operator environment;
documents and model output cannot override it. For a network host, retain the
existing fail-closed project authorizer and capability checks.

Use a **dedicated database and registry for one private project**, with no other
writers. memorykit extraction/embedding workers enumerate jobs across the store;
this example checks existing database scopes before proceeding but cannot stop
another independent writer racing that check. It deliberately refuses existing
foreign-scope rows. It is not a multi-project worker adapter. The registry lock
serializes CLI processes; after a crash remove `operator.lock` only after verifying
no process is running. Temp-file recovery is also an operator action, fail closed.

Registry snapshots and cached candidate drafts are private plaintext files
(created 0600 under a 0700 directory). Use a new private directory, preserve its
permissions, and back it up with the database. Source refs contain project identity
hash, full-file hash and chunk ordinal, never arbitrary paths or URLs. The reader
uses `os.OpenRoot`, rejects foreign refs and verifies evidence hashes. Original
filenames are not retained. Keep originals separately if needed.

A source chunk is at most 1,600 Unicode runes; files are bounded at 8 MiB. This is
simple character chunking, not layout-aware PDF parsing. UTF-8 and NUL checks do
not establish factual accuracy. Candidate review must check the actual evidence.

The first validated extractor output is cached before worker writes so retries
reuse candidate content, keys and timestamps. Identical bytes imported under a
new filename reuse job identities. Changing bytes creates a new document; changing
`extractor-version` creates a new job. Keep the version tied to model/prompt/settings.
The same key replaces existing active memory **only on explicit activation**.
Document-scoped keys keep contrary claims from different documents separate.
No candidate is automatically activated, regardless of confidence.

File jobs use their deterministic ingestion-job UUID as `SourceAgentID`:
it identifies the caller-owned source operation, not an LLM author or authority.
The worker checks the configured extractor ID before reading cached drafts or
invoking the extractor. Finish pending jobs with their original model/prompt
configuration before changing versions. A mismatch fails closed and follows the
existing worker failure/retry accounting; do not repeatedly retry with the wrong
configuration. Configuration changes under the same version label cannot be
detected by this example and remain the operator's responsibility.

`list`, `recall` and `benchmark` write requested results to stdout. Review/recall
output contains knowledge text; do not treat it as telemetry. Errors are fixed,
content-free messages. memorykit erase does not delete originals, snapshots or
cached drafts: removal of those external artifacts is caller-owned. Deleting
memory alone is not end-to-end erasure. Do not reimport erased sources unless the
operator intends to create new candidates.

## Run

Requires Go 1.26.1, PostgreSQL 16 + pgvector 0.8.2. From this directory:

```sh
docker run --name memory-ingest-pg --rm \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=memory_ingest \
  -p 127.0.0.1:5432:5432 pgvector/pgvector:0.8.2-pg16-bookworm
```

In another shell:

```sh
export MEMORYKIT_POSTGRES_TEST_DSN='postgres://postgres:postgres@localhost:5432/memory_ingest?sslmode=disable'
export MEMORY_TENANT_ID='local-owner'
export MEMORY_PROJECT_ID='personal-knowledge'
export MEMORY_ACTOR='owner'
export MEMORY_INGEST_EXCLUSIVE_DB=1
mkdir -m 700 -p /tmp/memory-inbox /tmp/memory-registry
printf '%s\n' 'Example source claim about citrate monitoring.' > /tmp/memory-inbox/example.txt
go run . -command ingest -registry /tmp/memory-registry -input /tmp/memory-inbox \
  -extractor copy-smoke -extractor-version v1 -profile lexical-only-v1 -dimensions 3
go run . -command list -registry /tmp/memory-registry -profile lexical-only-v1 -dimensions 3
```

`copy-smoke` copies a source chunk into an **untrusted candidate** to exercise the
lifecycle offline. It is not an LLM summary and does not generate embeddings.
Read the returned content/source and note the candidate's UUID and version:

```sh
go run . -command activate -registry /tmp/memory-registry \
  -profile lexical-only-v1 -dimensions 3 \
  -id '<reviewed UUID>' -version 1 -approve -reason 'Checked against source'
go run . -command recall -registry /tmp/memory-registry \
  -profile lexical-only-v1 -dimensions 3 -query 'citrate'
```

For model extraction set `OPENAI_COMPAT_BASE_URL`, `OPENAI_COMPAT_MODEL`, and
optionally `OPENAI_COMPAT_API_KEY`; select `-extractor model` and a new immutable
`-extractor-version`. The example uses the existing bounded OpenAI-compatible
client and strict JSONCandidateExtractor. The provider must support its JSON
candidate contract and existing usage metadata requirement. Extraction has no
tools or activation authority. It may produce zero candidates legitimately.

For real semantic recall, start with a dedicated database configured for the
actual embedding profile/dimension, set:

```sh
export MEMORY_EMBEDDING_URL='https://your-provider.example/v1/embeddings'
export MEMORY_EMBEDDING_MODEL='your-embedding-model'
# MEMORY_EMBEDDING_API_KEY is optional for an authenticated local server.
go run . -command recall -registry /tmp/memory-registry \
  -profile '<immutable embedding profile>' -dimensions '<model dimension>' \
  -vector -query 'your query'
```

The embeddings adapter requires HTTPS (or loopback HTTP), blocks redirects,
bounds responses, and validates item indices/dimensions. memorykit additionally
validates finite values and embedding/content-hash consistency. Active embeddings
are rebuilt before recall; benchmark timing excludes that rebuild. A rebuild
failure stops the CLI; a subsequent recoverable query-embedding failure can still
use memorykit's lexical degradation. No dummy embeddings are used by the CLI.

## Minimal acceptance / benchmark

```sh
go test -race ./...
go vet ./...
MEMORYKIT_REQUIRE_POSTGRES=1 go test -count=1 -race -run TestPostgresFileLifecycle ./...
```

The required PostgreSQL test fails rather than skips if DSN/database/extension is
missing. Run it on a **separate disposable test database**: its fixtures remain
for inspection and are foreign to the CLI project. It checks repeat import,
candidate invisibility, explicit versioned activation, real pgvector query using
a labeled 3D fixture embedder, cross-project exclusion, evidence return and forget.
Fixture vectors test database plumbing, **not semantic retrieval quality**.

Start with 10 real documents / 20 labeled questions, then 50 documents / 60
questions. After reviewing/activating, create a private JSON case file:

```json
[
  {
    "id": "known-source-1",
    "query": "a question answerable from the reviewed evidence",
    "keys": [],
    "expected_ids": ["actual-active-memory-UUID"],
    "forbidden_ids": ["actual-candidate-or-inactive-memory-UUID"]
  },
  {"id": "no-answer-1", "query": "an unrelated question", "expected_ids": [], "forbidden_ids": []}
]
```

```sh
go run . -command benchmark -registry /tmp/memory-registry \
  -profile '<profile>' -dimensions '<dimension>' -cases /path/to/private-cases.json
# Repeat with -vector for full-text + vector + optional explicit keys.
```

The runner emits memorykit RecallAtBudget/ForbiddenInjected, MRR@5 over answerable
cases, no-answer returned counts, errors/degradation, and p50/p95 end-to-end recall
latency (query embedding + SQL + fusion; excludes answer generation/rebuild).
The rune-based budget is explicit and deterministic, not an exact provider token
count. The runner is a first labeled evaluation pass, not a stable latency SLA;
repeat warmed runs and isolate SQL timing for performance decisions.

Full-text uses existing PostgreSQL `simple` configuration: Chinese segmentation
is not implemented. Include Chinese terminology, English abbreviations,
synonyms, OCR errors and no-answer questions. Use optional `keys` to test exact
key matches. Semantic quality must use real embeddings; do not label the fixture
as evidence of quality. For 1k/10k capacity runs, record CPU/RAM, dimensions,
concurrency 1/4, SQL `EXPLAIN (ANALYZE, BUFFERS)` and query-only latency separately.
Add segmentation/index changes only if these measurements identify a gap.

Initial targets (not measured claims): forbidden injection 0, source resolution
100%, Recall@5 ≥ 85%; database p95 ≤ 200 ms on the target machine. With this
exclusive-project CLI, foreign-scope checks belong to the PostgreSQL test suite,
not a mixed-scope production benchmark database.
