# llmkit

llmkit is an optional routing module for choosing an LLM model/account from host-provided task metadata. It keeps routing decisions deterministic (unless load spreading is enabled) and auditable while leaving provider clients, API keys, prompts, responses, and business workflow ownership in the host application.

## Configuration Home

`LLMKIT_HOME` points to the directory where a host keeps llmkit runtime configuration and audit files.

- Development mode may fall back to `.llmkit` under the current working directory.
- Production mode requires `LLMKIT_HOME`; fail startup if it is unset.
- Relative values are resolved from the host process working directory.

Example:

```sh
export LLMKIT_HOME=/srv/my-agent/.llmkit
```

Load the host configuration explicitly:

```go
config, err := llmkit.LoadConfigFromEnv(os.Getenv)
if err != nil {
    return err
}

candidates := config.Candidates()
defaultProfile := config.DefaultTaskProfile()
```

`LoadConfigFromEnv` resolves `LLMKIT_HOME` in production mode and reads `config.yaml`. `LoadConfig(home)` is available when a host has already resolved the directory. The loader validates account/model references and rejects plaintext `api_key`; it does not construct provider clients or read secret values from `api_key_env`.

Example `config.yaml`:

```yaml
accounts:
  - alias: local-dev
    provider: openai_compatible
    base_url: http://127.0.0.1:11434/v1
  - alias: cloud-prod
    provider: openai_compatible
    base_url: https://api.example.com/v1
    api_key_env: CLOUD_PROD_API_KEY

models:
  - alias: local-free
    model: qwen2.5:7b
    provider: openai_compatible
    account_alias: local-dev
    is_local: true
    capability_level: simple
    context_window_class: medium
    price_class: free
    latency_class: fast
    estimated_cents: 0
  - alias: cloud-advanced
    model: advanced-model
    provider: openai_compatible
    account_alias: cloud-prod
    capability_level: advanced
    context_window_class: long
    price_class: high
    latency_class: normal
    estimated_cents: 8
```

The host is responsible for failing closed when a configured `api_key_env` is
missing. `examples/host-api` demonstrates that behavior: if `config.yaml`
exists and references an unset secret environment variable, server startup
fails instead of silently routing unauthenticated requests.

## Routing Intent

The default policy applies hard filters first, then ranks eligible candidates by capability, price, locality, latency, recent reliability, and current provider health.

- Reliability is minus the smoothed failure rate in percent, bounded to `[-100, 0]`. Observed outcomes are blended with a 5% prior worth 10 observations, so a model with no history scores -5, a long clean history beats no history, and ten straight failures read as a majority failure rate.
- Latency uses observed average latency from model stats when present (at most 2s is fast, at most 10s is normal, slower is slow) and falls back to the configured `latency_class`.

Simple tasks should be local-first when the profile allows it. For example, low failure-cost classification, short rewriting, formatting, or simple JSON extraction can prefer a free local model with fast latency and low operational cost.

Hard or high failure-cost tasks should route to an advanced model. For example, tasks that need stronger reasoning, long context, tool use, strict JSON, or reviewer-visible correctness should be profiled with `complexity: hard`, `failure_cost: high`, or the relevant `needs_*` flags so local simple models are filtered out or outscored.

Hosts can set `TaskProfile.MaxEstimatedCents` to make cost a hard routing
constraint. Candidates with a known `ModelCapability.EstimatedCents` above that
limit are filtered out before scoring. `ApplyModelStats` also feeds
`AvgEstimatedCents` back into candidates, so observed cost can enforce future
budget constraints. Project/account-level budget governance remains host-owned;
see `../docs/llmkit-budget-governance.md`.

Hosts provide `TaskProfile`; llmkit does not infer business risk from prompt text by itself. If the host does not provide a profile, `DefaultTaskProfile` uses a conservative medium/cloud-allowed baseline.

## Audit Files

`JSONLRecorder` writes two append-only audit files under `LLMKIT_HOME`:

