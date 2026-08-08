# GoAgents Week 18 进程内双 Tag 发布收口实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox
> (`- [ ]`) syntax for tracking.

**Goal:** 从 clean `main@5bbcf73a02d05276ca6e231b0beedb53d8869646` 形成只包含
`goagent/v0.1.1` 与 `llmkit/v0.1.1` 的可审核 release candidate，供 AI Todo 进程内 runtime
精确消费。

**Architecture:** 不新增 Host、进程、协议或 Provider 类型。`goagent` 保留 Week 17 已完成的
Agent/Provider 可靠性修正；`llmkit` 精确依赖 `goagent v0.1.1`。仓库外 synthetic file proxy
只证明同一源码快照的候选 module 可消费，不冒充远端 tag、CI 或发布。

**Tech Stack:** Go 1.26.1、Go modules、Bash、GitHub Actions、仓库外 file proxy consumer。

## Global Constraints

- 当前发布集合只有 `goagent/v0.1.1` 与 `llmkit/v0.1.1`；不包含 `hostruntime`、
  `goagents-host`、UDS/mTLS、Host OCI、SBOM、容器许可证/漏洞或双模式 conformance。
- 现有 `/Users/nick/.codex/worktrees/2d15/goagents` Host Diff 保留为未来线索，不复制、删除、
  stage、commit 或混入本 worktree。
- `goagent/v0.1.1` 与 `llmkit/v0.1.1` 必须来自同一 clean reviewed commit；禁止 `replace`、
  pseudo-version、`go.work` 或本机 cache 充当仓外消费证据。
- `llmkit/go.mod` 精确要求 `github.com/eruca/goagents/goagent v0.1.1`。
- 保持 `v0.1.0` 公共 struct 形状与旧 constructor 兼容；本计划不修改 Go 公共 API 或 runtime
  行为，只收口 module metadata、候选验证、release layout、CI 文案和发布证据。
- Provider 验证只使用 loopback/fake；不读取真实凭据，不产生调用费用。
- 本计划只准备本地 release candidate。实际 Diff 通过用户审核前不 stage、commit、merge、
  push 或创建 tag；远端 CI、annotated tag 和正式 consumer 属于后续独立授权。

---

### Task 1：建立两模块候选 file proxy 与负面门禁

**Files:**
- Create: `scripts/lib/inprocess-candidate-proxy.sh`
- Create: `scripts/with-inprocess-candidate-proxy.sh`
- Create: `scripts/verify-inprocess-candidate-proxy-test.sh`

**Interfaces:**
- Consumes: 根 `LICENSE`、`goagent/`、`llmkit/` 当前源码。
- Produces: `with_inprocess_candidate_proxy command arguments` shell boundary；在仓库外临时目录
  发布 `goagent/v0.1.1`、`llmkit/v0.1.1`，覆盖调用方 cache 并在退出时精确清理。

- [x] **Step 1: 先写 candidate proxy 负面测试**

测试必须先因 wrapper 不存在而 RED，并固定以下断言：

```bash
test -x scripts/with-inprocess-candidate-proxy.sh
scripts/with-inprocess-candidate-proxy.sh bash -c '
  test "$GOMODCACHE" != "$CALLER_GOMODCACHE"
  test "$GOCACHE" != "$CALLER_GOCACHE"
'
```

另在仓库外 consumer 中要求：两个 module 精确解析为 `v0.1.1`、Replace 为空、module archive
包含与根 `LICENSE` 相同 SHA-256 的普通文件；请求不存在的 `goagent@v0.1.2` 必须失败。

- [x] **Step 2: 实现最小 deterministic module packager**

`scripts/lib/inprocess-candidate-proxy.sh` 只导出：

```bash
inprocess_sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}
package_inprocess_module() {
  local source_dir="$1" module_path="$2" module_version="$3"
  local proxy_root="$4" archive_root="$5" root_license="$6"
  # 校验绝对 regular non-symlink LICENSE 与固定 Apache-2.0 SHA-256；
  # 复制 module、补根 LICENSE、固定 mtime、按排序文件列表生成 -X zip。
}
```

