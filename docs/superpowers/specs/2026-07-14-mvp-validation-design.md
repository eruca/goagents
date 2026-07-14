# GoAgents MVP 验收与功能冻结设计

**日期：** 2026-07-14

**状态：** 待评审

## 1. 决策摘要

`goagents` 当前实现已达到首个可测试 MVP。自本设计确认起进入功能冻结期：
不再继续扩展 Skill tool projection、动态激活、脚本执行、远程 registry、依赖安装、
多 Agent 或分布式运行时，只验证当前单机宿主闭环是否真实、可恢复、可审计并可复现。

测试期只接受三类改动：

1. 修复阻断 MVP 闭环的缺陷；
2. 为已发现缺陷增加可复现的回归测试；
3. 修正文档、启动脚本或测试夹具中影响复现的错误。

任何新能力先记录为测试观察，不在测试期直接实现。

## 2. MVP 边界

本轮验收的产品定义是：

> 一个本机、单宿主、可恢复、可审计的 Agent/Workflow Runtime；模型只能通过宿主
> 注册并授权的工具产生副作用，workflow、approval、artifact、run 和 Skill 引用可在
> SQLite 与进程重启边界上保持一致。

必须验收的现有能力包括：

- `goagent`：模型循环、typed tools、policy、approval、stream 和 structured output；
- `workflowkit`：步骤编排、checkpoint、SQLite 恢复、审批后继续和 requeue；
- `runkit` / `artifactkit`：run、审计、artifact 与本机持久化；
- `llmkit`：模型选择、路由、健康状态和失败分类；
- `mcpkit/officialsdk`：stdio 与 Streamable HTTP 工具适配；
- `examples/host-api`：HTTP workflow、OIDC approval identity、Keychain 密钥、queued worker、
  查询与事件时间线；
- `skillkit`：可信根发现、digest、准入、run-start activation、受限资源读取、
  workflow `name@digest` 固化及重启恢复；
- `evalkit`：授权、提示注入、资源边界和内容漂移的安全评估。

本轮不把 examples 伪装成已发布的多租户产品，也不承诺远程 registry、自动安装、
脚本沙箱、动态 Skill activation、request-scoped Skill tool projection、多 Agent handoff
或分布式 worker。

## 3. 功能冻结规则

测试期遵循以下规则：

- `main` 是唯一验收基线；每轮测试记录起始 commit。
- 不为通过测试硬编码路径、digest、时间或特定 fixture 输出。
- 缺陷修复必须先复现失败，再做最小修改并补回归证据。
- 不顺手重构相邻模块，不调整已经稳定的公共接口。
- 测试数据、日志和报告不得保存 token、Keychain 内容、OIDC secret、Skill 正文或本机
  绝对信任路径。
- P0/P1 缺陷修复后重新运行受影响 smoke 和 `scripts/verify-all.sh`。
- P2/P3 可以进入已知问题清单，但不得无声忽略。

## 4. 验收层次

### 4.1 自动化基线

每个候选 commit 必须通过：

```bash
cd examples/host-api
go test ./... -count=1
go vet ./...
go test -tags hostapisystemsmoke \
  -run '^(TestHostAPIProcessToolApprovalSurvivesRestart|TestHostAPIProcessLoadsConfiguredSkillRootAcrossRestart)$' \
  -count=1
go vet -tags hostapisystemsmoke ./...
cd ../..
bash ./scripts/verify-all.sh
git diff --check
```

该层证明模块契约、race 覆盖、示例运行、MCP adapter、SQLite resume、Keychain restart
和 Skill restart 没有回归，但不能代替真实 provider 与黑盒 operator 验收。

### 4.2 真实模型 Provider

使用一个用户明确配置的 OpenAI-compatible endpoint 和模型，至少验证：

1. 普通文本回答；
2. 一次真实 typed tool call，tool observation 必须进入下一轮模型上下文；
3. structured output 成功与 schema 失败；
4. provider 超时、限流或不可用时产生正确错误分类，不伪造成功；
5. 日志、事件和错误中不出现 API key。

endpoint、模型名和凭证来自进程环境或本机安全存储，不写入仓库。若没有可用 provider，
该组状态为 `blocked`，不能用 fake provider 代替并宣称完成。

### 4.3 Host 黑盒闭环

通过真实 `host-api` 二进制和 HTTP 接口完成：

1. 创建不带 Skill 的 workflow 并取得终态；
2. 使用真实 `workflow-review` Skill 创建 workflow，响应固化完整 `name@digest`；
3. 触发需要 operator approval 的工具，完成 OIDC identity 校验与 Keychain 解密；
4. 停止并重启进程，确认 workflow、run、approval、artifact 和 Skill digest 可查询；
5. 对允许重排的失败 workflow 执行 requeue，并核对事件时间线；
6. 未授权工具始终不可见或被拒绝，不能因 Skill root 受信任而获得权限。