- `route-events.jsonl`: sanitized routing decisions, effective task profile, selected aliases, provider name, score, score breakdown, compact candidate aliases, and full candidate-level score/filter explanations.
- `outcomes.jsonl`: sanitized outcome metadata such as provider success, business outcome signal, error code, latency, token counts, and estimated cents.

Audit records intentionally avoid prompts, responses, headers, and API keys. The recorder also sanitizes key-like strings before writing JSONL.

Hosts can periodically rebuild `model-stats.json` from those audit files:

```go
stats, err := llmkit.RefreshModelStats(llmkitHome)
if err != nil {
    return err
}
_ = stats
```

`model-stats.json` groups records by `task_type`, `account_alias`, `model_alias`, and `provider`. Each bucket includes route attempts, completed outcomes, pending outcomes, success/failure rates, average latency, average token counts, average estimated cents, and last seen time. Outcomes are matched to routes by `route_id` and `attempt`, so every fallback attempt is counted; average latency and cost only include outcomes that reported them. `recent_outcomes` and `recent_failures` weight each outcome by its age, halving every `ModelStatsHalfLife` (7 days); reliability scoring uses these, so a model that failed weeks ago but works now is not held back by its history. The file is derived data: keep `route-events.jsonl` and `outcomes.jsonl` as the append-only source of truth.

Hosts can also expose route-level observability by reading the same audit files:

```go
records, err := llmkit.ReadRouteAudits(llmkitHome, llmkit.AuditFilter{
    TaskID: workflowID,
})
if err != nil {
    return err
}
```

`ReadRouteAudits` joins route traces with matching outcomes by `route_id`.
It returns only the allowlisted audit fields recorded in JSONL; it does not read
or reconstruct prompts, responses, headers, or API keys.
Readers take lines of any length and skip a line that is not valid JSON, such
as one a crash left half written, so one damaged record never makes the audit
or stats unreadable.

`BuildModelStatsFromAudit(now, routes, outcomes)` is the aggregation behind
`BuildModelStats`, for records that come from somewhere other than the JSONL
files. `SanitizeRouteTrace` and `SanitizeTaskOutcome` are the rules
`JSONLRecorder` applies before writing; another `Recorder` should apply them too.

### Shared audit in PostgreSQL

`llmkit/pgaudit.Store` is a `Recorder` that replicas share, with the same read
side as the audit files:

```go
audit, err := pgaudit.Open(ctx, dsn) // or pgaudit.OpenDB(ctx, db)
if err != nil {
    return err
}
defer audit.Close()

records, err := audit.ReadRouteAudits(ctx, llmkit.AuditFilter{TaskID: workflowID})
stats, err := audit.BuildModelStats(ctx)
```

`Open` applies the schema (`llmkit_audit_routes`, `llmkit_audit_outcomes`)
under an advisory lock and refuses a database a newer version has migrated.
Records are sanitized like JSONL ones and stored as JSON documents, with
`task_id`, `route_id` and `attempt` copied into indexed columns. An insertion
sequence stands in for file append order: it orders `ReadRouteAudits` results
and picks the latest outcome for a `route_id` and `attempt`. For the same
records written in the same order, `ReadRouteAudits` and `BuildModelStats`
return what `llmkit.ReadRouteAudits` and `llmkit.BuildModelStats` return for a
`JSONLRecorder`.

Known limitation: like the file version, `BuildModelStats` reads every record
and recomputes the stats on each call. Hosts that consult it often should
cache the result (as `model-stats.json` does for files). Tables only grow;
pruning old records is up to the host.

Its tests use the same `LLMKIT_POSTGRES_TEST_DSN` and `LLMKIT_REQUIRE_POSTGRES`
as `llmkit/pghealth`.

To make routing use this history, load the generated stats and pass them to the adapter:

```go
modelStats, err := llmkit.LoadModelStats(llmkitHome)
if err != nil {
    return err
}

client := goagentadapter.NewClient(goagentadapter.Config{
    Candidates: config.Candidates(),
    Providers:  providers,
    ModelStats: modelStats,
})
```

