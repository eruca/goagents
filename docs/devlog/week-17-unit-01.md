# W17-U01：GoAgents 基线、`llmkit/go.sum` 与失败矩阵

日期：2026-08-06

状态：实现与 Unit 自动验证已完成；用户已于 2026-08-06 以“继续”审核通过实际
Diff，并授权进入 W17-U02。本文只记录 W17-U01，不授权或实现 W17-U03～U05、
Week 18、tag、Release、OCI 或 push。

## 目标

- 从实时 Git、正式架构和真实代码重新建立 Week 17 基线。
- 在 `GOWORK=off` 下复现并最小修正 `llmkit` 独立 module 的 `go.sum` 缺项。
- 把 G01～G04、G08 转化为后续 Unit 可执行的失败矩阵，不提前实现运行时行为。

## 范围与停止线

- 本 Unit 只修改 `llmkit/go.sum` 和本 devlog。
- AI Todo 仅作只读事实源；没有修改依赖、代码、数据库、Server、容器或浏览器。
- 不连接、探测或操作 `127.0.0.1:54321`。
- 不使用真实 Provider 凭据，不发送业务或用户数据，不产生 Provider 费用。
- `examples/host-api` 和 `examples/host-runtime` 都是带本地 `replace` 的验证/组合示例；
  `hostkit` 只提供标准库单 Service 生命周期。三者都不是 Week 18 规划中的生产
  `hostruntime`。
- GoAgents worktree 未初始化 `.codegraph/`，因此没有运行 `codegraph init` 或 CodeGraph 查询。

## 正式架构与 module 边界

- ADR-0004～0007 要求 AI Todo 保持一个业务 Server/业务事实源，同时交付进程内和跨进程
  两种 GoAgents 模式；两种模式共用 Gateway、Tool、Candidate、Proposal、错误和审计合同。
- Week 17 只关闭 G01～G04、G08；Week 18 才实现 G05～G07、G09～G10、生产
  `hostruntime`、UDS/mTLS、Control/Tool Bridge、Host binary/OCI 和统一发布。
- `goagent` core 不依赖 `llmkit`；`llmkit/llmkit` 是独立路由核心，
  `llmkit/adapters/goagent` 才允许依赖 `goagent`。`llmkit/go.mod` 的正式依赖是
  `github.com/eruca/goagents/goagent v0.1.0`，没有 `replace`。
- 当前生产 Provider 相关代码只包括 `goagent/ports.ChatRequest`、
  `goagent/extensions/providers/openaiapi` 和 `llmkit/adapters/goagent` 的组合；没有正式
  Host wire/client/server 或 pre-dispatch hook。

## 实时 Git、remote 与 CI 基线

### AI Todo（只读）

- `git fetch origin --prune` 后：`HEAD = main = origin/main =
  713246d09e92ac004cba768b26a7d98897235cb6`，ahead/behind `0/0`，主目录 clean。
- handoff 中 `origin/main=97e503d`、本地 ahead 12、未 push 的状态已失效；远端已出现精确
  `713246d` 的 CI run `31110629251`。启动审计时为 `in_progress`，Unit 收口复核时已
  `completed/success`，Go 后端与 Web 前端 jobs 均通过。
- 这项外部漂移没有改变 Week 17 的 GoAgents 实施边界，也没有触发 AI Todo 运行时复验。

### GoAgents 隔离 worktree

- 路径：`/Users/nick/.codex/worktrees/4149/goagents`，detached HEAD，启动时 clean。
- `HEAD = main = origin/main = 0a24e951b92e46fe4d39ca59ffda93f42952aedc`，ahead/behind
  `0/0`。
- 最新远端 CI run `30140268232` 为 success，只覆盖 clean commit `0a24e951`，不覆盖本
  Unit 的未提交 Diff。

### GoAgents 主目录用户修改边界

- 路径：`/Users/nick/VibeCoding/goagents`，`main@0a24e951`，与 `origin/main` 同步。
- staged diff 为空；tracked diff SHA-256 为
  `beafd422a0bbf63212c4616eab2615659b3f58bfd210b36df8bed24815966a5f`；未跟踪内容清单
  SHA-256 为 `bf8c8baed2b7dfeccd6f9fd3053e17f23734b31fa5251c4b41c13c1ed6a10a6a`。
