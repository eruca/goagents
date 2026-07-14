# Host API MVP 统一黑盒验收设计

**日期：** 2026-07-14

**状态：** 已实现并验收

## 1. 目标

在不扩展生产功能的前提下，用一个可重复的真实进程 smoke 证明 Host API 的 MVP 主闭环：

- 真实 `host-api` 二进制通过公开 HTTP API 工作；
- workflow、Agent tool approval、OIDC、Keychain、Skill digest 和 LLM route 跨进程重启保持一致；
- Provider 暂时不可用时 workflow 失败，恢复后可通过 requeue 收敛；
- 未注册工具调用失败关闭，不产生审批或工具副作用；
- 验收过程不读取或修改 SQLite 来制造目标状态。

当前 `main@ebe49f7` 上已有两个 tagged system smoke 已重新执行并通过，且没有 `SKIP`：

- `TestHostAPIProcessToolApprovalSurvivesRestart`；
- `TestHostAPIProcessLoadsConfiguredSkillRootAcrossRestart`。

本设计补的是统一、公开 API 驱动的验收证据，不替换这两个职责单一的回归测试。

## 2. 范围

新增一个仅在以下 build tags 下编译的测试文件：

```text
darwin && cgo && hostapisystemsmoke
```

顶层测试命名为：

```text
TestHostAPIProcessMVPBlackBoxClosure
```

测试使用三个独立子场景，共用现有的二进制构建、OIDC、进程启停、HTTP JSON 和 Keychain
隔离 helper。每个子场景使用独立 runtime home，避免状态串扰。

黑盒测试本身不修改：

- 生产 HTTP API；
- SQLite schema 或 store contract；
- Agent、workflow、路由或重试策略；
- Qwen 配置或真实凭证；
- `scripts/verify-all.sh` 的默认外部环境要求。

若验收暴露产品缺陷，先按 P0-P3 分级，再单独设计最小修复；不得在 smoke 中硬编码绕过。

实际验收暴露了三项 P1 可观测性缺陷，均以独立语义提交做了最小修复；HTTP API、SQLite
schema、路由策略和重试策略仍未改变。详见第 9 节。

## 3. 本地 OpenAI-compatible Provider Stub

测试启动一个仅监听 loopback 的 `httptest.Server`，实现最小 `/v1/chat/completions` 契约。
Stub 使用互斥锁保护模式和请求记录，允许测试在 Host 进程运行期间确定性切换状态：

| 模式 | 行为 |
|---|---|
| `ready` | 无工具时返回文本；请求含工具且尚无 tool observation 时返回 `record_review` tool call；收到 observation 后返回最终文本 |
| `unavailable` | 返回 HTTP 503 和固定非秘密错误正文 |
| `unregistered_tool` | 返回名为 `unregistered_tool` 的 tool call，验证未知工具失败关闭 |

Stub 同时验证：

- 请求路径是 `/v1/chat/completions`；
- Authorization 使用测试专用合成值；
- 请求中只有当前 task profile 允许的工具；
- tool observation 确实进入下一轮 messages。

每个 runtime home 写入临时 `LLMKIT_HOME/config.yaml`，只保存 loopback base URL、测试模型名和
`api_key_env` 名称。合成 API key 仅通过子进程环境注入，不写入仓库，也不使用 todo 的真实 Qwen key。

## 4. 子场景

### 4.1 审批、Skill 与重启

1. 检查登录 Keychain 可用，创建带 `.smoke.` 前缀的唯一 service，并注册精确清理函数；
2. 启动本地 OIDC provider、`ready` Provider stub 和真实 Host 二进制；
3. 通过 `GET /skills` 取得 `workflow-review` 的 64 位 digest；
4. 创建不带 Skill、`needs_tools=true` 的 workflow，确认只出现 `record_review` 待审批工具；
5. 使用无效 bearer token 调用 agent approval，确认返回 401；
6. 停止并重启 Host，通过 `GET /workflows/{id}` 确认待审批状态仍可见；
7. 使用有效 OIDC token 完成 agent tool approval 和最终 workflow approval；
8. 创建引用 `workflow-review` 的 instruction-only workflow，确认响应固化完整 `name@digest`，
   然后完成最终 approval；
