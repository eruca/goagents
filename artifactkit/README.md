# ArtifactKit

`artifactkit` is a small host-side artifact contract for Go agent applications.
It stores full payloads behind refs so workflow records, agent events, tool
results, and LLM context can carry bounded references instead of raw content.

It is intentionally storage-neutral. The core package includes an in-process
`MemoryStore` for tests, examples, and prototypes, plus `FileStore` for simple
durable host-side storage.

## Use

```go
store := artifactkit.NewMemoryStore()

err := store.Put(ctx, artifactkit.Artifact{
    Ref:         "artifact:wf-1:input",
    Content:     []byte("full payload"),
    ContentType: "text/plain",
    Metadata: map[string]any{
        "source": "upload",
    },
})
if err != nil {
    return err
}

artifact, err := store.Get(ctx, "artifact:wf-1:input")
if err != nil {
    return err
}
_ = artifact
```

`MemoryStore` copies artifacts on read and write so callers cannot mutate stored
state through shared slices or maps.

For durable storage, open a file-backed store:

```go
store, err := artifactkit.NewFileStore("/srv/my-agent/runtime/artifacts")
if err != nil {
    return err
}
```

`FileStore` encodes refs into safe object filenames and stores artifact content,
content type, metadata, and creation time as JSON. It preserves the same
copy-on-read/write semantics as `MemoryStore`.

## PostgreSQL Store

Use `pgstore` when several host replicas share one artifact store:

```go
store, err := pgstore.Open(ctx, "postgres://user:pass@host:5432/app?sslmode=require")
if err != nil {
	return err
}
defer store.Close()
```

`pgstore.OpenDB(ctx, db)` uses a caller-owned `*sql.DB` and leaves it open on
`Close`. Both migrate the schema under a PostgreSQL advisory lock and refuse a
database migrated by a newer `pgstore.SchemaVersion`.

The store has the same semantics as `FileStore`: `Put` replaces any artifact
stored under the ref (last writer wins), `Get` of a missing ref returns an
error wrapping `ErrArtifactNotFound`, and content and metadata are copied on
the way in and out. Content is stored as `BYTEA` in the `artifacts` table and
metadata as JSON text, so metadata values read back as their JSON-decoded
types, as with `FileStore`.

Known limits:

- PostgreSQL `TEXT` cannot hold a NUL byte or invalid UTF-8, so `Put` rejects
  such refs and `Get` reports them as not found. `FileStore` accepts them.
- Each artifact is read and written whole in one row; very large payloads
  belong in object storage with a ref kept here.

Tests run against a real database when `ARTIFACTKIT_POSTGRES_TEST_DSN` is set,
and fail instead of skipping when `ARTIFACTKIT_REQUIRE_POSTGRES=1`.

## Boundary

`artifactkit` does not import `goagent`, `workflowkit`, `llmkit`, `contextkit`,
or `ocrs`. Host applications compose it with those modules.

Use artifact refs in workflow records, tool results, route metadata, and UI
surfaces. Keep raw prompts, model responses, OCR JSON, files, and other large or
sensitive payloads in an artifact store chosen by the host.

## Verify

```bash
go test ./...
```