wrapper 使用 `mktemp -d`，固定打包两个 `v0.1.1`，设置独立 `GOMODCACHE/GOCACHE`、
`GONOSUMDB` 与 `GOPROXY=file://$proxy_root,https://proxy.golang.org,direct`，再 `exec "$@"`；trap 只清理
本脚本创建的临时目录。

- [x] **Step 3: 运行 proxy 测试确认 GREEN**

Run:

```bash
bash -n scripts/lib/inprocess-candidate-proxy.sh
bash -n scripts/with-inprocess-candidate-proxy.sh
bash -n scripts/verify-inprocess-candidate-proxy-test.sh
bash scripts/verify-inprocess-candidate-proxy-test.sh
```

Expected: 精确两个 `v0.1.1`、无 Replace、错误 LICENSE/版本/cache 边界全部 fail closed。

### Task 2：冻结 `llmkit` 精确依赖与两模块 release manifest

**Files:**
- Modify: `llmkit/go.mod`
- Modify: `llmkit/go.sum`
- Modify: `go.work`
- Modify: `scripts/verify-release-layout.sh`
- Modify: `scripts/verify-release-layout-test.sh`

**Interfaces:**
- Consumes: Task 1 candidate proxy。
- Produces: 同一 commit 上 `goagent v0.1.1` 与依赖它的 `llmkit v0.1.1` module graph。

- [x] **Step 1: 建立 release layout RED**

先只把 `llmkit/go.mod` 的内部依赖改为：

```go
github.com/eruca/goagents/goagent v0.1.1
```

Run:

```bash
bash scripts/verify-release-layout.sh
```

Expected: 因 manifest/internal requirement/go.work 仍声明 `v0.1.0` 而失败。

- [x] **Step 2: 收口两模块 manifest**

`published_modules` 中仅以下两项为本次 `release-delta`：

```text
goagent|github.com/eruca/goagents/goagent|v0.1.1|goagent/v0.1.1|release-delta
llmkit|github.com/eruca/goagents/llmkit|v0.1.1|llmkit/v0.1.1|release-delta
```

`hostkit/v0.1.0`、`workflowkit/v0.1.1`、`runkit/v0.1.1` 改为 `existing`；
`release_delta_tags` 精确为上述两个 tag。`internal_requirements` 只把 `llmkit -> goagent` 更新为
`v0.1.1`，其他 module 仍保持其已发布要求。

`go.work` 精确改为：

```text
replace github.com/eruca/goagents/goagent v0.1.1 => ./goagent
replace github.com/eruca/goagents/llmkit v0.1.1 => ./llmkit
```

负面测试文案从固定“第四个 delta”改为“unexpected extra delta”，继续证明额外 module 和
`memorykit/v0.0.0` 都会触发 `release delta set mismatch`。

- [x] **Step 3: 生成并验证 `llmkit/go.sum`**

Run:

```bash
bash scripts/with-inprocess-candidate-proxy.sh bash -c \
  'cd llmkit && GOWORK=off go mod tidy'
bash scripts/with-inprocess-candidate-proxy.sh bash -c \
  'cd llmkit && GOWORK=off go mod tidy -diff'
bash scripts/with-inprocess-candidate-proxy.sh bash -c \
  'cd llmkit && GOWORK=off go list -m -f "{{.Path}}|{{.Version}}|{{if .Replace}}{{.Replace.Path}}{{end}}" all'
```

Expected: `goagent|v0.1.1|` 且无 Replace；tidy diff 为空。

- [x] **Step 4: 验证 release layout GREEN**

```bash
bash scripts/verify-release-layout.sh
bash scripts/verify-release-layout-test.sh
```

Expected: 正向 layout 和额外 delta/unreleased delta 负面门禁均通过。

### Task 3：把 Week 17 consumer 升格为精确双 tag 候选门禁

**Files:**
- Modify: `scripts/verify-week17-consumer.sh`
- Modify: `.github/workflows/ci.yml`
- Modify: `README.md`
- Modify: `docs/modules.md`

**Interfaces:**
- Consumes: Task 2 精确 module graph。
- Produces: 无 `replace` 的仓库外 `v0.1.1` consumer，以及 future exact-commit CI 入口。

