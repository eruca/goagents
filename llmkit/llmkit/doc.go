// Package llmkit defines contracts for routing LLM calls across accounts and
// models.
//
// llmkit helps host applications choose a model and account for a task based on
// task profile, model capability, cost, latency, concurrency, and audit needs.
// It owns routing, account/model policy, and audit contracts, but it does not
// store API keys. Implementations should refer to keys through host-owned
// environment variables, secret stores, or aliases.
//
// llmkit is an optional adapter/capability module in the goagents workspace. It
// is not part of github.com/eruca/goagent core, and goagent must not import it.
package llmkit
