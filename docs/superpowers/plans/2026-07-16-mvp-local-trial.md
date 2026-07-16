# GoAgents MVP Local Trial Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add and execute an opt-in real Host × real Qwen local trial without changing product runtime code.

**Architecture:** A Darwin/CGO tagged test reuses the existing real-process, loopback OIDC, Keychain isolation, and HTTP contract helpers. It injects a one-model llmkit configuration from caller-owned environment variables, drives the workflow through a persisted approval boundary and two Host restarts, then verifies workflow, run, route, and event evidence.

**Tech Stack:** Go 1.24, `testing`, real `host-api` binary, SQLite, macOS Keychain, loopback OIDC, llmkit OpenAI-compatible provider.

## Global Constraints

- Do not modify product runtime code.
- Do not write or log the real endpoint, API key, bearer token, Prompt, model response, or raw Provider error.
- Missing Provider configuration or inaccessible login Keychain is `SKIP/blocked`, never PASS.
- Use only a unique `goagents.host-api.approvals.smoke.` Keychain service and exact-item cleanup.
- Keep the gate out of default CI by requiring both `hostapisystemsmoke` and `provideracceptance` tags.

---

### Task 1: Real Host × Provider trial gate

**Files:**
- Create: `examples/host-api/host_real_provider_trial_test.go`

**Interfaces:**
- Consumes: `requireRealProviderConfig`, `newOIDCTestProvider`, `buildHostBinary`, `startHostProcessWithEnv`, `processJSON`, `stopHostProcess`, `smokeKeychainCleanup`, `assertMVPCompletedWorkflow`.
- Produces: `TestHostAPIProcessRealProviderLocalTrial` and `writeHostRealProviderTrialConfig`.

- [ ] **Step 1: Add the tagged acceptance test**

Create a test with build tag:

```go
//go:build darwin && cgo && hostapisystemsmoke && provideracceptance
```

The test must perform these exact transitions:

```text
POST /workflows -> waiting_approval
POST /workflows/{id}/approve with invalid token -> 401, state unchanged
stop -> restart -> GET persisted waiting_approval
POST /workflows/{id}/approve with valid token -> succeeded
stop -> restart -> workflow/run/route/events remain queryable
```

`writeHostRealProviderTrialConfig` must write one `openai_compatible` account and one advanced model alias named `qwen-local-trial`; `api_key_env` must be `OPENAI_COMPAT_API_KEY`, while endpoint and model are read at runtime and never logged.

- [ ] **Step 2: Verify the missing-configuration path**

Run:

```bash
cd examples/host-api
env -u OPENAI_COMPAT_BASE_URL -u OPENAI_COMPAT_MODEL -u OPENAI_COMPAT_API_KEY \
  go test -v -tags 'hostapisystemsmoke provideracceptance' \
  -run '^TestHostAPIProcessRealProviderLocalTrial$' -count=1 ./...
```

Expected: compile succeeds and the test explicitly reports `SKIP` because configuration is missing.

- [ ] **Step 3: Run the real local trial**

Map the existing local Qwen values into the three `OPENAI_COMPAT_*` variables without printing them, then run the same tagged command. Expected: PASS with no SKIP, one successful real Provider route, 401 for the invalid approval, succeeded after valid approval, and persisted evidence after the second restart.

- [ ] **Step 4: Run race and static checks**

Run the real trial with `go test -race`, then run:

```bash
go vet -tags 'hostapisystemsmoke provideracceptance' ./...
go test ./... -count=1
git diff --check
```

Expected: all commands exit 0.

- [ ] **Step 5: Commit the gate**

```bash
git add examples/host-api/host_real_provider_trial_test.go
git commit -m 'test(host-api): 增加真实 Host 本机试用门禁'
```

### Task 2: Runbook, evidence, and release decision

**Files:**
- Modify: `examples/host-api/README.md`
- Create: `docs/superpowers/specs/2026-07-16-mvp-local-trial.md`
- Modify: `docs/superpowers/specs/2026-07-15-mvp-final-acceptance.md`

**Interfaces:**
- Consumes: the exact tagged command and evidence from Task 1.
- Produces: a reproducible operator runbook and a P0/P1 plus `v0.1.0` readiness decision.

- [ ] **Step 1: Document the opt-in command and boundary**

Add a Host README section that names all three `OPENAI_COMPAT_*` variables, the two required build tags, the exact test name, and the rule that `SKIP` is blocked. State that the test uses a real Provider but loopback OIDC and a temporary Keychain item; it is not a production IdP test.

- [ ] **Step 2: Record observed evidence**

Create the local-trial record with the commit baseline, command, PASS/SKIP status, workflow transitions, restart evidence, sensitive-output check, and any observed defect classified as P0-P3. Do not include credentials, endpoint, model response, token, or machine-local temporary paths.

- [ ] **Step 3: Update the final acceptance record**

Link the trial record and state whether the trial introduced any P0/P1. Keep the existing two P2 items and exactly-once boundary unchanged unless the run supplies contrary evidence.

- [ ] **Step 4: Run final verification**

Run:

```bash
bash scripts/verify-all.sh
go test -v -tags 'hostapisystemsmoke provideracceptance' \
  -run '^TestHostAPIProcessRealProviderLocalTrial$' -count=1 ./...
git diff --check
```

Also scan changed files for the actual endpoint and API key without printing either value. Expected: workspace PASS, real trial PASS without SKIP, no sensitive match, and a clean diff check.

- [ ] **Step 5: Commit the runbook and evidence**

```bash
git add examples/host-api/README.md \
  docs/superpowers/specs/2026-07-15-mvp-final-acceptance.md \
  docs/superpowers/specs/2026-07-16-mvp-local-trial.md
git commit -m 'docs(mvp): 记录真实 Host 本机试用'
```

- [ ] **Step 6: Merge locally after an independent diff review**

Require Critical=0 and Important=0, fast-forward the branch into `main`, rerun the workspace and tagged trial from `main`, then remove the worktree and branch. Do not create or push a tag in this task; only record whether the checkout is ready for a separate multi-module tag review.
