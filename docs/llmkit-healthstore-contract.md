# llmkit HealthStore contract

## Scope

`HealthStore` is a host-facing runtime health contract for provider/account/model
state. It lets routing avoid or downgrade unhealthy candidates without putting a
database, Redis client, tenant model, or production gateway policy inside
`llmkit` core.

The current built-in implementation is `MemoryHealthStore`. It is correct for a
single process and for deterministic examples. Multi-replica hosts should supply
their own shared implementation.

## Interface

```go
type HealthStore interface {
    Begin(context.Context, Candidate) error
    RecordOutcome(context.Context, TaskOutcome) error
    Snapshot() ProviderHealthSnapshot
}
```

The goagent adapter uses it in this order:

```text
available candidates
  -> ApplyProviderHealth(HealthStore.Snapshot())
  -> RoutePolicy.Select(...)
  -> HealthStore.Begin(selected candidate)
  -> provider.Chat(...)
  -> HealthStore.RecordOutcome(outcome)
```

`llmkit` routing only depends on `Snapshot`; it does not require a concrete
store type.

## Key

Health entries are keyed by:

```text
account_alias|model_alias|provider
```

Use `ProviderHealthKey(accountAlias, modelAlias, provider)` to construct the
same key as `llmkit`.

The key intentionally does not include project id, tenant id, user id, region,
or billing period. Hosts that need those dimensions should namespace their
storage outside the `llmkit` key, for example:

```text
tenant:{tenant_id}:llm_health:{account_alias}|{model_alias}|{provider}
```

Do not add tenant or billing fields to `TaskProfile` only to support health
storage.

## Entry Fields

`ProviderHealthEntry` contains only routing-safe metadata:

- `account_alias`
- `model_alias`
- `provider`
- `availability`
- `in_flight`
- `max_concurrency`
- `quota_remaining`
- `quota_exhausted`
- `failure_streak`
- `cooldown_until`
- `updated_at`

It must not contain prompts, responses, request headers, API keys, raw provider
payloads, or user content.

## Begin Semantics

`Begin(ctx, candidate)` is called after route selection and before the selected
provider is invoked.

Required behavior:

- Return `ctx.Err()` if the context is already cancelled.
- Identify the entry by `ProviderHealthKey`.
- Increment `in_flight` for the selected candidate.
- Preserve account/model/provider identity on the entry.
- Update `updated_at`.

Optional shared-store behavior:

- Reject the call if a distributed concurrency limit is already full.
- Acquire a short-lived lease with a TTL.
- Return an error if the lease cannot be acquired.

If `Begin` returns an error, the adapter does not call the provider.

## RecordOutcome Semantics

`RecordOutcome(ctx, outcome)` is called after the provider returns success or
failure.

Required behavior:

- Return `ctx.Err()` if the context is cancelled.
- Identify the entry by `ProviderHealthKey(outcome.AccountAlias,
  outcome.ModelAlias, outcome.Provider)`.
- Decrement `in_flight` if it is greater than zero.
- On provider success:
  - reset `failure_streak`
  - set `availability=available`
  - clear `cooldown_until`
- On provider failure:
  - increment `failure_streak`
  - mark the entry `degraded` until the cooldown threshold is reached
  - mark the entry `unavailable` and set `cooldown_until` after the threshold
- Update `updated_at`.

`TaskOutcome.Success` is provider-call success. Business outcome fields such as
`business_outcome` and `success_signal` can be recorded separately, but they
should not automatically drive provider health unless the host explicitly wants
that policy.

## Snapshot Semantics

`Snapshot()` returns a point-in-time view used by `ApplyProviderHealth`.

Required behavior:

- Return all currently known entries.
- Set `generated_at`.
- Return entries keyed by `ProviderHealthKey`.
- Do not mutate store state as a side effect.

`RoutePolicy` treats snapshot data as follows:

- `quota_exhausted=true`: hard filter.
- `quota_remaining < 0`: hard filter.
- `availability=unavailable` with no active cooldown: hard filter.
- active `cooldown_until`: hard filter.
- `in_flight >= max_concurrency`: hard filter when `max_concurrency > 0`.
- `availability=degraded`: still eligible, but scored lower.
- higher `failure_streak`: scored lower through health/reliability signals.

## MemoryHealthStore

`MemoryHealthStore` is suitable for:

- local development
- CLI workflows
- desktop apps
- tests
- single-process host-api deployments

It is not suitable for:

- horizontally scaled API servers
- shared account concurrency across multiple processes
- strict quota governance
- provider-wide rate-limit coordination

