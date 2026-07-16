# GoAgents MVP 最终验收记录

**日期：** 2026-07-15

**产品代码基线：** `dabbef6`

**最终验收提交：** 本文与 `provideracceptance` 门禁所在提交；不再修改产品运行代码

**目标环境：** Apple M1 Pro、10 核、16 GiB、macOS 26.5.1

**结论：** GO——当前本机、单宿主、单执行槽 MVP 已达到既定退出标准，可进入首个
`v0.1.0` tag 候选评审；本记录不自动创建 tag、推送远端或宣称生产就绪。

**本机试用补充：** 2026-07-16 的真实 Host × Qwen 组合试用无新增 P0/P1，详见
[2026-07-16-mvp-local-trial.md](2026-07-16-mvp-local-trial.md)。

## 1. 验收边界

本结论只覆盖一个本机、可恢复、可审计的 Agent/Workflow Runtime：Host 通过 OIDC 验证
operator，使用 Keychain 保护 approval checkpoint 密钥，以 SQLite 保存 workflow、run、
artifact 引用、审批和 Skill `name@digest`，并通过单个 queued worker 执行。

不把 examples 视为多租户产品，不承诺远程 Skill registry、自动安装、脚本沙箱、动态 Skill
activation、多 Agent handoff、分布式 worker、任意崩溃点 exactly-once 或生产吞吐。

## 2. 退出标准

| 标准 | 结果 | 证据 |
|---|---|---|
| 自动化基线全绿 | PASS | 合并后的 `main@dabbef6` 执行 `bash scripts/verify-all.sh`，退出 0 |
| 至少一个真实模型 Provider | PASS | `openai_compatible` / `qwen3.7-plus`，6 个子场景全部 PASS |
| Host 黑盒闭环与重启恢复 | PASS | 三个 `TestHostAPIProcessMVPBlackBoxClosure` 子场景 PASS、无 SKIP |
| 安全负例失败关闭 | PASS | 未注册工具、无效 OIDC、Skill/digest/resource gate、审批过期/冲突/重试及 Provider 错误回归通过 |
| 干净环境按公开文档复现 | PASS | 全新 worktree 执行 workspace、Host 默认与 tagged smoke，公开链接和安全文本检查通过 |
| 已定义负载并通过稳定性门禁 | PASS | 3×50 workflow、10 路提交、同 runtime home 重启，全部 succeeded |
| 未关闭 P0/P1 | PASS | 声明的安全空闲或已持久化停止/重启边界内 P0=0、P1=0 |
| P2/P3 与环境限制已记录 | PASS | 见第 6、7 节 |

## 3. 真实 Provider

执行仓库内显式门禁：

```bash
cd examples/host-api
go test -v -tags provideracceptance \
  -run '^TestRealProviderMVPAcceptance$' \
  -count=1 ./...
```

本机只把 `todo/.env` 的 `LLM_BASE_URL`、`LLM_MODEL`、`LLM_API_KEY` 映射到
`OPENAI_COMPAT_BASE_URL`、`OPENAI_COMPAT_MODEL`、`OPENAI_COMPAT_API_KEY`；没有复制或提交值。
结果：

```text
TestRealProviderMVPAcceptance/text                       PASS
TestRealProviderMVPAcceptance/tool_observation           PASS
TestRealProviderMVPAcceptance/structured_output_success  PASS
TestRealProviderMVPAcceptance/structured_output_failure  PASS
TestRealProviderMVPAcceptance/auth_error_classification  PASS
TestRealProviderMVPAcceptance/timeout_error_classification PASS
```

工具场景确认一次 typed tool call 位于两次 LLM call 之间，最终回答包含本轮随机生成且只由工具
返回的 observation nonce；结构化失败使用不可满足 schema，确认合法 JSON 被 schema 拒绝并
稳定返回 `ErrOutputInvalid` 和 partial run；故意无效的合成凭证形成
`provider_error/auth_error` outcome，真实 endpoint 上的
强制 client deadline 形成 `provider_error/timeout` outcome。输出未包含 endpoint、API key、
Prompt、模型正文、随机 nonce 或原始 Provider 错误。

## 4. Host 黑盒与稳定性

基于产品代码 `main@dabbef6` 的最终验收 worktree 执行：

```bash
cd examples/host-api
go test -v -tags hostapisystemsmoke \
  -run '^(TestHostAPIProcessMVPBlackBoxClosure|TestHostAPIProcessMVPStability|TestWaitForStabilityWorkflowsRejectsMatchAfterDeadline|TestOpenStabilityDatabaseReadOnly)$' \
  -count=1 ./...
```

黑盒三个子场景全部 PASS、无 SKIP：

