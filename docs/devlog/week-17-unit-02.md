# W17-U02：取消后 health/outcome 有界收口

日期：2026-08-06

状态：实现与 Unit 自动验证已完成；用户已于 2026-08-06 以“继续”审核通过实际
Diff，并授权进入 W17-U03。本文只记录 W17-U02，不授权或实现 W17-U04～U05、
Week 18、tag、Release、OCI 或 push。

## 前置审核与范围

- 用户于 2026-08-06 以“继续”审核通过 W17-U01 实际 Diff，并授权进入 W17-U02。
- 本 Unit 只关闭 G01：Provider `Begin` 成功后，即使请求 context 已取消或超时，仍在独立、
  有界的 context 中收口 health/outcome；收口失败不得进入第二 Provider。
- G02 的 `canceled` 类型、按稳定类型裁决 fallback、取消/timeout 不 fallback 和晚到成功响应
  丢弃仍属于 W17-U03，本 Unit 不提前实现。
- G03 Provider token/字节限制与脱敏、G08 pre-dispatch hook、生产 `hostruntime`、UDS/mTLS、
  Control/Tool Bridge、Host binary/OCI 均未实现。
- AI Todo 仅作为只读合同来源；没有修改依赖、代码、数据库、Server、容器或浏览器，也没有
  连接、探测或操作 `127.0.0.1:54321`。
- 没有使用真实 Provider 凭据、发送业务/用户数据或产生 Provider 费用。

## 实时起点与隔离边界

- 实施路径：`/Users/nick/.codex/worktrees/4149/goagents`，detached
  `0a24e951b92e46fe4d39ca59ffda93f42952aedc`。
- U02 启动时未暂存任何文件；已有未提交内容仅为审核通过的 U01
  `llmkit/go.sum` 与 `docs/devlog/week-17-unit-01.md`。
- AI Todo 保持 `HEAD = main = origin/main =
  713246d09e92ac004cba768b26a7d98897235cb6` 且 clean。
- GoAgents 主目录用户文档没有被修改、stage、提交、回退或清理。收口复核时：
  - tracked diff SHA-256：
    `beafd422a0bbf63212c4616eab2615659b3f58bfd210b36df8bed24815966a5f`；
  - staged diff SHA-256：
    `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`；
  - 六个既有 untracked 文档内容清单 SHA-256：
    `bf8c8baed2b7dfeccd6f9fd3053e17f23734b31fa5251c4b41c13c1ed6a10a6a`。
- 隔离 worktree 与 GoAgents 主目录均没有 `.codegraph/`，因此没有初始化或使用 CodeGraph。

## 正式合同与真实根因

正式 W17-U02 Exit Gate 是：cancel/timeout 不泄漏 `in_flight`，post-call failure 不
fallback。进程内与跨进程架构合同都把取消后的 health/outcome cleanup 固定为独立的
2 秒 server-owned context。

真实调用顺序原为：

```text
HealthStore.Begin(requestCtx)
Provider.Chat(requestCtx)
HealthStore.RecordOutcome(requestCtx)
Recorder.RecordOutcome(requestCtx)
```

`llmkit.MemoryHealthStore.RecordOutcome` 会先检查 `ctx.Err()`，再递减 `InFlight`。Provider
调用期间若请求取消或超时，adapter 在 Provider 返回后继续复用该请求 context，
`RecordOutcome` 会在递减前返回 `context.Canceled` 或 `context.DeadlineExceeded`，已开始的
调用永久停留在 `InFlight=1`；后续安全 outcome 也无法执行。

`MemoryHealthStore` 对调用方 context 的检查本身是正确边界，不能通过忽略取消或无界
background context 修补。错误位于 adapter 的 Provider 后收口生命周期。

## 正确 RED

先只增加直接回归测试和确定性 request context 故障注入，未修改生产实现。执行目录为
`llmkit/`：

```bash
GOWORK=off go test -count=1 ./adapters/goagent \
  -run 'TestClient(ClosesProviderHealthWhenRequestContextEnds|BoundsPostCallCleanupAndDoesNotFallbackOnFailure)' \
  -v
```