In those cases, use a custom `HealthStore`.

## Shared Store Requirements

A shared implementation should provide atomicity for:

- incrementing `in_flight` in `Begin`
- decrementing `in_flight` in `RecordOutcome`
- setting cooldown and availability after failures
- clearing cooldown after success

It should also defend against leaked in-flight counts when a process crashes
after `Begin` but before `RecordOutcome`. Use leases or TTLs for that.

Recommended behavior:

- Store `in_flight` as a lease count with per-call TTL.
- Generate an internal lease id in `Begin`; the public `HealthStore` interface
  does not currently pass `route_id`.
- Refresh or expire leases independently of the health entry.
- Let `Snapshot` compute `in_flight` from active leases.
- Keep `cooldown_until` in UTC.
- Treat stale entries as `available` unless host policy says otherwise.

## Redis-Style Pseudocode

```text
Begin(candidate):
  key = health_key(candidate)
  lease_key = key + ":leases"
  now = utc_now()
  remove_expired_leases(lease_key, now)
  entry = read_entry(key)
  if entry.max_concurrency > 0 and lease_count(lease_key) >= entry.max_concurrency:
      return concurrency_full
  lease_id = generate_internal_lease_id()
  add_lease(lease_key, lease_id, expires_at=now+lease_ttl)
  update_entry_identity_and_updated_at(key, candidate, now)

RecordOutcome(outcome):
  key = health_key(outcome)
  lease_key = key + ":leases"
  remove_one_active_lease(lease_key)
  entry = read_entry(key)
  if outcome.success:
      entry.failure_streak = 0
      entry.availability = available
      entry.cooldown_until = zero
  else:
      entry.failure_streak += 1
      if entry.failure_streak >= threshold:
          entry.availability = unavailable
          entry.cooldown_until = now + cooldown
      else:
          entry.availability = degraded
  entry.updated_at = now
  write_entry(key, entry)

Snapshot():
  for each entry:
      entry.in_flight = active_lease_count(entry)
  return ProviderHealthSnapshot{generated_at: now, entries: entries}
```

## SQL-Style Pseudocode

Use two tables:

```text
provider_health(
  key primary key,
  account_alias,
  model_alias,
  provider,
  availability,
  max_concurrency,
  quota_remaining,
  quota_exhausted,
  failure_streak,
  cooldown_until,
  updated_at
)

provider_health_leases(
  key,
  lease_id,
  expires_at,
  primary key(key, lease_id)
)
```

`Begin` runs in a transaction:

```text
delete expired leases for key
select health row for update
count active leases
if max_concurrency reached: rollback and return error
insert lease
upsert identity and updated_at
commit
```

`RecordOutcome` runs in a transaction:

```text
delete one active lease for key
select health row for update
update failure_streak, availability, cooldown_until, updated_at
commit
```

`Snapshot` can be eventually consistent. It does not need to lock all rows.

## Host Injection

Host code injects the implementation through the goagent adapter:

```go
client := goagentadapter.NewClient(goagentadapter.Config{
    Candidates:  candidates,
    Providers:   providers,
    HealthStore: sharedHealthStore,
    Recorder:    recorder,
})
```

`goagent` core still only sees `ports.LLMClient`; it does not know whether the
health store is memory-backed or shared.

## Error Handling

`Begin` errors are pre-provider failures. Hosts can decide whether they should
be treated as:

- retryable local store errors
- hard route failures
- signals to choose another candidate

The current adapter returns the error. Do not hide store failures by silently
calling the provider anyway; that defeats concurrency and quota protection.

`RecordOutcome` errors happen after provider completion. Hosts should treat
these as audit/health persistence errors. Provider output may already exist, so
do not assume the LLM call can be retried safely.

## Non-Goals

- No Redis, SQL, or distributed lock dependency in `llmkit` core.
- No tenant, project, or billing period fields in `TaskProfile`.
- No prompt, response, header, or API key storage in health entries.
- No production quota ledger. Use host budget governance for that.
- No guarantee that `MemoryHealthStore` coordinates multiple processes.

## Verification Checklist

A custom `HealthStore` should have tests for:

- `Begin` increments in-flight.
- `RecordOutcome(success)` decrements in-flight and clears cooldown.
- repeated failures move availability from degraded to unavailable.
- cooldown produces a snapshot that `RoutePolicy` hard-filters.
- max concurrency produces a snapshot that `RoutePolicy` hard-filters.
- cancelled context returns `ctx.Err()`.
- leaked leases expire.
- snapshot does not expose secret or prompt data.