The adapter applies stats after it receives the current `TaskProfile`, so history is task-type aware. Without `ModelStats`, routing behavior is unchanged.

Long-running hosts can provide fresh stats for every route decision:

```go
client := goagentadapter.NewClient(goagentadapter.Config{
    Candidates: config.Candidates(),
    Providers:  providers,
    ModelStatsProvider: func(ctx context.Context) (*llmkit.ModelStats, error) {
        return llmkit.RefreshModelStats(llmkitHome)
    },
})
```

This keeps newly written outcomes useful without waiting for a process restart.

## Provider Health

`MemoryHealthStore` tracks current provider/account/model runtime state:

- in-flight calls and concurrency ceilings
- quota exhaustion
- failure streaks, counting only failures attributable to the provider; a
  cancelled, oversized, or over-budget request does not degrade it
- degraded or unavailable providers
- cooldown windows after repeated failures; each failure past the threshold,
  such as a failed recovery probe, doubles the cooldown up to
  `HealthPolicy.MaxCooldownDuration` (default 32x `CooldownDuration`)

Hosts can keep this in memory for a single process, or replace the interface
with a durable/shared implementation later. The route policy treats quota
exhaustion, active cooldown, unavailable providers, and full provider or
account concurrency (including `max_concurrency` from config) as hard filters.
Degraded providers remain eligible but receive a lower score, so they are used
only when better options are unavailable. After a cooldown expires, an
unavailable provider is half-open: exactly one probe call tests recovery while
the others wait for its outcome.

Typical adapter wiring:

```go
health := llmkit.NewMemoryHealthStore(llmkit.HealthPolicy{
    FailureCooldownThreshold: 3,
    CooldownDuration:         30 * time.Second,
})

client := goagentadapter.NewClient(goagentadapter.Config{
    Candidates:   config.Candidates(),
    Providers:    providers,
    HealthStore:  health,
    ModelStats:   modelStats,
    Recorder:     recorder,
})
```

The adapter applies `HealthStore.Snapshot()` before each routing decision.
Because the snapshot can be stale under concurrency, it then calls `Acquire`,
which checks the same limits atomically and takes the slot; a candidate that
turns out busy is skipped without spending a fallback attempt. After the call
the adapter records the outcome, or `Release`s the slot if the provider was
never called. This keeps simple tasks local-first when local is
healthy, while allowing fallback to cloud when the local provider is busy,
quota-exhausted, or cooling down after failures.

`MemoryHealthStore` is intentionally a single-process implementation. For
replicas that share providers, use `llmkit/pghealth` (below). The shared-store
contract is documented in `../docs/llmkit-healthstore-contract.md`; a custom
store can run the same behavioral checks as the built-in ones with
`healthtest.RunHealthStoreConformance`. Stores share the rules themselves
through `llmkit.EvaluateAdmission`, `llmkit.ApplyHealthOutcome`, and
`llmkit.SettleHealthEntry`, which `MemoryHealthStore` also uses.

### Shared health in PostgreSQL

`llmkit/pghealth.Store` is a `HealthStore` that replicas share, so provider and
account concurrency limits, the single half-open probe, and the circuit breaker
hold across all of them:

```go
health, err := pghealth.Open(ctx, dsn, pghealth.Config{
    Policy:          llmkit.HealthPolicy{FailureCooldownThreshold: 3, CooldownDuration: 30 * time.Second},
    LeaseTTL:        30 * time.Second, // default
    RefreshInterval: time.Second,      // default
})
if err != nil {
    return err
}
defer health.Close()
```

`Open` (or `OpenDB` on a pool the caller keeps owning) applies the schema
(`llmkit_provider_health`, `llmkit_provider_health_leases`) under an advisory
lock and starts the background work; `Close` stops it.

- **Admission.** `Acquire` runs in one transaction under an advisory lock on
  the account, counts unexpired leases as the in-flight calls, applies
  `llmkit.EvaluateAdmission`, and takes a lease for an admitted call.