9. 再次重启，通过公开接口读取 workflows、Agent runs、LLM routes、events 和 Skill 列表；
10. 确认 workflow/run 状态、tool call 数、route outcome、Skill digest 和 `input_ref/output_ref`
    在重启后保持一致；
11. 精确删除 smoke Keychain item。

Host API 当前没有 artifact 正文读取端点。本轮把公开 workflow/run 响应中的稳定
`input_ref/output_ref` 视为 artifact handle 的黑盒证据，不新增 artifact 内容 API。

### 4.2 Provider 失败与 requeue

1. 以 `unavailable` 模式启动 Stub 和真实 Host；
2. 通过 HTTP 创建 queued workflow，并轮询 `GET /workflows/{id}` 直到 `failed`；
3. 通过 `GET /workflows/{id}/llm-routes` 确认 Provider outcome 为
   `provider_error/transient_error`；
4. 把同一个 Stub 切换为 `ready`；
5. 调用 `POST /workflows/{id}/requeue`，确认仍是同一 workflow ID；
6. 轮询到等待最终 approval，使用有效 OIDC token 完成 approval；
7. 通过 `GET /workflows/{id}/events` 确认先有失败步骤，再有 `workflow_requeued`，最终成功；
8. 不直接打开 workflow SQLite 或写入 artifact store。

### 4.3 未注册工具失败关闭

1. 以 `unregistered_tool` 模式启动 Stub 和真实 Host；
2. 创建 `needs_tools=true` 的 queued workflow；
3. 轮询到 `failed`，确认没有 agent approval；
4. 读取 Agent run/events，确认没有 `tool.started` 或 `tool.completed`；
5. 确认 Stub 收到的工具清单不包含 `unregistered_tool`，证明 Provider 返回的是越权调用而非
   Host 授权；
6. 检查错误响应、进程日志和公开事件不包含合成 key、Keychain 内容或本机 Skill 物理路径。

## 5. HTTP-only 边界

新增 smoke 的断言只能使用：

- `GET /skills`；
- `POST /workflows`；
- `GET /workflows/{id}`；
- `POST /workflows/{id}/agent-approve`；
- `POST /workflows/{id}/approve`；
- `POST /workflows/{id}/requeue`；
- `GET /workflows/{id}/events`；
- `GET /workflows/{id}/llm-routes`；
- `GET /agent-runs/{id}`。

SQLite、Keychain CLI 和临时文件只用于测试环境生命周期管理：

- SQLite 不参与结果断言；
- Keychain CLI 只清理带精确 smoke service/account 的测试 item；
- 临时 `config.yaml` 只用于启动 Host，验收结束由 `t.TempDir()` 清理。

## 6. 失败处理

- 登录 Keychain 不可访问：测试可 `SKIP`，但 MVP 验收台账必须记为 `blocked`，不能记作通过；
- Stub 请求协议不符合预期：测试立即失败并只输出脱敏摘要；
- Host 未在时限内收敛：输出公开 workflow/worker 状态和脱敏进程日志，不读取 SQLite补证；
- requeue 后再次失败：保留失败状态和 route/event 证据，不循环重试；
- Keychain 清理失败：测试失败，且清理器继续坚持 `.smoke.` 前缀保护，不扩大删除范围。

## 7. 验证命令

实现后至少执行：

```bash
cd examples/host-api
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPBlackBoxClosure$' \
  -count=1 ./...
go test ./... -count=1
go vet ./...
go vet -tags hostapisystemsmoke ./...
cd ../..
bash ./scripts/verify-all.sh
git diff --check
```