- [x] **Step 1: 将 consumer 版本从 prerelease 改为精确 `v0.1.1`**

保持现有兼容、limits、pre-dispatch、cancel、fallback、redirect、响应脱敏测试不变，仅将：

```bash
candidate_version="v0.1.1"
```

并把最终摘要改为 `Week 18 in-process release candidate verification passed`。consumer 继续使用
空 cache、仓库外目录、无 Replace；不得把 synthetic proxy 写成远端 tag 证据。

- [x] **Step 2: 同步 CI 与正式文档**

- CI step 改名为 `Verify Week 18 in-process release candidate`，仍只运行该 consumer；不增加
  registry、Host、OCI、secret 或 write permission。
- README 当前 release candidate 改为 `goagent/llmkit v0.1.1`，明确发布还需 exact remote
  commit、CI、annotated tags 和 tag consumer。
- `docs/modules.md` 把 `goagent`、`llmkit` 当前 release target 改为 `v0.1.1`；Host runtime 不进入
  module/release 表。

- [x] **Step 3: 运行仓外候选 consumer**

```bash
env -u OPENAI_COMPAT_BASE_URL -u OPENAI_COMPAT_MODEL -u OPENAI_COMPAT_API_KEY \
  GOWORK=off bash scripts/verify-week17-consumer.sh
```

Expected: 精确解析两个 `v0.1.1`，全部可靠性场景通过；无真实 Provider 调用。

### Task 4：冻结本地 release candidate 并交付审核

**Files:**
- Create: `docs/devlog/week-18-inprocess-release.md`
- Modify: `docs/superpowers/plans/2026-08-08-week18-inprocess-release.md`

**Interfaces:**
- Consumes: Tasks 1～3 冻结 Diff。
- Produces: U01～U05 本地证据包；U06 的 commit/push/tag/远端 CI/正式 consumer 保持待授权。

- [x] **Step 1: 运行两模块 fresh gate**

```bash
GOWORK=off bash scripts/with-inprocess-candidate-proxy.sh bash -c \
  'cd goagent && go mod tidy -diff && go test -count=1 ./... && go test -race -count=1 ./... && go vet ./...'
GOWORK=off bash scripts/with-inprocess-candidate-proxy.sh bash -c \
  'cd llmkit && go mod tidy -diff && go test -count=1 ./... && go test -race -count=1 ./... && go vet ./...'
```

Expected: 全部通过。

- [x] **Step 2: 运行冻结后的仓库门禁**

```bash
bash scripts/verify-inprocess-candidate-proxy-test.sh
bash scripts/verify-week17-consumer.sh
bash scripts/verify-release-layout.sh
bash scripts/verify-release-layout-test.sh
env -u OPENAI_COMPAT_BASE_URL -u OPENAI_COMPAT_MODEL -u OPENAI_COMPAT_API_KEY \
  -u MEMORYKIT_POSTGRES_TEST_DSN -u MEMORYKIT_REQUIRE_POSTGRES -u LLMKIT_HOME \
  bash scripts/verify-all.sh
git diff --check
```

Expected: 全部通过；PostgreSQL、真实 Provider、Docker 与独立 Host 进程均不启动。
`verify-all` 可回归仓库既有 `hostkit` 单测与 `examples/host-runtime` 进程内示例，但不得把它们
计入本次 release delta 或写成 Host runtime/OCI 发布证据。

- [x] **Step 3: 记录六层状态与停止点**

devlog 必须分别记录：实现、自动验证、真实运行（loopback/仓外 consumer）、CI（未执行）、独立
审核（未执行）、用户审核（待完成），并记录：

- clean 起点 `5bbcf73a02d05276ca6e231b0beedb53d8869646`；
- dirty Host worktree 不属于本 release candidate；
- 远端当前仅有两个 `v0.1.0` tag；
- 本地 synthetic `v0.1.1` 不是正式 tag；
- commit、merge、push、tag、Release 均未执行。

- [x] **Step 4: 展示实际 Diff 并停止**

```bash
git status --short --branch
git diff --stat
git diff --check
```

等待用户审核实际 Diff；未经新的 Git/tag/push 授权，不进入远端发布动作。
