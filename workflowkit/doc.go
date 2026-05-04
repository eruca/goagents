// Package workflowkit provides host-side workflow orchestration contracts for
// composing ordered steps, approvals, retries, resumable state, and references to
// host-owned artifacts or audit records.
//
// The package is intentionally not an agent runtime. Agent execution, OCR,
// context compression, durable payload storage, queues, HTTP APIs, and UI layers
// belong in host applications or sibling modules.
package workflowkit