- **Leases.** `Release` and `RecordOutcome` delete the oldest lease this Store
  holds for the key; `RecordOutcome` applies `llmkit.ApplyHealthOutcome` in the
  same transaction. Each Store renews its leases every `LeaseTTL/3`, so a long
  call keeps its slot. A crashed or closed replica's leases stop being renewed
  and free their slots within `LeaseTTL`; `Close` does not release them.
- **Snapshot.** `Snapshot()` serves a view cached in memory, reloaded every
  `RefreshInterval` and after each of this Store's writes. Other replicas'
  writes show up within `RefreshInterval`; that is enough because the snapshot
  only pre-filters and `Acquire` is the authoritative check.
- **Host-set state.** `Store.Set(ctx, entry)` sets a provider's concurrency
  limit, quota, or availability, like `MemoryHealthStore.Set`.
- **Clocks.** Times come from `Policy.Now` (the wall clock by default) on each
  replica and are compared with times other replicas wrote. Keep replicas'
  clocks within a small fraction of `LeaseTTL/3` of each other (NTP): skew
  shortens or lengthens leases and shifts when a cooldown ends by as much.

Its tests need PostgreSQL: set `LLMKIT_POSTGRES_TEST_DSN`, and
`LLMKIT_REQUIRE_POSTGRES=1` to fail instead of skipping without it.

### Load Spread

By default `RoutePolicy.Select` is deterministic: equal inputs always choose
the same top candidate, so concurrent requests all go to it until its
concurrency limit fills. Setting `RoutePolicy.LoadSpread` trades that
reproducibility for load spreading. Among the candidates scoring within
`ScoreTolerance` of the best, it draws two at random and keeps the one with
fewer requests in flight (power of two choices). In-flight counts come from the
health snapshot, so without a `HealthStore` the choice is uniform among the
near-best candidates. The decision's `Reason` names both draws and their load,
so route traces still explain each choice.

```go
client := goagentadapter.NewClient(goagentadapter.Config{
	// ...
	Policy: llmkit.RoutePolicy{LoadSpread: &llmkit.LoadSpread{ScoreTolerance: 5}},
})
```

## Fallback Policy

When a selected provider fails, the adapter removes that candidate and asks the
policy to select the next best provider-backed candidate. Each attempt records a
route trace with an incremented attempt number and, when enabled, a provider
outcome.

Hosts can cap fallback attempts:

```go
client := goagentadapter.NewClient(goagentadapter.Config{
    Candidates: config.Candidates(),
    Providers:  providers,
    FallbackPolicy: goagentadapter.FallbackPolicy{
        MaxAttempts: 2,
    },
})
```

`MaxAttempts <= 0` preserves the default behavior: try all remaining eligible
provider-backed candidates.

Reliability-sensitive runtimes should fail closed and allowlist only errors
that are known to occur before dispatch:

```go
client := goagentadapter.NewClient(goagentadapter.Config{
    Candidates: config.Candidates(),
    Providers:  providers,
    FallbackPolicy: goagentadapter.FallbackPolicy{
        FailClosed: true,
        RetryableErrorClasses: []llmkit.ErrorClass{
            llmkit.ErrorClassTransient,
            llmkit.ErrorClassTimeout,
        },
    },
})
```

A fail-closed client never falls back when the provider error is dispatched or
its dispatch status is unknown. Setting `Config.PreDispatch` (a typed host
claim boundary run before each provider call) implies `FailClosed`.

Hosts can also classify provider errors for audit and future typed fallback
rules:

```go
client := goagentadapter.NewClient(goagentadapter.Config{
    Candidates: config.Candidates(),
    Providers:  providers,
    ErrorClassifier: func(err error) llmkit.ErrorClass {
        return llmkit.ErrorClassTimeout
    },
})
```

Without `ErrorClassifier`, provider failures keep the existing
`error_code=provider_error` outcome and fallback behavior. With a classifier,
failed outcomes also record `error_class`, such as `timeout` or
`rate_limited`.