- 用户已有 tracked 修改仅为：
  `docs/superpowers/specs/2026-07-21-long-term-memory-design.md`。
- 用户已有未跟踪文件为：
  - `docs/decisions/0001-memorykit-cjk-normalization-boundary.md`
  - `docs/discovery/goal-discovery.md`
  - `docs/product/product-baseline.md`
  - `docs/roadmap/README.md`
  - `docs/roadmap/week-01-retrieval-evidence.md`
  - `docs/superpowers/plans/2026-07-27-memorykit-week-01-retrieval-evidence.md`
- 本 Unit 没有在主目录写文件、stage、提交、回退或清理上述内容。

## 正确 RED 与根因

执行目录：`llmkit/`。

```bash
GOWORK=off go test -count=1 ./...
```

结果：exit `1`。`llmkit/llmkit` 通过，但以下包在 setup 阶段失败：

- `github.com/eruca/goagents/llmkit/adapters/goagent`
- `github.com/eruca/goagents/llmkit/examples/goagent-routing`

精确错误是 `llmkit/go.sum` 缺少为 `goagent` packages 提供 module 的校验项，涉及实际导入：

- `goagent/extensions/providers/openaiapi`
- `goagent/ports`
- `goagent/agentcore`
- `goagent/prompt`

只读诊断：

```bash
GOWORK=off go mod tidy -diff
```

结果：exit `1`，只要求在 `llmkit/go.sum` 增加
`github.com/eruca/goagents/goagent v0.1.0` 的 module/content 两条校验；`go.mod` 无变化。
运行前后 manifest SHA-256 一致，证明 `tidy -diff` 没有写文件。

根因是根 `go.work` 的本地映射让 workspace 验证直接使用 `../goagent`，掩盖了 `llmkit`
作为独立主 module 时必须验证正式 `goagent v0.1.0` 的要求。公开 tag consumer 能解析不等于
当前 module checkout 能独立测试，二者必须分开记录。

## 最小修正

仅向 `llmkit/go.sum` 增加 `goagent v0.1.0` 的两条由 Go checksum database 验证的校验值；
没有修改 `go.mod`、源码、API、测试、`go.work` 或 release script。

定向 GREEN：

```bash
GOWORK=off go mod tidy -diff
GOWORK=off go test -count=1 ./...
```

结果：两条命令均 exit `0`；三个 `llmkit` package 全部通过。

## W17 失败矩阵

| 缺口 | 当前真实代码事实 | 后续正确 RED / 必须证明 | 影响面 | Unit |
| --- | --- | --- | --- | --- |
| G01 取消后收口 | Adapter 在 Provider 返回后继续把请求 `ctx` 传给 `HealthStore.RecordOutcome` 和 Recorder；`MemoryHealthStore.RecordOutcome` 在 canceled context 下先返回，已 `Begin` 的 `in_flight` 可留在 1 | Provider 在调用中取消/超时后，使用有界 server-owned cleanup context 精确收口，`in_flight=0`；post-call health/audit 失败不调用第二 Provider | `llmkit/adapters/goagent`、`llmkit/llmkit` | W17-U02 |
| G02 类型化错误与 fallback | `ErrorClass` 没有 `canceled`；`DefaultErrorClassifier(context.Canceled)` 的既有测试期望 `unknown`；fallback 循环不按错误类别裁决，任意 Provider error 都可能进入下一候选 | canceled/auth/config/policy/schema/budget/post-call/结果未知均用类型判断且默认零 fallback；只有明确允许类别才进入下一候选，不解析错误字符串 | `llmkit/llmkit/audit.go`、`llmkit/adapters/goagent` | W17-U03 |
| G03 Provider 限制与脱敏 | `ports.ChatRequest` 没有最大输出 token；OpenAI-compatible 请求不发送 token 上限，未限制请求字节，使用无界 `io.ReadAll`；`ResponseError` 保存 raw body 且 `Error()` 直接拼接正文，既有测试还显式要求泄漏正文 | token 必须下传；request/response 在边界值通过、`+1` fail closed；redirect/usage 缺失/invalid JSON/超限有稳定类型；非 2xx 公开错误和 audit 不含 raw body | `goagent/ports`、`goagent/extensions/providers/openaiapi`、`llmkit/adapters/goagent` | W17-U03 |
| G04 独立 module 卫生 | 根 workspace 测试可绕过 `llmkit` 已声明的发布依赖校验；U01 RED 已证明 checkout 独立测试失败 | U01 先完成 `go.sum` RED/GREEN；U05 在冻结 Diff 上再次证明 `goagent`、`llmkit` 独立 tidy/test/race/vet、layout 与仓库外 consumer | `llmkit/go.sum`，后续验证脚本/发布门禁 | W17-U01、W17-U05 |
| G08 typed pre-dispatch | Route metadata 只在配置 Recorder 时构造；当前没有 typed hook，顺序直接是可选 RecordRoute → health `Begin` → Provider | 无 Recorder 时仍构造安全 route metadata；hook 精确位于 health/Provider 前；hook 失败时 health begin、Provider、fallback 均为 0，敏感 request 内容不进入 metadata | `llmkit/adapters/goagent` 公共 Config/DTO 与测试 | W17-U04 |

