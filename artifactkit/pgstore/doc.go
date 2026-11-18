// Package pgstore implements artifactkit.Store with PostgreSQL persistence.
//
// Store has the same semantics as artifactkit.FileStore and MemoryStore: Put
// requires a ref and replaces any artifact stored under it (last writer
// wins), Get of a missing ref returns an error wrapping
// artifactkit.ErrArtifactNotFound, and content and metadata are copied on the
// way in and out. Unlike FileStore it is safe to share between processes.
//
// Content is stored as BYTEA and metadata as JSON text, encoded the same way
// as FileStore, so metadata values read back as their JSON-decoded types
// (numbers become float64). created_at records when a ref was first written
// and updated_at when it was last replaced.
//
// PostgreSQL TEXT cannot hold a NUL byte or invalid UTF-8, so Put rejects
// such refs and Get reports them as not found.
package pgstore