The typed fallback contract is documented in
`../docs/plans/2026-05-07-llmkit-host-contract-followups-design.md`.

## Automatic-test quality cascade

The optional `adapters/goagent.CascadeExecutor` registers versioned tasks and
host-owned automatic verifiers. Existing `Client`, `Config`, `TaskProfile`,
`ModelStats`, and their fallback behavior remain unchanged.

Register each task as a `CascadeTask` with a `TaskKey` (type, task version,
frozen test-suite version), explicit `TaskProfile`, and `Verify` callback.
The callback receives the original request and generated response: return
`true, nil` only when the acceptance tests pass, `false, nil` when they fail,
and an error when the test infrastructure cannot determine a result.
Define tests independently of the model output. A schema check alone cannot
establish semantic correctness. Execute generated code only in a host-owned
sandbox; the library never executes generated code or tools.

Register models as `CascadeModel` values, binding candidate/account/provider
metadata to an immutable `Revision` and its provider client. The revision must
cover the actual model, prompt and generation settings. The host must change it
when those settings change; the library cannot detect silent provider updates.

Run `Evaluate` explicitly against the same caller-owned frozen corpus for each
model. It produces a `QualityEvidence` record with test passed/sample counts,
task/test/model versions, account/provider identity and an expiry. An invocation
of this method using real clients makes real provider calls. Evidence persistence
and reload are host-owned; pass the resulting records as `CascadeConfig.Evidence`
when constructing the executor. No background evaluation or implicit promotion
from live traffic occurs.

`QualityPolicy` requires explicit `MinSamples`, `MaxAttempts` and cumulative
`MaxEstimatedCents`. Qualification requires observed test success **strictly
greater than `MinSuccessRate`** (zero means `DefaultMinSuccessRate`, 85%),
sufficient samples, matching versions/identity, and unexpired evidence. A rate
equal to the threshold, missing evidence, and version mismatches are rejected.
If local health state (busy, cooldown, quota) keeps a request from being sent,
`Execute` and `Evaluate` return `ErrCascadeUnavailable`: `Evaluate` publishes
no evidence rather than counting the case as failed, and `Execute` charges no
estimated cost for it.
This is an observed proportion, not a statistical confidence bound. Choose a
representative corpus and sample minimum for the task's risk; improving success
on the full corpus does not guarantee success on the earlier model's failures.

`Execute` first applies existing capability, privacy, quota, cooldown and
concurrency filters, then starts with the cheapest qualified model. Cost ties
prefer greater test success, then a stable alias/account/provider order.
Failed acceptance tests may escalate only to a remaining model with a strictly
greater measured success rate and no lower configured cost. Passing tests stop
immediately. Provider errors, cancellation, broken verifiers and audit failures
stop without quality escalation. Requests offering tools and responses issuing
tool calls are rejected by this first generation-only executor; tool side
effects remain host-owned.

Costs retain the existing integer-cent estimate: configure conservative per-call
estimates, rounding positive sub-cent costs up. Zero is eligible only with
`PriceFree`; a zero cumulative budget admits only free models. A prospective
call exceeding the remaining estimated budget is never dispatched. Evaluation
also checks its full-corpus estimate before dispatch. These checks cap reserved
estimates, not invoices or actual token billing; account-level actual-cost
governance remains host-owned.

Health accounting continues to use provider transport outcomes. Audit
`BusinessOutcome` and `SuccessSignalTestsPassed` describe acceptance separately;
old `ModelStats.SuccessRate` remains a transport statistic and is never used
as quality qualification. `CascadeResult` returns the task key, versioned model
attempts, estimated cost and only a test-accepted response. Hosts can persist
this metadata with the versioned evidence without logging input/output text.
Verifier errors are content-free. Host verifiers, providers,
recorders and clocks must honor context and be concurrency-safe; configured
provider limits and deadlines remain host-owned.

Run the complete fixture-only flow from the repository root:

```sh
go run ./llmkit/examples/quality-cascade
go test ./llmkit/...
LLMKIT_POSTGRES_TEST_DSN='postgres://...' go test -p 1 ./llmkit/pghealth ./llmkit/pgaudit
```

The example evaluates three deterministic arithmetic fixtures, audits the
verifier, and reports rescue rates and held-out cascade results.
It uses no credentials or network and makes no claims about real LLM quality.

### Paired rescue evidence and verifier checks

`AuditVerifier` checks a registered verifier against caller-labeled positive
and negative responses. Both categories are required. False acceptance or false
rejection returns `ErrVerifierRejected` with the counts; verifier errors/panics
return `ErrCascadeVerifier` without publishable partial evidence. Production
acceptance uses the same error/panic boundary. Labels must come from an
independent trusted oracle, not the model being evaluated. Real knowledge tasks
need counterexamples for omitted conditions, unsupported claims and incorrect
source references in addition to malformed output.

`EvaluateCascade` takes a `CascadeEvaluationSet` with qualification cases,
disjoint validation cases and verifier cases, plus explicit expiry, total
evaluation budget and `RescuePolicy`. It checks case IDs and exact request
overlap before any provider call, audits the verifier, and runs each eligible
model on the same qualification corpus. Canonical SHA-256 binds evidence to
the corpus. Per-model automatic-test counts produce `QualityEvidence`;
each ordered model pair produces `RescueEvidence`: failed baseline cases,
cases rescued by the next model, and the conditional success rate. Zero failed
baseline cases means rescue ability is unmeasured, not 100%.

Enable paired gating with `CascadeConfig.RescuePolicy` and
`RescueEvidence`. Upgrades must retain the existing greater-overall-success
and nondecreasing-cost rules, and additionally satisfy the explicit rescue
sample/rate minimum. Evidence must match task/test/model/account/provider
versions, corpus, denominator and expiry. Missing or stale paired evidence
blocks an upgrade. Cost still ranks first; equal-cost upgrades prefer greater
rescue success. Without the optional rescue policy, overall-rate routing is
unchanged. Persist/reload quality and paired evidence together.

After qualification, the evaluator invokes the actual cascade on the held-out
requests, recording solved/failed counts, attempts, estimated costs, cost per
solved task, case error categories and p50/p95 end-to-end cascade latency.
Verifiers or audit failures abort the evaluation; ordinary unsolved/provider
failures remain explicit failed cases. The evaluation budget reserves the full
matrix and worst permitted validation calls before dispatch. The report
distinguishes total evaluation estimates from validation-chain estimates.
Reports contain IDs, versions, hashes and metrics, never request/response text.
Pairwise rescue rates do not establish rescue on every multi-stage residual
intersection; the held-out chain report measures that combined behavior.
Disjoint exact requests alone do not establish representative or semantically
independent samples; corpus design and trusted labels remain host-owned.

The executable example now emits this full report using arithmetic fixtures:
overall rates 90/100, 98/100 and 96/100; rescue on the first model's failures
80% and 100%. With a fixture rescue minimum of 90%, the 98% model is skipped.
The held-out 20-case run solves 20/20 with 22 attempts and 4 simulated cents;
qualification plus validation calls total 304 simulated cents in estimates.
These are fixture outcomes, not measurements of real providers or billing.

## API Keys

Do not store plaintext API keys in llmkit config files or audit files. Configuration should reference secret material through environment variable names such as `api_key_env: OPENAI_API_KEY`, account aliases, or a host-owned secret store.

Recommended rules:

- Store only `alias`, `account_alias`, provider metadata, capability labels, and `api_key_env` names in config.
- Resolve the actual key in the host/provider client at request time.
- Keep provider clients responsible for transport headers and credential loading.
- Do not copy keys into `TaskProfile`, `Candidate`, `RouteTrace`, `TaskOutcome`, logs, or errors.

## goagent Adapter

The adapter in `adapters/goagent` implements `github.com/eruca/goagents/goagent/ports.LLMClient`.

Typical usage:

