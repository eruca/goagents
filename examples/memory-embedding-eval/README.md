# memory-embedding-eval

Runs the memorykit retrieval corpus through the production recall path with
a real embedding model served by [Ollama](https://ollama.com):

1. seeds the corpus into a fresh PostgreSQL schema through `memorykit/pgstore`;
2. embeds every active memory with `memorykit.EmbeddingWorker`;
3. recalls every case with `memorykit.Recaller` for exact-only, fts-only,
   exact+fts, and, for each `--min-similarity` value, vector-only and
   exact+fts+vector;
4. writes a JSON report with the corpus hash, model and dimensions, overall
   and per-category metrics, misses and injections, per-case vector
   similarities, and latency. The report holds IDs only, never memory or
   query text.

```bash
ollama pull bge-m3
go run ./examples/memory-embedding-eval \
  --dsn 'postgres://postgres:postgres@localhost:5432/memorykit_test?sslmode=disable' \
  --model bge-m3 \
  --out report.json
```

The database needs pgvector. The schema is dropped afterwards unless
`--keep-schema` is set. Results for bge-m3 are in
[`docs/evals/memorykit-v2-ollama-bge-m3.md`](../../docs/evals/memorykit-v2-ollama-bge-m3.md).

The test runs every policy against PostgreSQL with a fake Ollama server when
`MEMORYKIT_POSTGRES_TEST_DSN` is set.
