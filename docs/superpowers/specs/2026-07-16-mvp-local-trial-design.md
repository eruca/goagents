# GoAgents MVP 本机试用设计

**日期：** 2026-07-16

**状态：** 已批准

## 1. 目标

在不修改产品运行代码的前提下，把已经分别通过的真实 Provider 门禁与 Host 黑盒门禁组合成
一次可复跑的本机 operator 试用，确认真实 Qwen 能经过实际 Host 二进制、SQLite 持久化、
OIDC 审批和重启恢复形成完整闭环。

## 2. 方案选择

采用显式 opt-in 的 Go tagged test。相比一次性脚本，它可以复用现有真实进程、loopback OIDC、
Keychain 清理和 HTTP contract helper；相比继续只跑两组分离门禁，它能证明真实 Provider 与
Host composition 的接缝。该测试不进入默认 CI，避免把外部服务波动混入确定性回归。

## 3. 场景

1. 从调用方环境读取 OpenAI-compatible endpoint、模型和 API key；缺少任一项时明确 `SKIP`，
   验收台账按 blocked 处理。
2. 构建并启动真实 `host-api` 二进制，使用临时 runtime home、独立 `.smoke.` Keychain 项和
   loopback OIDC issuer。
3. 通过 HTTP 创建真实模型 workflow，要求进入 final approval 等待态，并核对 AgentRun 与
   llmkit 成功 route。
4. 使用无效 bearer token 请求审批，必须返回 401 且 workflow 不变。
5. 在已持久化等待态停止并重启 Host，使用有效 OIDC token 完成审批。
6. 再次重启，复核 workflow、AgentRun、route 和事件时间线仍可查询且一致。

## 4. 安全与清理

- API key 只通过命名环境变量传给 Host 子进程，不写入配置、文档或测试输出。
- 测试不记录 endpoint、Prompt、模型正文、bearer token 或原始 Provider 错误。
- Keychain service 使用 `goagents.host-api.approvals.smoke.` 前缀，只删除本次创建的精确项。
- runtime home、OIDC issuer、二进制和配置均位于测试临时目录并由测试清理。

## 5. 验收标准

- tagged test 的所有断言 PASS，且无 SKIP；
- 真实模型 route outcome 为 success；
- 无效审批为 401，有效审批后 workflow 为 succeeded；
- 两次安全边界重启后持久化和审计一致；
- Host 输出不包含真实 API key、endpoint、OIDC token 或本机 Skill 路径；
- 默认 `go test ./...` 与 workspace 全量验证不回归。

本阶段只据此判定本机 MVP 是否出现新 P0/P1，不扩大到生产 OIDC、多 worker、分布式执行、
长期容量或任意崩溃点 exactly-once。
