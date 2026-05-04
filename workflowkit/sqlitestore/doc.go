// Package sqlitestore implements workflowkit.Store with SQLite persistence.
//
// Scalar workflow fields are stored as columns. Completed steps, step attempts,
// step records, and metadata are stored as JSON fields so the Store contract can
// persist the full workflow state while keeping payload storage host-owned.
package sqlitestore

