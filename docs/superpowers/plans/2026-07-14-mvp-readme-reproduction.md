# MVP README Clean Reproduction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让首次进入仓库的开发者只按 README 即可找到 Host API MVP 入口，并在本机完成不依赖真实云端凭证的进程级验收。

**Architecture:** 根 README 只提供 workspace 导航与最短验证入口，Host API README 负责区分“本机自动化验收”和“连接真实 OIDC 后长期运行”。不新增开发认证服务或绕过开关，现有进程级 black-box smoke 继续充当可重复的本地闭环。

**Tech Stack:** Markdown、Bash、Go test build tags、macOS Keychain、现有 `hostapisystemsmoke`。

## Global Constraints

- 只修改根 `README.md`、`examples/host-api/README.md` 和本阶段规范/计划记录。
- 不修改 Go 生产代码、HTTP API、OpenAPI、认证或持久化实现。
- 不增加关闭 OIDC、伪造 approver 或把 Keychain key 放入文件/环境变量的说明。
- 本机 MVP smoke 必须使用现有 `TestHostAPIProcessMVPBlackBoxClosure`，不得加载 todo/Qwen 真实凭证。
- 三个 black-box 子场景必须全部 PASS；任何 SKIP 都按环境阻塞处理。
- 文档不得包含机器专属绝对路径或真实 secret。

---

### Task 1: 根 README 导航与 MVP 入口

**Files:**
- Create: `README.md`
- Reference: `scripts/verify-all.sh`
- Reference: `examples/host-api/README.md`

**Interfaces:**
- Consumes: 现有模块 README 和仓库级验证脚本。
- Produces: 仓库统一入口、模块链接和 Host API MVP 快速验收命令。

- [ ] **Step 1: 验证根 README 当前缺失**

Run:

```bash
test -f README.md
```

Expected: FAIL，退出码为 1。

- [ ] **Step 2: 创建最小根 README**

新增 `README.md`，内容为：

```markdown
# goagents

`goagents` is a Go workspace for composing durable agent runtimes from small,
independent kits. Each top-level module owns one boundary and can be used or
tested independently.

## Modules

- [`goagent`](goagent/README.md): agent loop, typed tools, approvals, events, and provider adapters.
- [`workflowkit`](workflowkit/README.md): durable workflow lifecycle, retries, approvals, and queue leases.
- [`runkit`](runkit/README.md): durable agent run records, events, and approval checkpoint storage.
- [`artifactkit`](artifactkit/README.md): artifact references and durable stores.
- [`llmkit`](llmkit/README.md): model routing, provider health, audit, and outcome statistics.
- [`skillkit`](skillkit/README.md): immutable Skill discovery, gating, activation, and agent projection.
- [`contextkit`](contextkit/README.md): context windows, projections, and tool budgets.
- [`evalkit`](evalkit/README.md): reproducible agent evaluation traces and graders.
- [`mcpkit`](mcpkit/README.md): MCP transport and tool integration.
- [`ocrs`](ocrs/README.md): OCR parsing, chunking, scheduling, and retry support.

## Verify the workspace

From the repository root:

```bash
bash ./scripts/verify-all.sh
```

This runs module tests, race checks for the core execution paths, MCP smokes,
and runnable examples.

## Host API MVP

The runnable single-host MVP is documented in
[`examples/host-api`](examples/host-api/README.md). On an interactive macOS
login session with CGO and an unlocked login Keychain, run its complete
process-level acceptance suite:

```bash
cd examples/host-api
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPBlackBoxClosure$' \
  -count=1 ./...
```

All three subtests must report `PASS`. A `SKIP` means the local environment is
blocked and is not evidence of a completed MVP acceptance run. The smoke uses
loopback test providers, synthetic credentials, isolated temporary runtime
state, and separate `.smoke.` Keychain service/account pairs for its three
scenarios. Each scenario removes only its exact pair.
The suite never accesses the production-default Keychain item.
```

- [ ] **Step 3: 验证根 README 链接与命令存在**

Run:

```bash
for target in \
  goagent/README.md workflowkit/README.md runkit/README.md \
  artifactkit/README.md llmkit/README.md skillkit/README.md \
  contextkit/README.md evalkit/README.md mcpkit/README.md \
  ocrs/README.md examples/host-api/README.md scripts/verify-all.sh; do
  test -f "$target" || exit 1
done
rg -n "TestHostAPIProcessMVPBlackBoxClosure" README.md
rg -n 'A `SKIP` means the local environment is' README.md
rg -n "not evidence of a completed MVP acceptance run" README.md
rg -n 'separate `\.smoke\.` Keychain service/account pairs' README.md
rg -n "Each scenario removes only its exact pair" README.md
rg -n "never accesses the production-default Keychain item" README.md
```

Expected: 退出码为 0，并匹配完整 smoke 名称和 SKIP 边界。

- [ ] **Step 4: 提交根入口**

```bash
git add README.md
git commit -m "docs: 增加仓库入口与 MVP 导航"
```

### Task 2: Host API 两条运行路径

**Files:**
- Modify: `examples/host-api/README.md`
- Reference: `examples/host-api/host_mvp_blackbox_smoke_test.go`

**Interfaces:**
- Consumes: `TestHostAPIProcessMVPBlackBoxClosure` 和现有 OIDC 配置契约。
- Produces: 不需要真实外部或云端 Provider 凭证的本机验收路径，以及明确要求真实 OIDC 的长期运行路径。

- [ ] **Step 1: 验证 README 尚未暴露完整 MVP smoke**

Run from the repository root:

```bash
rg -n "TestHostAPIProcessMVPBlackBoxClosure" examples/host-api/README.md
```

Expected: FAIL，退出码为 1。

- [ ] **Step 2: 在简介后增加本机 MVP 快速验收**

在默认 runtime 目录说明之前插入：

```markdown
## Local MVP acceptance

The shortest path that needs no real credentials for external or cloud
providers is the real-process black-box smoke. From this directory, first run
the default package tests, then the complete MVP acceptance suite:

```bash
go test ./... -count=1
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPBlackBoxClosure$' \
  -count=1 ./...
```

The tagged suite requires macOS, CGO, an interactive login session, and an
unlocked login Keychain. It starts loopback-only OIDC and OpenAI-compatible
test providers, uses a synthetic API key, and keeps each scenario in an
isolated temporary runtime home. It never loads a real Qwen or cloud-provider
credential.

Each scenario uses a separate Keychain service/account pair whose service
contains `.smoke.`. Cleanup deletes only that exact pair and refuses non-smoke
services. The suite never accesses or deletes the production-default item.
That item uses service `goagents.host-api.approvals` and account `approval-data-key:local-v1`.

The three subtests cover approval plus Skill restart recovery, Provider 503
followed by HTTP requeue, and fail-closed handling of an unregistered tool.
All three must report `PASS`. A `SKIP` means the environment is blocked and is
not a successful MVP acceptance result.
```

- [ ] **Step 3: 把末尾运行命令改成真实 OIDC 路径**

把原来的 `Run it:` 段落替换为：

```markdown
## Run the long-lived service

`go run .` is not a zero-configuration command. The service fails closed
unless it can discover a real OIDC issuer and verify approval bearer tokens
through that issuer's JWKS:

```bash
export HOST_API_OIDC_ISSUER=https://id.example.com
export HOST_API_OIDC_AUDIENCE=goagents-host-api
go run .
```

Set `HOST_RUNTIME_HOME` to retain state across restarts and `HOST_API_ADDR` to
choose the listen address. If `LLMKIT_HOME` is unset, it defaults to
`$HOST_RUNTIME_HOME/.llmkit`. Set `HOST_API_QUEUED_LEASE_DURATION` to tune the
in-process queued worker lease duration; it accepts Go durations such as `30s`
or `2m` and defaults to `1m`.

There is intentionally no switch that disables OIDC or accepts an approver
identity from the request body. For a local proof without real external or
cloud-provider credentials, use the MVP acceptance smoke above.
```

- [ ] **Step 4: 验证文档明确区分两条路径**

Run:

```bash
rg -n "Local MVP acceptance" examples/host-api/README.md
rg -n "TestHostAPIProcessMVPBlackBoxClosure" examples/host-api/README.md
rg -n "Run the long-lived service" examples/host-api/README.md
rg -n "not a zero-configuration command" examples/host-api/README.md
rg -n "no switch that disables OIDC" examples/host-api/README.md
rg -n "needs no real credentials for external or cloud" examples/host-api/README.md
rg -n "Each scenario uses a separate Keychain service/account pair" examples/host-api/README.md
rg -n 'contains `\.smoke\.`' examples/host-api/README.md
rg -n "Cleanup deletes only that exact pair" examples/host-api/README.md
rg -n "refuses non-smoke" examples/host-api/README.md
rg -n "never accesses or deletes the production-default item" examples/host-api/README.md
rg -n 'service `goagents\.host-api\.approvals`' examples/host-api/README.md
rg -n 'account `approval-data-key:local-v1`' examples/host-api/README.md
if rg -n "^Run it:$" examples/host-api/README.md; then exit 1; fi
```

Expected: 退出码为 0，上述必需边界均可匹配，旧的模糊标题不存在。

- [ ] **Step 5: 提交 Host README**

```bash
git add examples/host-api/README.md
git commit -m "docs(host-api): 区分本机验收与真实 OIDC 运行"
```

### Task 3: 按 README 原样复现并记录结果

**Files:**
- Modify: `docs/superpowers/specs/2026-07-14-mvp-readme-reproduction-design.md`
- Verify: `README.md`
- Verify: `examples/host-api/README.md`

**Interfaces:**
- Consumes: Task 1 和 Task 2 中发布的命令与边界。
- Produces: 阶段 3 的脱敏验收证据和完成状态。

- [ ] **Step 1: 从根 README 执行 workspace 验证**

Run:

```bash
bash ./scripts/verify-all.sh
```

Expected: 退出码为 0，末行包含 `goagents workspace verification passed`。

- [ ] **Step 2: 从 Host README 执行默认测试与完整 black-box smoke**

Run:

```bash
cd examples/host-api
go test ./... -count=1
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPBlackBoxClosure$' \
  -count=1 ./...
```

Expected: 退出码为 0；三个子场景均 PASS，没有 SKIP。

- [ ] **Step 3: 校验链接、安全文本和工作区**

Run:

```bash
cd ../..
for target in \
  goagent/README.md workflowkit/README.md runkit/README.md \
  artifactkit/README.md llmkit/README.md skillkit/README.md \
  contextkit/README.md evalkit/README.md mcpkit/README.md \
  ocrs/README.md examples/host-api/README.md scripts/verify-all.sh; do
  test -f "$target" || exit 1
done
if rg -n '/Users/|sk-[A-Za-z0-9_-]{8,}|Bearer [A-Za-z0-9._-]{12,}' README.md examples/host-api/README.md; then exit 1; fi
base=$(git merge-base main HEAD)
git diff --check "$base"..HEAD
```

Expected: 退出码为 0，不包含机器路径、疑似 secret 或空白错误。

- [ ] **Step 4: 更新设计文档验收状态**

把设计文档状态改为 `已实现并验收`，并追加：

```markdown
## 8. 验收记录

2026-07-14 在全新 worktree 中只按 README 发布的命令完成复现：

- `bash ./scripts/verify-all.sh`：PASS；
- Host API 默认测试：PASS；
- `approval_skill_and_restart`：PASS；
- `provider_failure_requeue_and_success`：PASS；
- `unregistered_tool_fails_closed`：PASS；
- tagged smoke：没有 SKIP；
- 根 README 相对链接、安全文本扫描和基于 `main` merge-base 的 range `git diff --check`：PASS。

长期服务仍明确要求真实 OIDC discovery/JWKS；本阶段没有增加认证绕过或真实 Provider 凭证。
```

- [ ] **Step 5: 提交验收记录并检查分支**

```bash
git add docs/superpowers/specs/2026-07-14-mvp-readme-reproduction-design.md
git commit -m "docs(mvp): 记录 README 干净环境复现结果"
base=$(git merge-base main HEAD)
git diff --check "$base"..HEAD
git status --short --branch
git log --oneline --decorate -6
```

Expected: 分支工作区干净，文档提交按入口、Host 路径和验收记录分离。
