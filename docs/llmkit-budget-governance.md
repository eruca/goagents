# llmkit budget governance boundary

## Scope

`llmkit` supports task-level routing cost constraints. It does not own project,
tenant, account, billing-period, invoice, ledger, or quota-governance state.

This boundary keeps `llmkit` as a routing kit instead of a production billing
gateway.

## Current llmkit Budget Capability

The current reusable contract is:

```go
type TaskProfile struct {
    MaxEstimatedCents int `json:"max_estimated_cents,omitempty"`
}
```

`RoutePolicy` applies it as a hard pre-score filter:

```text
if profile.MaxEstimatedCents > 0
and candidate.Model.EstimatedCents > profile.MaxEstimatedCents:
    candidate unavailable: "estimated cost exceeds task budget"
```

Candidate cost can come from:

- static model config: `estimated_cents`
- runtime stats: `ModelStatsEntry.AvgEstimatedCents`

This is enough for a host to express:

- simple task: allow only free or cheap models
- complex task: allow higher cost when success probability matters
- local-first task: set a low task budget and local/cloud privacy preference

## What llmkit Does Not Own

Project/account-level budget governance usually needs:

- project id
- tenant id
- user id
- billing period
- monthly or daily budget
- per-account budget
- model-family budget
- reservation and commit ledger
- refunds and corrections
- admin overrides
- alerting
- provider invoice reconciliation

These are host/product concerns. They should not be added to
`llmkit.TaskProfile`, `Candidate`, `RouteTrace`, or `goagent` core.

## Recommended Host Flow

Hosts should do budget governance before and after llmkit routing:

```text
request
  -> host identifies project/account/user
  -> host checks project/account budget
  -> host chooses task_profile_preset or task_profile
  -> host sets TaskProfile.MaxEstimatedCents
  -> llmkit filters candidates by per-task budget
  -> llmkit records route audit
  -> host optionally reserves estimated budget for the selected route
  -> provider call runs
  -> llmkit records provider outcome
  -> host commits actual or estimated cost to its ledger
```

The important point is that llmkit receives only the routing-relevant budget:

```text
max_estimated_cents
```

The host keeps the full governance context.

## Budget Failure Semantics

Budget failures are not provider failures. They should not be mixed into normal
provider fallback unless the host explicitly chooses a downgrade strategy.

Recommended host-side error codes:

```text
budget_exceeded
budget_unknown
budget_reservation_failed
budget_commit_failed
```

Meaning:

- `budget_exceeded`: the host knows the project/account cannot afford this
  route. Reject the request, ask for approval, or lower `MaxEstimatedCents`.
- `budget_unknown`: the host cannot determine remaining budget. Conservative
  hosts should reject or force local/free candidates.
- `budget_reservation_failed`: do not call the provider. No provider outcome
  should be recorded for a call that did not happen.
- `budget_commit_failed`: the provider call already happened. Record the LLM
  outcome, then put the host ledger into a repair/reconciliation path.

## Downgrade Strategy

If a host wants to continue after a budget failure, it should change the task
profile before routing:

```text
budget exceeded for cloud route
  -> set max_estimated_cents to remaining budget
  -> set privacy to local_preferred or local_only when appropriate
  -> rerun routing
```

This is a host policy decision. `llmkit` should not decide whether a high-risk
task may be downgraded to a cheaper model.

## Reservation Model

Production hosts often need reservation before a provider call:

```text
selected route estimated cost
  -> reserve budget
  -> call provider
  -> commit actual or estimated cost
  -> release unused reservation
```

This currently belongs outside `llmkit`. A host can associate its reservation
with:

- workflow id
- route id
- task id
- account alias
- model alias
- provider

Do not store provider API keys, prompts, responses, headers, or user content in
the budget ledger.

## When To Consider A BudgetGate Interface

Do not add a `BudgetGate` to `llmkit` until at least one real host needs the
same abstraction across multiple workflows.

Signals that extraction may be justified:

- more than one host implements the same pre-route check
- hosts need route-level reservation around the adapter call
- audit needs a stable `budget_decision` field
- route policy needs budget decisions that cannot be expressed by
  `MaxEstimatedCents`

If needed later, the smallest possible shape should be host-injected and
optional:

```go
type BudgetDecision struct {
    Allowed bool
    Reason string
    MaxEstimatedCents int
    RemainingCents int
}

type BudgetGate interface {
    CheckRouteBudget(context.Context, TaskProfile, Candidate) (BudgetDecision, error)
}
```

Reserve/commit/release should remain host-owned until a concrete implementation
proves that `llmkit` needs to coordinate provider execution around it.

## Relationship To Audit

Current llmkit audit records:

- selected route
- task profile
- candidate explanations
- provider outcome
- estimated cents
- business outcome signal

Host budget audit should live alongside this, not inside provider audit:

```text
route-events.jsonl         llmkit route decision
outcomes.jsonl             llmkit provider/business outcome
host-budget-ledger.*       host budget reservation and commit
```

A host can join records by `route_id` and `task_id`.

## Non-Goals

- No `project_id`, `tenant_id`, `user_id`, or `billing_period` in `TaskProfile`.
- No production budget ledger in `llmkit`.
- No invoice reconciliation in `llmkit`.
- No automatic downgrade of high-risk tasks to cheaper models.
- No hidden fallback after budget rejection.
- No provider API keys, prompts, responses, or headers in budget records.

## Verification Checklist

A host budget implementation should test:

- budget exceeded rejects or lowers `MaxEstimatedCents` before routing
- `MaxEstimatedCents` filters expensive candidates
- zero or negative task budget is rejected at the host API boundary
- budget reservation failure does not call the provider
- budget commit failure preserves provider outcome and enters ledger repair
- budget records join to llmkit audit through `route_id` or `task_id`
- budget records do not contain prompts, responses, API keys, or headers
