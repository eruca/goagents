// Package sqlitestore implements runkit.Store with SQLite persistence.
//
// Run identity and query fields are stored as columns. Metadata and terminal
// summary tool lists are stored as JSON so hosts can keep bounded audit context
// without putting prompts, messages, or large artifacts in the run store.
package sqlitestore
