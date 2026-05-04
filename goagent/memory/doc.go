// Package memory provides message memory implementations.
//
// Memory is session-scoped. The default Agent pipeline uses memory only when a
// MemoryProvider is configured and the request includes a SessionID. It loads
// memory before appending the current user input, loads at most once per run,
// and saves only after a successful final answer.
//
// WindowMemory is an in-process bounded session store. It is safe for
// concurrent use, drops older messages past its limit, and does not survive
// process restart. Summarization, compaction, vector retrieval, and durable
// storage are extension concerns, not core memory behavior.
package memory
