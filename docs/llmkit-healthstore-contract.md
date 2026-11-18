# llmkit HealthStore contract

## Scope

`HealthStore` is a host-facing runtime health contract for provider/account/model
state. It lets routing avoid or downgrade unhealthy candidates without putting a
database, Redis client, tenant model, or production gateway policy inside
`llmkit` core.

`MemoryHealthStore` is correct for a single process and for deterministic
examples. `llmkit/pghealth` is the reference shared implementation, backed by
PostgreSQL, for hosts that run several replicas. Both apply the same rules (see
Reference Rules) and pass the same conformance suite,
`llmkit/healthtest.RunHealthStoreConformance`.

## Interface

```go
type HealthStore interface {
    Acquire(context.Context, Candidate) error
    Release(context.Context, Candidate) error
    RecordOutcome(context.Context, TaskOutcome) error
    Snapshot() ProviderHealthSnapshot
}
```

The goagent adapter uses it in this order:

```text
available candidates
  -> ApplyProviderHealth(HealthStore.Snapshot())
  -> RoutePolicy.Select(...)
  -> HealthStore.Acquire(selected candidate)
       busy -> drop the candidate, select again (no attempt spent)
  -> route trace, pre-dispatch hook
       failure -> HealthStore.Release(selected candidate)
  -> provider.Chat(...)
  -> HealthStore.RecordOutcome(outcome)
```

The snapshot is a pre-filter that keeps routing from choosing a candidate that
is visibly unavailable; `Acquire` is the authoritative, atomic admission check,
so concurrent requests cannot all pass a stale snapshot.

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
- `last_failure_at`
- `cooldown_until`
- `updated_at`

It must not contain prompts, responses, request headers, API keys, raw provider
payloads, or user content.

## Acquire Semantics

`Acquire(ctx, candidate)` is called after route selection and before anything
else happens for that attempt. It must check admission and take the slot in
one atomic step (a mutex, a transaction, or a compare-and-set).

Required behavior:

- Return `ctx.Err()` if the context is already cancelled.
- Identify the entry by `ProviderHealthKey`.
- Reject with an error wrapping `ErrProviderBusy`, without changing the entry,
  when:
  - `cooldown_until` is in the future;
  - `availability=unavailable` with no cooldown (set by the host);
  - `availability=unavailable`, the cooldown has passed, and `in_flight > 0`:
    half-open admits exactly one recovery probe;
  - quota is exhausted;
  - `in_flight` reached the limit: the entry's `max_concurrency`, or else the
    candidate's configured `Model.MaxConcurrency`;
  - the account's total `in_flight` across its entries reached
    `candidate.AccountMaxConcurrency`.
- Otherwise increment `in_flight`, preserve account/model/provider identity,
  and update `updated_at`.

The adapter treats `ErrProviderBusy` as "try another candidate": it records no
outcome and does not spend a fallback attempt. Any other error is a store
failure and ends the request. Every successful `Acquire` is closed by exactly
one `RecordOutcome` (the provider was called) or `Release` (it was not).

## Release Semantics

`Release(ctx, candidate)` gives back a slot whose call never reached the
provider, for example because the route recorder or the pre-dispatch hook
failed. It decrements `in_flight` if it is greater than zero and changes
nothing else. The adapter calls it with a short cleanup context detached from
request cancellation.

## Degraded Recovery

A degraded entry (failures below the threshold) whose `last_failure_at` is at
least `CooldownDuration` old is reported as available with no streak, and a
later failure starts a new streak. Degraded providers rank below healthy ones,
so without this they would rarely be chosen and never get the success that
clears them. Shared stores should apply the same rule when they read an entry.

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
- On a failure the caller caused (`CountsTowardProviderHealth(outcome.ErrorClass)`
  is false: canceled, request too large, budget exceeded, policy blocked,
  capability mismatch, post-call error), leave `failure_streak`,
  `availability`, and `cooldown_until` unchanged.