- approval、Skill digest、加密 checkpoint 与已完成工具跨已持久化边界的停止/重启保持一致；
- Provider 503 的失败 run/route 可审计，同一 workflow 经 HTTP requeue 后成功；
- 未注册工具失败关闭，不产生 approval 或工具副作用。

稳定性门禁完成 150 个正式 workflow 和 2 个暖机 workflow。正式波次 waiting-approval
收敛为 1.19–1.27 秒；暖机 FD 为 13，三个正式波次均为 14；暖机 RSS 约 24.7–26.8 MiB，
正式波次约 32.1–34.1 MiB。重启后前 100 条状态复核通过，Provider 请求数、AgentRun、
route、worker/heartbeat error 和最终 lease 均满足门禁。

这些数值只用于目标机器上的卡死、重复执行和资源持续增长回归，不是容量或生产 SLO。

## 5. 已修复缺陷

| 级别 | 问题 | 修复 |
|---|---|---|
| P1 | requeue 复用 RouteID，成功 outcome 覆盖历史失败 | `9fe1459` 使用 AgentRunID 与持久化 LLM 调用序号 |
| P1 | workflow 丢失失败 StepResult 的诊断引用 | `62020ed` 保留 AgentRunID/AuditRef |
| P1 | 初次 Agent 失败未终结 run | `c98e89f` 统一写入 failed terminal summary |
| P1 | queued create/requeue 可并发启动多个执行 goroutine | `a8c4130` 改为容量 1 wake channel 与单次 worker 启动 |
| P2 | 稳定性轮询可能在 deadline 后误报成功 | `dabbef6` 使用共享 deadline context 并增加慢响应回归 |
| P2 | 验收以会 migration 的 Store opener 核对 SQLite | `dabbef6` 改为 SQLite `mode=ro` 直查并验证写入失败 |
| P2 | 退出输出扫描和资源工具环境分类不完整 | `dabbef6` 先停止再扫描，增加真实预检和命令 deadline |

## 6. 未关闭问题

- **P0：0。**
- **P1：0。**
- **P2：2。**
  1. `examples/host-api` 对启动配置或 `ListenAndServe` 错误仍使用 `panic`，会输出 Go stack；
     配置正确时不影响运行，规避方法是按 README 显式配置 OIDC/Keychain/runtime 环境并由
     本机 supervisor 管理进程。后续可改为结构化 stderr 和明确退出码。
  2. CLI 尚未实现 signal-aware drain/graceful shutdown；当前安全操作方式是在没有进行中的
     HTTP 决策、queued worker 计数稳定且队列已进入空闲窗口后再停止。lease 能恢复中断的
     workflow，但不能消除第 7 节所述外部副作用提交窗口。后续可增加停止接单、等待 active
     execution 归零并取消 worker context 的显式 shutdown 流程。
  两项都有明确规避方法，首个 MVP tag 不因此阻塞。
- **P3：0。**

## 7. 已知限制与运行约束

1. **任意崩溃点不保证 exactly-once。** 已验证声明的安全空闲或已持久化边界上的停止/重启、
   approval lease 冲突和精确
   HTTP 重试不会重复工具，但若进程在外部工具已产生副作用、checkpoint 尚未持久化的窗口被
   `SIGKILL`，runtime 无法仅靠本地 SQLite 与外部系统做原子提交。不可逆写工具必须由宿主
   使用幂等 API 或以稳定 ToolCallID 实现去重；本 MVP 不应被描述为可安全重放任意非幂等写。
2. **系统门禁是 macOS 本机门禁。** Host 真实进程测试依赖 CGO、交互登录、已解锁 login
   Keychain、`ps` 和 `lsof`；缺失或无权限时为 `SKIP/blocked`，不是 PASS。
3. **真实 Provider 是外部依赖。** `provideracceptance` 需要调用方提供 endpoint/model/key，
   受服务可用性和模型行为影响，不进入无凭证默认 CI；缺少配置时为 `SKIP/blocked`。
4. **OIDC 生产集成未被 loopback 替代。** 黑盒使用真实 discovery/JWKS 协议的 loopback
   issuer；长期服务仍必须配置可访问的真实 issuer，仓库不提供关闭认证的开发开关。
5. **容量边界只到当前门禁。** 已证明一个进程、一个执行槽、150 个 workflow 的回归目标；
   未证明超过该规模、多 worker、跨进程 worker、限流策略或长期运行容量。

## 8. 发布决策

当前代码和证据满足既定 MVP 退出标准。真实 Host × Qwen 本机试用已完成且没有新增 P0/P1，
因此 checkout 已具备进入各模块 `v0.1.0` tag 独立评审的条件；既有两项 P2 和第 7 节边界
保持不变。

创建 tag、推送远端、发布 registry、安装器或生产部署不属于本次验收，不自动执行。
