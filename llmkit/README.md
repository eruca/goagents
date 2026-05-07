# llmkit

llmkit is an optional routing module for choosing an LLM model/account from host-provided task metadata. It keeps routing decisions deterministic and auditable while leaving provider clients, API keys, prompts, responses, and business workflow ownership in the host application.

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

Simple tasks should be local-first when the profile allows it. For example, low failure-cost classification, short rewriting, formatting, or simple JSON extraction can prefer a free local model with fast latency and low operational cost.

Hard or high failure-cost tasks should route to an advanced model. For example, tasks that need stronger reasoning, long context, tool use, strict JSON, or reviewer-visible correctness should be profiled with `complexity: hard`, `failure_cost: high`, or the relevant `needs_*` flags so local simple models are filtered out or outscored.

Hosts can set `TaskProfile.MaxEstimatedCents` to make cost a hard routing
constraint. Candidates with a known `ModelCapability.EstimatedCents` above that
limit are filtered out before scoring. `ApplyModelStats` also feeds
`AvgEstimatedCents` back into candidates, so observed cost can enforce future
budget constraints.

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

`model-stats.json` groups records by `task_type`, `account_alias`, `model_alias`, and `provider`. Each bucket includes route attempts, completed outcomes, pending outcomes, success/failure rates, average latency, average token counts, average estimated cents, and last seen time. The file is derived data: keep `route-events.jsonl` and `outcomes.jsonl` as the append-only source of truth.

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
- failure streaks
- degraded or unavailable providers
- cooldown windows after repeated failures

Hosts can keep this in memory for a single process, or replace the interface
with a durable/shared implementation later. The route policy treats quota
exhaustion, active cooldown, unavailable providers, and full provider
concurrency as hard filters. Degraded providers remain eligible but receive a
lower score, so they are used only when better options are unavailable.

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

The adapter applies `HealthStore.Snapshot()` before each routing decision,
calls `Begin` before invoking the selected provider, and calls `RecordOutcome`
after success or failure. This keeps simple tasks local-first when local is
healthy, while allowing fallback to cloud when the local provider is busy,
quota-exhausted, or cooling down after failures.

`MemoryHealthStore` is intentionally a single-process implementation. The
shared-store contract and future host replacement path are documented in
`../docs/plans/2026-05-07-llmkit-host-contract-followups-design.md`.

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

The planned typed fallback contract is documented in
`../docs/plans/2026-05-07-llmkit-host-contract-followups-design.md`.

## API Keys

Do not store plaintext API keys in llmkit config files or audit files. Configuration should reference secret material through environment variable names such as `api_key_env: OPENAI_API_KEY`, account aliases, or a host-owned secret store.

Recommended rules:

- Store only `alias`, `account_alias`, provider metadata, capability labels, and `api_key_env` names in config.
- Resolve the actual key in the host/provider client at request time.
- Keep provider clients responsible for transport headers and credential loading.
- Do not copy keys into `TaskProfile`, `Candidate`, `RouteTrace`, `TaskOutcome`, logs, or errors.

## goagent Adapter

The adapter in `adapters/goagent` implements `github.com/eruca/goagent/ports.LLMClient`.

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
    goagentadapter "github.com/eruca/llmkit/adapters/goagent"
    "github.com/eruca/llmkit/llmkit"
)
```

The provider map keys must match `Candidate.Model.Alias`. Missing provider-backed candidates are skipped before routing.

`OpenAICompatibleProvidersFromConfig` supports `provider: openai`, `provider: openai_compatible`, and local OpenAI-compatible servers. It uses account `base_url`, model `model`, and account `api_key_env`; if `model` is omitted, the model alias is used as the provider model id. Passing `nil` as the HTTP client uses the default client.

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

## Independent Testing

Run llmkit tests from the module directory:

```sh
cd llmkit
go test ./...
```

For release-style checks, use `GOWORK=off` to test llmkit as an independent module:

```sh
cd llmkit
GOWORK=off go test ./...
```

If the module still depends on a local `replace` such as `github.com/eruca/goagent => ../goagent`, `GOWORK=off` will still need that relative path to exist. Before publishing or testing from a clean checkout, either remove the replace after the dependency is released or provide the matching local module path.
