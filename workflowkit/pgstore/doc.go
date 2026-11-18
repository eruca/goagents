// Package pgstore implements workflowkit.Store with PostgreSQL persistence.
//
// Store implements workflowkit.CreateStore, workflowkit.WorkflowQueryStore,
// and workflowkit.QueueRecoveryStore with the same semantics as sqlitestore,
// but is safe to share between processes: Update locks the run row for the
// whole read-mutate-write, and ClaimRunnable and FailExpiredRunning use
// FOR UPDATE SKIP LOCKED so concurrent workers never claim or report the same
// run twice.
//
// Times are stored as TIMESTAMPTZ, which keeps microseconds, so the store
// truncates every time it writes to microsecond precision. A NULL time column
// means the zero time. Completed steps, step attempts, step records, and
// metadata are stored as JSON text, encoded the same way as sqlitestore.
//
// ListWorkflows filters metadata in Go after reading the rows that match the
// status, so a metadata query scans every run with that status.
package pgstore
