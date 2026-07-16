# GoAgents MVP 本机试用记录

**日期：** 2026-07-16

**产品代码基线：** `8e1bd88`

**组合门禁提交：** `bc1e327`

**目标环境：** Apple M1 Pro、10 核、16 GiB、macOS 26.5.1

**结论：** PASS——真实 Host × Qwen 本机 operator 闭环通过，无 SKIP、无新增 P0/P1；
checkout 可以进入独立的多模块 `v0.1.0` tag 评审，但本记录不创建或推送 tag。

## 1. 试用边界

本轮只补齐此前两组分离证据之间的组合接缝：真实 OpenAI-compatible Qwen Provider 进入实际
`host-api` 二进制，经 SQLite、AgentRun/llmkit 审计、OIDC final approval 和安全边界重启
完成一个 workflow。

OIDC issuer/JWKS 与 bearer token 仍是 loopback 合成环境，Keychain 使用本轮唯一的
`goagents.host-api.approvals.smoke.` 项。该结果不等价于生产 IdP、多 worker、分布式执行、
长期容量或任意崩溃点 exactly-once。

## 2. 可复跑命令

调用方在本机安全环境提供：

```bash
export OPENAI_COMPAT_BASE_URL=...
export OPENAI_COMPAT_MODEL=...
export OPENAI_COMPAT_API_KEY=...

cd examples/host-api
go test -v -tags 'hostapisystemsmoke provideracceptance' \
  -run '^TestHostAPIProcessRealProviderLocalTrial$' \
  -count=1 ./...
```

缺少任一 Provider 变量时，门禁已验证会明确 `SKIP`；这代表 blocked，不计为 PASS。真实配置
来自既有本机 `todo/.env` 映射，值未被复制到仓库或输出。

## 3. 观察结果

| 检查点 | 结果 | 证据 |
|---|---|---|
| 实际 Host 二进制启动 | PASS | 临时 runtime home、独立 Keychain 项、loopback OIDC |
| llmkit 真实模型 composition | PASS | 只暴露一个 `openai_compatible` 候选，route outcome success |
| workflow 创建 | PASS | HTTP 202，进入已持久化 `waiting_approval` |
| AgentRun | PASS | succeeded，1 次 LLM call、0 次 tool call |
| 无效 bearer token | PASS | final approval 返回 401，workflow/approval ref 不变 |
| 第一次重启 | PASS | workflow、input/output ref、AgentRunID、approval ref 一致 |
| 有效 OIDC approval | PASS | workflow 转为 succeeded，final output ref 固化 |
| 第二次重启 | PASS | workflow、AgentRun、成功 route 与事件时间线均可查询 |
| 敏感输出 | PASS | Host 输出不含 API key、endpoint 或 bearer token |
| race/static/default regression | PASS | tagged `-race`、tagged `go vet`、默认 Host tests 均退出 0 |

首次真实组合命令与 `-race` 重跑均 PASS、无 SKIP。测试不记录 Prompt、模型正文、endpoint、
凭证、token、临时路径或原始 Provider 错误。

## 4. 缺陷分级

- **新增 P0：0。**
- **新增 P1：0。**
- **新增 P2：0。**
- **新增 P3：0。**

最终验收记录中的两个既有 P2 不变：启动/监听错误仍使用 `panic`，CLI 仍无 signal-aware
graceful drain。试用严格在已持久化 final-approval 和空闲边界停止进程，没有扩大其保证。

## 5. 发布判断

本机 MVP 的功能冻结、真实 Provider、真实 Host composition、审批、持久化、审计和重启证据
已经闭合。下一阶段应只做各 Go module 的 tag 路径、版本一致性、依赖边界和发布内容审查；
不应在 tag 前继续加入新功能。
