// Package prompt compiles prompt blocks into deterministic LLM input.
//
// Prompt blocks are model-facing instructions. They are not tools, memory,
// policy, or orchestration. The default compiler sorts ModeCacheable blocks
// before ModeDynamic blocks, then sorts by lower Priority, then by Name. Empty
// block content is omitted from compiled text, and non-empty block content is
// joined with newlines.
//
// Do not put secrets or raw sensitive data into prompt blocks unless the host
// intentionally wants that data in model context.
package prompt