矩阵只冻结失败起点与验证合同，不代表 G01～G03/G08 已实现。若后续真实 RED 证明需要破坏
公共 API，必须停止并重新选择版本，不能伪装成 patch。

## Unit 验证状态

- `GOWORK=off go mod tidy -diff`：通过。
- `GOWORK=off go test -count=1 ./...`：通过。
- `GOWORK=off go test -race -count=1 ./...`：通过，三个 package 全部通过。
- `GOWORK=off go vet ./...`：通过。
- `GOWORK=off go list -m ... all`：通过；当前独立 module 精确解析
  `goagent v0.1.0`，没有 `replace`。
- fresh module/build cache：通过；从空缓存重新下载 `goagent v0.1.0` 和传递依赖后，三个
  package 全部通过，临时目录已移入系统废纸篓。
- 仓库外 clean consumer：通过；从空缓存消费 `goagent v0.1.0 + llmkit v0.1.0`，公共
  `LLMClient`/adapter 类型可组合，两个 GoAgents module 均为精确 tag 且无 `replace`。
  该证据只证明公开 tag 消费边界，不冒充本次未提交 `go.sum` Diff 的验证。
- `bash ./scripts/verify-release-layout.sh`：通过，现有发布 module 与示例边界保持不变。
- 真实 Provider/Server/数据库/浏览器：不适用；本 Unit 没有运行时行为，也没有相应授权。
- CI：未执行；已有 GoAgents CI 只覆盖旧 clean commit。
- 独立审核：未执行；Week 级门禁不在 U01 冒充完成。
- 用户审核：2026-08-06 已以“继续”审核通过实际 Diff，并授权进入 W17-U02。

## 计划外事件

- AI Todo `origin/main` 相对 handoff 已前移到 `713246d`，且出现新的精确 SHA CI；已将旧
  ahead/未 push 结论标为失效。该事件不改变 GoAgents Week 17 目标、Unit 顺序或工期。
- GoAgents 主目录 dirty 文档与 handoff 一致，内容属于 memorykit 产品/roadmap 线；已用
  路径和内容指纹隔离，没有混入本 Unit。
- 第一次 fresh-cache 验证的测试本身通过，但 macOS `/usr/bin/trash` 不接受 `--`，导致
  cleanup-only exit `5`；已按精确路径移入废纸篓，并用正确清理命令完整重跑到 exit `0`。
- 第一次仓库外 consumer 使用了未经合同支持的 `DefaultTaskProfile().TaskType != ""` 断言，
  因测试假设错误而失败；没有修改生产代码迎合该断言。删除该假设后，最小公共类型消费
  测试从空缓存通过，两个临时 consumer 目录均已移入废纸篓。

## Diff 与审核停止点

- 本次实现文件：`llmkit/go.sum`。
- 本次证据文件：`docs/devlog/week-17-unit-01.md`。
- 用户已有修改：仅存在于 GoAgents 主目录，未进入本隔离 worktree Diff。
- stage/commit/merge/push/tag/Release/OCI：均未执行。
- 完成 Unit 验证并展示实际 Diff 后已停止；用户审核通过后才进入 W17-U02。
