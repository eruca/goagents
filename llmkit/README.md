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

See `examples/config/.llmkit/config.yaml` for a configuration shape that uses only aliases and environment variable names.

## Routing Intent

The default policy applies hard filters first, then ranks eligible candidates by capability, price, locality, latency, and recent reliability.

Simple tasks should be local-first when the profile allows it. For example, low failure-cost classification, short rewriting, formatting, or simple JSON extraction can prefer a free local model with fast latency and low operational cost.

Hard or high failure-cost tasks should route to an advanced model. For example, tasks that need stronger reasoning, long context, tool use, strict JSON, or reviewer-visible correctness should be profiled with `complexity: hard`, `failure_cost: high`, or the relevant `needs_*` flags so local simple models are filtered out or outscored.

Hosts provide `TaskProfile`; llmkit does not infer business risk from prompt text by itself. If the host does not provide a profile, `DefaultTaskProfile` uses a conservative medium/cloud-allowed baseline.

## Audit Files

`JSONLRecorder` writes two append-only audit files under `LLMKIT_HOME`:

- `route-events.jsonl`: sanitized routing decisions, selected aliases, provider name, score, score breakdown, and candidate aliases.
- `outcomes.jsonl`: sanitized outcome metadata such as success, error code, latency, token counts, and estimated cents.

Audit records intentionally avoid prompts, responses, headers, and API keys. The recorder also sanitizes key-like strings before writing JSONL.

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

client := goagentadapter.NewClient(goagentadapter.Config{
    Candidates: candidates,
    Providers: map[string]goagentadapter.ProviderClient{
        "local-free":     localProvider,
        "cloud-advanced": cloudProvider,
    },
    ProfileProvider: func(ctx context.Context, req ports.ChatRequest) llmkit.TaskProfile {
        profile := llmkit.DefaultTaskProfile()
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