- On a provider failure while the circuit is already open (`unavailable` and
  `cooldown_until` in the future), only move `last_failure_at`: the call was
  admitted before the circuit opened, so it neither counts nor extends the
  cooldown.
- On any other provider failure:
  - increment `failure_streak`
  - mark the entry `degraded` until the cooldown threshold is reached
  - mark the entry `unavailable` and set `cooldown_until` once the threshold
    is reached; each failure past it, such as a failed recovery probe, doubles
    the cooldown up to `MaxCooldownDuration`
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

In those cases, use `llmkit/pghealth` or a custom `HealthStore`.

## Shared Store Requirements

A shared implementation should provide atomicity for:

- checking admission and incrementing `in_flight` in `Acquire`, across every
  key of the account (the account limit spans keys, and half-open admits one
  probe across all replicas)
- decrementing `in_flight` in `RecordOutcome`
- setting cooldown and availability after failures
- clearing cooldown after success

It should also defend against leaked in-flight counts when a process crashes
after `Acquire` but before `RecordOutcome` or `Release`. Use leases or TTLs for that.

Recommended behavior:

- Store `in_flight` as a lease count with per-call TTL.
- Generate an internal lease id in `Acquire`; the public `HealthStore` interface
  does not currently pass `route_id`.
- Refresh or expire leases independently of the health entry.
- Let `Snapshot` compute `in_flight` from active leases.
- Keep `cooldown_until` in UTC.
- Treat stale entries as `available` unless host policy says otherwise.

## Reference Rules

The rules below are what `MemoryHealthStore` and `pghealth.Store` both apply.
They are exported as pure functions so a custom store can apply the same ones
instead of re-implementing them:

- `llmkit.EvaluateAdmission(policy, entry, candidate, accountInFlight, now)`
- `llmkit.ApplyHealthOutcome(policy, entry, outcome, now)`
- `llmkit.SettleHealthEntry(policy, entry, now)`

Unset `HealthPolicy` fields default to `threshold=3`, `cooldown=1m`, and
`max_cooldown=32*cooldown`.

```text
settle(entry, now):
  # degraded recovery: failures far apart are not consecutive
  if entry.availability == degraded and entry.last_failure_at is set
     and now - entry.last_failure_at >= cooldown:
      entry.availability = available
      entry.failure_streak = 0
  return entry

Acquire(candidate):                    # atomic per account
  if ctx is cancelled: return ctx.err
  key = health_key(candidate)
  entry = read_entry(key) or zero entry, identity filled from candidate
  entry.availability = entry.availability or available
  entry.in_flight = live calls admitted to key
  account_in_flight = live calls admitted to any key of candidate.account
  entry = settle(entry, now)
  if entry.cooldown_until is set and now < entry.cooldown_until:
      busy("provider is in cooldown")
  if entry.availability == unavailable and entry.cooldown_until is unset:
      busy("provider is unavailable")           # set by the host
  if entry.availability == unavailable and entry.in_flight > 0:
      busy("provider recovery probe is in flight")   # half-open: one probe
  if entry.quota_exhausted or entry.quota_remaining < 0:
      busy("provider quota is exhausted")
  limit = entry.max_concurrency if > 0 else candidate.model.max_concurrency
  if limit > 0 and entry.in_flight >= limit:
      busy("provider concurrency is full")
  if candidate.account_max_concurrency > 0
     and account_in_flight >= candidate.account_max_concurrency:
      busy("account concurrency is full")
  entry.in_flight += 1                 # take the slot (a lease, in a shared store)
  entry.updated_at = now
  write_entry(key, entry)
  # busy(...) returns an error wrapping ErrProviderBusy and writes nothing

RecordOutcome(outcome):
  if ctx is cancelled: return ctx.err
  key = health_key(outcome)
  entry = read_entry(key) or zero entry
  entry identity = outcome identity
  entry.in_flight -= 1 if > 0          # give back the slot (delete a lease)
  entry = settle(entry, now)
  if outcome.success:
      entry.failure_streak = 0
      entry.availability = available
      entry.cooldown_until = unset
  else if not CountsTowardProviderHealth(outcome.error_class):
      pass                             # the caller caused it
  else if entry.availability == unavailable and now < entry.cooldown_until:
      entry.last_failure_at = now      # circuit already open: admitted before
                                       # it opened, so it neither counts nor
                                       # extends the cooldown
  else:
      entry.failure_streak += 1
      entry.last_failure_at = now
      if entry.failure_streak >= threshold:
          entry.availability = unavailable
          # exponential backoff: each failure past the threshold, such as a
          # failed recovery probe, doubles the cooldown
          entry.cooldown_until = now + min(cooldown * 2^(failure_streak - threshold), max_cooldown)
      else:
          entry.availability = degraded
  entry.updated_at = now
  write_entry(key, entry)

Snapshot():
  for each entry:
      entry.in_flight = live calls admitted to it
      entry = settle(entry, now)
  return ProviderHealthSnapshot{generated_at, entries}
```