结果：exit `1`，精确失败为：

- `canceled`：`health in-flight = 1, want 0`；
- `deadline_exceeded`：`health in-flight = 1, want 0`；
- post-call recorder 用例：实际返回 `context canceled`，预期的
  `outcome recorder unavailable` 没有被调用或返回。

这三个失败共同证明问题来自 Provider 后仍使用失效请求 context，而非 fallback 排序、
Provider 模拟结果或 `MemoryHealthStore` 的独立行为。

## 最小实现

只修改 `llmkit/adapters/goagent/client.go`：

- 增加私有固定值 `providerOutcomeCleanupTimeout = 2 * time.Second`，不扩大公共 API；
- Provider 返回后用
  `context.WithTimeout(context.WithoutCancel(requestCtx), 2*time.Second)` 构造独立、保留调用链
  context value 且带新上界的 cleanup context；
- 同一个 2 秒预算依次覆盖 health 与可选 Recorder outcome；
- 成功和失败 Provider 结果都经过同一个 helper，避免复制两套生命周期逻辑；
- 任一 post-call health/outcome 错误继续立即返回，不删除候选、不调用第二 Provider。

没有修改 `llmkit.HealthStore`、`MemoryHealthStore`、fallback policy、错误分类、Provider port
或示例 Host，也没有用 `go.work`、`replace`、缓存或跳包掩盖问题。

## GREEN 与验证

定向 GREEN：

```bash
GOWORK=off go test -count=1 ./adapters/goagent \
  -run 'TestClient(ClosesProviderHealthWhenRequestContextEnds|BoundsPostCallCleanupAndDoesNotFallbackOnFailure)' \
  -v
```

结果：exit `0`。cancel 与 deadline 两类请求结束后 `InFlight=0`；post-call recorder 收到仍
有效且 deadline 不超过 2 秒的 cleanup context，返回注入错误后 Provider 调用数为
`local=1, cloud=0`。

冻结实现后的新鲜验证：

- `GOWORK=off go test -count=1 ./...`：通过，三个 `llmkit` package 全部通过；
- `GOWORK=off go test -race -count=1 ./...`：通过，三个 package 全部通过；
- `GOWORK=off go vet ./...`：通过；
- `GOWORK=off go mod tidy -diff`：通过，无 module 文件漂移；
- `git diff --check`：通过；
- 真实 Provider/Server/数据库/浏览器：不适用且未获授权；
- 外部 consumer：本 Unit 没有公共 API 变化，留到 W17-U05 冻结门禁统一复核；
- CI 与独立审核：未执行；Week 级门禁不在 U02 冒充完成。
- 用户审核：2026-08-06 已以“继续”审核通过实际 Diff，并授权进入 W17-U03。

## 未关闭风险与后续边界

- 当前 `context.Canceled` 仍由既有 classifier 归为 `unknown`，Provider error 后的 fallback
  仍未按稳定类别裁决；这是 W17-U03/G02 的正确 RED 起点。
- Provider 忽略取消并晚到成功时，结果丢弃与 canceled typed error 也尚未实现；本 Unit 只
  保证已开始的 health/outcome 有界、精确收口，不把 G01 宣称成完整取消语义。
- Provider 请求/响应大小、输出 token 与错误 body 脱敏仍是 W17-U03/G03。
- typed pre-dispatch hook 仍是 W17-U04/G08。

## Diff 与审核停止点

- U02 生产实现：`llmkit/adapters/goagent/client.go`。
- U02 直接回归：`llmkit/adapters/goagent/client_test.go`。
- U02 证据：`docs/devlog/week-17-unit-02.md`。
- U01 devlog 仅把用户审核状态从“待审核”更新为“已通过”；U01 `go.sum` 修正保持不变。
- stage/commit/merge/push/tag/Release/OCI：均未执行。
- 完成 Unit 验证并展示实际 Diff 后已停止；用户审核通过后才进入 W17-U03。