```go
recorder, err := llmkit.NewJSONLRecorder(llmkitHome)
if err != nil {
    return err
}

providers, err := goagentadapter.OpenAICompatibleProvidersFromConfig(*config, os.Getenv, nil)
if err != nil {
    return err
}

client := goagentadapter.NewClient(goagentadapter.Config{
    Candidates: config.Candidates(),
    Providers:  providers,
    ProfileProvider: func(ctx context.Context, req ports.ChatRequest) llmkit.TaskProfile {
        profile := config.DefaultTaskProfile()
        profile.Source = llmkit.ProfileSourceHost
        profile.TaskType = "answer"
        profile.Complexity = llmkit.ComplexityHard
        profile.FailureCost = llmkit.FailureCostHigh
        profile.NeedsReasoning = true
        return profile
    },
    RouteMetadataProvider: func(ctx context.Context, req ports.ChatRequest) goagentadapter.RouteMetadata {
        return goagentadapter.RouteMetadata{
            RouteID: routeID,
            TaskID:  taskID,
            Attempt: attempt,
        }
    },
    Recorder: recorder,
})
```

Import aliases are host-owned; one common pattern is:

```go
import (
    goagentadapter "github.com/eruca/goagents/llmkit/adapters/goagent"
    "github.com/eruca/goagents/llmkit"
)
```

The provider map keys must match `Candidate.Model.Alias`. Missing provider-backed candidates are skipped before routing.

`OpenAICompatibleProvidersFromConfig` supports `provider: openai`, `provider: openai_compatible`, and local OpenAI-compatible servers. It uses account `base_url`, model `model`, and account `api_key_env`; if `model` is omitted, the model alias is used as the provider model id. Passing `nil` as the HTTP client uses the default client.

Its `limits` argument applies the same `openaiapi.Limits` to every provider it
builds, and `ports.ChatRequest.MaxOutputTokens` reaches the selected provider
unchanged. Zero values disable a bound; limits are never silently inferred from
an application profile.

The adapter implements `ports.StreamingLLMClient`, so `Agent.Stream` receives model output as it is generated. `ChatStream` streams through providers that implement `ports.StreamingLLMClient`, such as `openaiapi`, and delivers any other provider's complete response as one delta. An attempt that has already delivered output never falls back to another candidate, so a consumer never sees two answers spliced together. With a pre-dispatch hook, streaming providers must implement `ProviderStreamRequestDigester` so the claim covers the exact streaming request body.

When a selected provider fails, the adapter removes that candidate and asks the policy to select the next best provider-backed candidate. Each attempted route is recorded with an incremented `attempt` value when a recorder is configured.

See `examples/goagent-routing` for a minimal host-style example that loads `LLMKIT_HOME/config.yaml`, builds OpenAI-compatible providers, wires the goagent adapter, runs one request, and writes `route-events.jsonl` plus `outcomes.jsonl`.

## Host API Composition Example

`examples/host-api` shows a fuller host composition. It is not a new core
module; it combines `workflowkit`, `artifactkit`, `runkit`, `goagent`, and
`llmkit` behind HTTP endpoints.

Important behavior:

- If `LLMKIT_HOME/config.yaml` is absent, the example uses deterministic static
  demo providers so tests and walkthroughs run without network access.
- If `config.yaml` exists, host-api builds candidates and OpenAI-compatible
  providers from that file.
- Startup validates and initializes `model-stats.json`. During runtime,
  host-api refreshes stats before each route decision through
  `ModelStatsProvider`, and refreshes again when serving `/llmkit/models`.
- `GET /workflows/{id}/llm-routes` exposes route audit for a workflow.
- `GET /llmkit/models` exposes configured models, current health snapshot, and
  model stats used for history-aware routing.

See `../docs/host-api-contract.md` for the prose endpoint contract and
`../examples/host-api/openapi.yaml` for the machine-readable contract.

## Testing

```sh
go test ./llmkit/...
```

The core `llmkit` package does not import `goagent`; only
`llmkit/adapters/goagent` does. `internal/boundary` enforces this.