验收记录必须明确列出三个子场景的 PASS/FAIL/SKIP。只有全部 PASS，且没有秘密或物理路径
泄露，MVP 的 Host 黑盒闭环才可进入完成状态。

## 8. 后续阶段边界

该 smoke 完成后按既定顺序继续：

1. 在干净 worktree 按 README 从零复现；
2. 明确单机 MVP 负载目标并执行稳定性测试；
3. 汇总 P0-P3、环境限制和验收证据，形成 MVP 最终结论。

这些阶段不与本次 smoke 实现混在同一语义提交中。

## 9. 实现与验收记录

### 9.1 交付提交

| 提交 | 内容 |
|---|---|
| `4330030` | 审批、Skill 与重启黑盒场景及可控 Provider stub |
| `9fe1459` | 修复重排与多轮调用复用 RouteID 导致历史 Outcome 被覆盖 |
| `96cb5f4` | Provider 503、HTTP requeue 与恢复成功场景 |
| `62020ed` | `workflowkit` 保留失败步骤返回的 AgentRunID/AuditRef |
| `c98e89f` | Host 把初次执行失败的 AgentRun 正确终结为 `failed` |
| `90d00ff` | 未注册工具失败关闭场景 |
| `8f76103` | Provider 失败场景校验失败 AgentRun 终态 |
| `c2a3926` | 显式校验进程日志不泄露 Skill 物理路径 |

### 9.2 验收中发现并修复的问题

| 级别 | 问题 | 影响 | 处理结果 |
|---|---|---|---|
| P1 | 同一 workflow 重排时复用 RouteID | 后一次成功 Outcome 覆盖前一次 Provider 失败证据 | RouteID 改为 AgentRunID 加持久化 LLM 调用序号，已修复 |
| P1 | 执行器丢弃失败 StepResult 中的诊断引用 | 失败 workflow 无法关联 AgentRun | 失败路径保留并持久化诊断引用，已修复 |
| P1 | 初次 Agent 执行失败未写 terminal summary | AgentRun 长期停留在 `running` | 统一失败运行收口为 `failed`，已修复 |

没有发现 P0、P2 或 P3 缺陷。

### 9.3 黑盒结果

2026-07-14 执行：

```text
TestHostAPIProcessMVPBlackBoxClosure/approval_skill_and_restart             PASS
TestHostAPIProcessMVPBlackBoxClosure/provider_failure_requeue_and_success  PASS
TestHostAPIProcessMVPBlackBoxClosure/unregistered_tool_fails_closed        PASS
```

三个子场景均为 PASS，没有 SKIP。断言只通过第 5 节列出的公开 HTTP API 获取产品状态；没有读取、
修改 SQLite 或直接写入 artifact store 来制造结果。

### 9.4 回归证据

以下命令均以退出码 0 完成：

```bash
cd examples/host-api
go test -v -tags hostapisystemsmoke -run '^TestHostAPIProcessMVPBlackBoxClosure$' -count=1 ./...
go test ./... -count=1
go vet ./...
go vet -tags hostapisystemsmoke ./...
cd ../..
bash ./scripts/verify-all.sh
git diff --check
```

仓库级验证覆盖所有 Go 模块、`workflowkit` 与 `goagent` race、MCP stdio/HTTP smoke、示例程序和
Host API 回归。

### 9.5 安全边界结果

- Provider 使用测试专用合成 key，没有读取 todo/Qwen 的真实凭证；
- 进程输出显式检查不包含合成 key 或配置的 Skill 绝对路径；
- `/skills` 只返回安全目录字段与固定 digest，不返回 manifest、instructions、resources 或 root；
- 每个场景使用唯一 `.smoke.` Keychain service，并通过原有前缀保护清理精确 item；
- 未注册工具场景中 AgentRun 为 `failed`、`ToolCalls=0`、`UsedTools=[]`，且事件没有任何
  `tool.*`，证明策略阶段在工具执行前失败关闭。