## Reference Shared Implementation: pghealth

`llmkit/pghealth.Store` implements the rules above in PostgreSQL and is the
reference for other shared stores. It uses two tables:

```text
llmkit_provider_health(
  key primary key,                     -- ProviderHealthKey
  account_alias, model_alias, provider,
  availability, max_concurrency, quota_remaining, quota_exhausted,
  failure_streak, last_failure_at, cooldown_until, updated_at
)                                      -- in_flight is not stored

llmkit_provider_health_leases(
  lease_id primary key,
  key, account_alias,
  owner,                               -- the Store instance that took it
  acquired_at, expires_at
)
```

`Acquire` runs in one transaction:

```text
pg_advisory_xact_lock(hashtext('llmkit.account:' || account))
  -- serializes admissions per account: the account limit spans keys, and
  -- half-open must admit one probe across replicas
insert entry row if missing; select it FOR UPDATE
delete the account's expired leases
in_flight = leases for key; account_in_flight = leases for account
EvaluateAdmission(...)
  busy -> rollback, return the ErrProviderBusy error (entry unchanged)
update entry; insert lease(expires_at = now + LeaseTTL)
commit
```

`Release` deletes the oldest lease this instance holds for the key
(`FOR UPDATE SKIP LOCKED`, so concurrent releases take different leases).
`RecordOutcome` locks the entry row, deletes that lease, and writes
`ApplyHealthOutcome(...)` in one transaction.

Each instance renews its own unexpired leases every `LeaseTTL/3`, so a long
call keeps its slot; a crashed instance's leases stop being renewed and free
their slots within `LeaseTTL`. An expired lease is never revived, since another
replica may already have counted its slot as free. `Snapshot()` serves a cache
reloaded every `RefreshInterval` and after the instance's own writes; it is
eventually consistent, which the contract allows because `Acquire` is the
authoritative check. Times come from `HealthPolicy.Now` on each replica, so
replicas' clocks must agree to well within `LeaseTTL/3`.

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

`Acquire` errors wrapping `ErrProviderBusy` move routing to the next
candidate; when no candidate admits the call, the request fails with class
`provider_busy`. Other `Acquire` errors are store failures and the adapter
returns them. Do not hide store failures by silently calling the provider
anyway; that defeats concurrency and quota protection.

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

Run `healthtest.RunHealthStoreConformance` against a custom `HealthStore`; it
covers the admission and circuit-breaker behavior with a fake clock. A shared
store should also test, across instances with separate connections:

- concurrent `Acquire` calls spread over instances never exceed the provider
  or account limit, and admit exactly one half-open probe between them.
- an instance that stops without releasing its slots frees them after the
  lease TTL.
- a call longer than the lease TTL keeps its slot while its instance runs.
- one instance's snapshot shows another's writes within its refresh interval.
- the snapshot does not expose secret or prompt data.