验收通过 API 和公开事件观察行为；除定位缺陷外，不直接修改 SQLite 来制造目标状态。

### 4.4 失败关闭与恢复

必须覆盖以下负例：

- 相对、缺失、普通文件或不可读的 `HOST_API_SKILL_ROOT`；
- Skill package digest 在运行前发生变化；
- workflow 请求不存在或不可用的 Skill；
- Skill 请求未授权 tool、host feature、资源或安装行为；
- resource URI 包含绝对路径、`..`、未 allowlist 文件或越界符号链接；
- approval 过期、拒绝、租约冲突和重复提交；
- queued、waiting approval 和已完成工具调用边界上的进程中断与重启。

失败必须返回稳定、可分类且不含秘密的错误；不得静默升级 digest、扩大工具集、重复执行
已确认副作用或丢失审计记录。

### 4.5 干净环境复现

在不依赖现有构建缓存和隐式 shell 环境的 checkout 中验证：

- `go work` 仅用于本仓库开发，不掩盖发布模块的错误依赖；
- README 中的 host 启动步骤可执行；
- SQLite、LLMKit home、OIDC、Keychain identity 和 Skill root 均由显式配置决定；
- 未配置 `HOST_API_SKILL_ROOT` 时保持原行为；
- 删除临时 runtime home 后不会遗留测试凭证或 Keychain item。

### 4.6 并发与稳定性

本轮不凭空设定生产吞吐量。开始负载测试前，先明确目标机器、provider 限额、预期并发
workflow 数和可接受延迟。最低要求是：在该明确目标下，无数据竞争、无重复副作用、
无永久 lease、无 goroutine/文件描述符持续增长，重启后队列能够继续收敛。

## 5. 测试证据

每个场景记录：

- 测试 ID、起始 commit、时间和操作系统；
- 使用的非秘密配置，例如 provider 类型、模型名和是否启用 Skill；
- 执行命令或 HTTP 请求摘要；
- 期望结果、实际结果和退出码；
- 相关 workflow/run/approval ID；
- 已脱敏日志或响应片段；
- 结论：`pass`、`fail` 或 `blocked`；
- 若失败，关联缺陷编号和修复 commit。

进入 `evalkit` 的自动化场景还必须分别记录：

- Outcome：领域终态、host-owned output ref 和稳定错误分类；
- Trajectory：脱敏后的 workflow step 与 agent event，以及策略 grader 结论；
- Efficiency：input/output token、LLM call、tool call 和预算 grader 结论；
- 固定指纹：commit、provider、model alias、Agent definition hash、Prompt 版本、完整
  Skill `name@digest` 与可见工具 ID。

固定指纹和轨迹采用字段白名单；不得复制 Prompt、Skill 正文、event message、工具输入输出、
原始 provider 错误或未知 metadata。

不得把完整数据库、Keychain 数据、access token、API key、OIDC secret 或未脱敏环境导出
作为测试附件。

## 6. 缺陷分级

- **P0：** 数据丢失、权限绕过、秘密泄露、重复执行不可逆副作用，或无法安全停止。
- **P1：** 核心闭环不可完成、重启不能恢复、审批或 Skill digest 语义错误、干净环境无法启动。
- **P2：** 有明确规避方法的非核心错误、诊断不足或局部兼容问题。
- **P3：** 文案、体验和不影响正确性的改进。

P0/P1 阻止 MVP 验收；P2/P3 必须记录，但是否进入首个 tag 由已知问题审查决定。

## 7. 退出标准

只有同时满足以下条件，MVP 才从“实现完成”进入“验收完成”：

1. 自动化基线在目标 commit 上全绿；
2. 至少一个真实模型 provider 完成文本、工具与 structured output 验收；
3. Host 黑盒闭环和进程重启恢复全部通过；
4. 安全负例均失败关闭，未发现路径、正文、凭证或密钥泄露；
5. 干净环境可按公开文档复现；
6. 目标负载已明确定义并通过稳定性测试；
7. 未关闭 P0/P1 为零；
8. 所有 P2/P3 和环境限制已进入已知问题清单。

满足后再决定是否创建各模块的 `v0.1.0` tag。测试期本身不自动发布、不推送远端、
不创建 registry 或安装器。

## 8. 执行顺序

1. 建立自动化基线与测试台账；
2. 验证真实模型 provider；
3. 执行 Host 黑盒主流程；
4. 执行失败注入与重启恢复；
5. 做干净环境复现；
6. 确认负载目标后做并发与稳定性测试；
7. 汇总缺陷和已知限制，做 MVP 验收决策。

该顺序优先暴露配置和真实 provider 阻塞，避免在基础闭环未成立时过早进行负载测试。
