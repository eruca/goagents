# llmkit Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a new isolated `llmkit` module that routes LLM calls by task profile, model capability, cost, latency, concurrency, and historical failures while keeping `goagent` core unchanged.

**Architecture:** `llmkit` is a sibling module in the `goagents` workspace. Its core package owns configuration, task profiles, route decisions, audit records, and deterministic routing. A separate adapter package exposes the router as `github.com/eruca/goagent/ports.LLMClient`.

**Tech Stack:** Go modules, standard library JSON/YAML parsing where practical, `github.com/eruca/goagent/ports` only in the adapter package, JSONL audit files under `LLMKIT_HOME`.

---

### Task 1: Create Module Skeleton

**Files:**
- Create: `llmkit/go.mod`
- Create: `llmkit/llmkit/doc.go`
- Modify: `go.work`
- Modify: `docs/modules.md`

**Step 1: Write module boundary docs**

Create `llmkit/llmkit/doc.go` with package documentation explaining that the package routes model calls and does not store API keys.

**Step 2: Create the module**

Run:

```bash
mkdir -p llmkit/llmkit
cd llmkit
go mod init github.com/eruca/llmkit
```

Expected: `llmkit/go.mod` exists.

**Step 3: Add the module to the workspace**

Run:

```bash
go work use ./llmkit
```

Expected: root `go.work` includes `./llmkit`.

**Step 4: Update module documentation**

Add `github.com/eruca/llmkit` to `docs/modules.md` as an optional adapter/capability module. State that `goagent` must not import `llmkit`.

**Step 5: Verify**

Run:

```bash
(cd llmkit && go test ./...)
```

Expected: PASS.

**Step 6: Commit**

```bash
git add llmkit/go.mod llmkit/llmkit/doc.go go.work docs/modules.md
git commit -m "chore(llmkit): 新增模块骨架"
```

### Task 2: Define Task Profile And Model Capability Types

**Files:**
- Create: `llmkit/llmkit/profile.go`
- Create: `llmkit/llmkit/capability.go`
- Create: `llmkit/llmkit/profile_test.go`

**Step 1: Write tests**

Add tests for default `TaskProfile`, capability matching, and local-only filtering semantics.

**Step 2: Implement types**

Define:

```go
type Complexity string
type LatencyRequirement string
type FailureCost string
type PrivacyLevel string
type TaskProfile struct { ... }
type ModelCapability struct { ... }
```

Include fields for JSON, tools, long context, local preference, price class, latency class, and concurrency.

**Step 3: Verify**

Run:

```bash
(cd llmkit && go test ./llmkit -run 'TestTaskProfile|TestModelCapability' -v)
```

Expected: PASS.

**Step 4: Commit**

```bash
git add llmkit/llmkit/profile.go llmkit/llmkit/capability.go llmkit/llmkit/profile_test.go
git commit -m "feat(llmkit): 定义任务画像和模型能力"
```

### Task 3: Implement Environment Home Resolution

**Files:**
- Create: `llmkit/llmkit/home.go`
- Create: `llmkit/llmkit/home_test.go`

**Step 1: Write tests**

Cover:

- `LLMKIT_HOME` wins.
- missing `LLMKIT_HOME` can use `.llmkit` in development mode.
- production mode requires explicit `LLMKIT_HOME`.

**Step 2: Implement resolver**

Expose:

```go
type HomeMode string
func ResolveHome(cwd string, getenv func(string) string, mode HomeMode) (string, error)
```

**Step 3: Verify**

Run:

```bash
(cd llmkit && go test ./llmkit -run TestResolveHome -v)
```

Expected: PASS.

**Step 4: Commit**

```bash
git add llmkit/llmkit/home.go llmkit/llmkit/home_test.go
git commit -m "feat(llmkit): 支持 LLMKIT_HOME 工作目录"
```

### Task 4: Add Route Policy

**Files:**
- Create: `llmkit/llmkit/policy.go`
- Create: `llmkit/llmkit/policy_test.go`

**Step 1: Write routing tests**

Cover:

- simple task with no latency requirement selects local free model.
- hard task with high failure cost selects advanced model.
- JSON task excludes models without JSON support.
- tool task excludes models without tool support.
- rate-limited or concurrency-full accounts are skipped.

**Step 2: Implement deterministic policy**

Implement:

```go
type Candidate struct { ... }
type RouteDecision struct { ... }
type RoutePolicy struct { ... }
func (p RoutePolicy) Select(profile TaskProfile, candidates []Candidate) (RouteDecision, error)
```

Use filter-then-sort. Keep score breakdown explicit.

**Step 3: Verify**

Run:

```bash
(cd llmkit && go test ./llmkit -run TestRoutePolicy -v)
```

Expected: PASS.

**Step 4: Commit**

```bash
git add llmkit/llmkit/policy.go llmkit/llmkit/policy_test.go
git commit -m "feat(llmkit): 实现确定性路由策略"
```

### Task 5: Add Audit Recorder

**Files:**
- Create: `llmkit/llmkit/audit.go`
- Create: `llmkit/llmkit/audit_test.go`

**Step 1: Write tests**

Cover JSONL append for:

- route event.
- task outcome.
- no API key fields written.

**Step 2: Implement recorder**

Expose:

```go
type RouteTrace struct { ... }
type TaskOutcome struct { ... }
type Recorder interface { RecordRoute(context.Context, RouteTrace) error; RecordOutcome(context.Context, TaskOutcome) error }
type JSONLRecorder struct { ... }
```

**Step 3: Verify**

Run:

```bash
(cd llmkit && go test ./llmkit -run TestJSONLRecorder -v)
```

Expected: PASS.

**Step 4: Commit**

```bash
git add llmkit/llmkit/audit.go llmkit/llmkit/audit_test.go
git commit -m "feat(llmkit): 记录路由审计和任务结果"
```

### Task 6: Add goagent Adapter

**Files:**
- Create: `llmkit/adapters/goagent/client.go`
- Create: `llmkit/adapters/goagent/client_test.go`
- Modify: `llmkit/go.mod`

**Step 1: Write tests**

Use a fake provider client. Verify the adapter implements `ports.LLMClient`, selects a candidate, calls the selected client, and records a route trace.

**Step 2: Add dependency**

Run:

```bash
(cd llmkit && go get github.com/eruca/goagent)
```

Expected: `llmkit/go.mod` references `github.com/eruca/goagent`.

**Step 3: Implement adapter**

Expose:

```go
type Client struct { ... }
func (c *Client) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error)
```

The adapter must not change `goagent`.

**Step 4: Verify**

Run:

```bash
(cd llmkit && go test ./...)
```

Expected: PASS.

**Step 5: Commit**

```bash
git add llmkit/adapters/goagent/client.go llmkit/adapters/goagent/client_test.go llmkit/go.mod llmkit/go.sum
git commit -m "feat(llmkit): 提供 goagent 适配器"
```

### Task 7: Add Example Config And Documentation

**Files:**
- Create: `llmkit/examples/config/.llmkit/config.yaml`
- Create: `llmkit/README.md`

**Step 1: Write README**

Document:

- `LLMKIT_HOME`.
- local-first routing for simple tasks.
- advanced-model routing for hard tasks.
- audit files.
- API key environment variable rules.
- goagent adapter usage.

**Step 2: Add example config**

Use aliases only. Do not include real keys.

**Step 3: Verify**

Run:

```bash
(cd llmkit && go test ./...)
```

Expected: PASS.

**Step 4: Commit**

```bash
git add llmkit/README.md llmkit/examples/config/.llmkit/config.yaml
git commit -m "docs(llmkit): 添加配置示例和使用说明"
```
