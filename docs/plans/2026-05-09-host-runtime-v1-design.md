# Host Runtime v1 设计文档

## 结论

下一步应把 `examples/host-api` 和 `examples/host-runtime` 收敛成一套
canonical host runtime v1，而不是继续扩 `goagent` core，也暂不抽新的
`hostkit` module。

`goagent` 已经能表达单次可审计 ReAct run；当前更需要补齐的是 host 如何把
workflow、agent run、artifact、LLM routing 和人工审批组合成一个可恢复、可审计、
可治理的运行时参考架构。

## 设计目标

- 固定 host runtime 的对象模型和引用关系。
- 明确同步执行、预留队列执行、审批恢复和取消的边界。
- 明确 workflow、agent run、artifact、LLM route audit 的持久化职责。
- 建立 strict audit 的写入顺序，避免关键审计静默丢失。
- 给 capability modules 提供一致接入规范。
- 让后续实现可以按小任务推进，而不是一次性抽象新框架。

## 非目标

- 不把 `examples/host-api` 包装成生产级 HTTP server。
- 不在本阶段抽 `hostkit`、`runtimekit` 或其他新模块。
- 不修改 `goagent` core 的 ReAct loop。
- 不引入分布式 worker、Redis lease、cron、dashboard、认证或租户模型。
- 不在 workflow/run store 中保存 raw prompt、raw model response、raw tool input、
  full tool output、document payload 或 API key。

## 上层对象模型

Host runtime v1 的核心对象链路是：

```text
Task
  -> WorkflowRun
    -> AgentRun
      -> RouteTrace
      -> ArtifactRef
      -> TerminalSummary
```

对象职责：

- `Task`：host 接收到的业务请求，包含 host-owned id、输入引用、task profile 和运行模式。
- `WorkflowRun`：由 `workflowkit` 管理，记录应用生命周期、当前 step、审批状态和 refs。
- `AgentRun`：由 `runkit` 管理，记录单次 agent run 的生命周期事件和 terminal summary。
- `RouteTrace`：由 `llmkit` audit 管理，记录模型/账号选择、候选解释、provider outcome 和业务反馈。
- `ArtifactRef`：由 `artifactkit` 管理，指向输入、agent output、final output 或大工具结果。
- `TerminalSummary`：由 host 在 agent run 完成后写入，包含 token usage、call counts、used tools、abort reason 和 content ref。

## 模块职责

```text
workflowkit
  owns workflow state, step records, approval wait/resume, lifecycle guards

goagent
  owns one ReAct-style agent run, stage events, policy, approval gate, summary

llmkit
  owns task-profile-based routing, health snapshot, route/outcome audit

artifactkit
  owns full payload storage behind refs

runkit
  owns agent run records, bounded lifecycle events, terminal summaries

host application
  owns HTTP API, auth, tenant/project policy, task profile, provider clients,
  capability wiring, artifact content policy, strict audit ordering
```

组合模块可以 import 多个 core modules。基础模块之间仍保持单向边界：`goagent`
不 import `workflowkit`、`llmkit`、`artifactkit`、`runkit` 或 domain capability
modules。

## Runtime Home

Host runtime v1 采用一个可重建的 runtime home：

```text
$HOST_RUNTIME_HOME/
  workflow.db
  agent-runs.db
  artifacts/
  .llmkit/
    config.yaml
    route-events.jsonl
    outcomes.jsonl
    model-stats.json
```

解析规则：

1. `HOST_RUNTIME_HOME` 显式存在时使用该目录。
2. 未设置时，示例程序可以创建临时目录，保持 demo 体验。
3. `LLMKIT_HOME` 显式存在时优先使用。
4. 未设置 `LLMKIT_HOME` 时，默认使用 `$HOST_RUNTIME_HOME/.llmkit`。

生产 host 可以替换任意 backend，但要保持同一类 refs 和 bounded metadata 语义。

## 执行模型

第一版只实现同步执行，继续保留 `queued` 作为显式未实现模式。

```text
POST /workflows
  -> validate request
  -> write input artifact
  -> create workflow run
  -> execute steps synchronously until waiting_approval or terminal
  -> return workflow state

POST /workflows/{id}/approve
  -> record approval metadata and audit ref
  -> continue workflow synchronously
  -> return terminal or failed workflow state
```

`run_mode` 语义：

- empty：等同 `sync`。
- `sync`：同步推进 workflow。
- `queued`：返回 `unsupported_run_mode`，不创建半成品队列语义。

后续如果支持 queued，必须先补 worker contract：

- pending/runnable workflow claim。
- lease owner、lease deadline、heartbeat。
- worker crash 后 stuck run recovery。
- retry 与 idempotent resume。
- cancel 与 waiting approval 的交互。

这些不是 host runtime v1 第一阶段内容。

## 状态机

Workflow 状态继续使用 `workflowkit` 的状态集：

```text
pending -> running
running -> waiting_approval
running -> succeeded
running -> failed
running -> cancelled
waiting_approval -> running
waiting_approval -> cancelled
```

Host runtime v1 只允许：

- `Run` 从 empty/pending 开始。
- `Approve` 从 `waiting_approval` 继续。
- `Cancel` 对 pending/running/waiting_approval 生效。
- terminal 状态不可继续。

Step 输出中的 `OutputRef`、`AuditRef`、`AgentRunID`、`ApprovalRef` 都必须是
host-owned refs，不存 raw payload。

## 持久化职责

`workflowkit/sqlitestore`：

- workflow id、status、current step、completed steps、step attempts。
- step records。
- `InputRef`、`OutputRef`、`AgentRunID`、`AuditRef`、`ApprovalRef`。
- small metadata。

`runkit/sqlitestore`：

- agent run id。
- workflow/task correlation。
- bounded lifecycle events。
- terminal summary。

`artifactkit.FileStore`：

- input payload。
- agent output payload。
- final output payload。
- full OCR/retrieval/tool payload when host decides to retain it。

`llmkit` audit files：

- sanitized route decisions。
- candidate-level score/filter explanations。
- provider outcome。
- business outcome signal。
- model stats derived from append-only audit files。

## Strict Audit 顺序

`WithEventSink` 适合观察，但不适合 strict audit，因为 runtime 不应因普通观察失败而
中断 agent run。生产 host 如果要求审计失败即失败，应使用 host-side wrapper 明确控制
写入顺序。

推荐顺序：

```text
1. create workflow run
2. write input artifact
3. create agent run record before first agent event
4. record route trace before provider call
5. record provider outcome after provider call
6. append bounded agent events during stream
7. write agent output artifact
8. write terminal summary with content ref
9. update workflow step result with output/agent refs
10. on approval, record approval audit ref and business outcome
11. write final artifact
12. mark workflow succeeded
```

错误处理原则：

- 写 route trace 失败：provider call 不应继续，避免不可审计调用。
- 写 provider outcome 失败：agent run 应返回错误或 workflow failed，避免成功但无 outcome。
- 写 terminal summary 失败：workflow step 不应标记成功。
- 写 artifact 失败：只保留 refs 不够，必须让 workflow failed 或停在可恢复状态。
- 普通 UI/metrics sink 失败：不影响 run。

## LLM 路由治理

Host runtime v1 通过 host-provided `TaskProfile` 明确 routing intent。

推荐 preset：

- `simple_local`：简单、低失败成本、本地优先。
- `balanced`：中等复杂度、云可用。
- `high_success`：高失败成本、需要 reasoning、优先成功率。
- `local_only`：只允许本地候选。

预算边界：

- `TaskProfile.MaxEstimatedCents` 是 llmkit 内部 per-task hard filter。
- project/account/tenant/monthly budget 属于 host governance。
- budget failure 不应被普通 provider fallback 吞掉。

Provider health 边界：

- `MemoryHealthStore` 适合单进程。
- 多副本 host 需要 shared health store，但这不是 v1 第一阶段。
- health store 不保存 prompt、response、header 或 API key。

业务反馈：

- 人工 approval 可以写入 llmkit business outcome。
- provider success 和 human accepted 是两类信号，不能混成一个。

## Capability 接入规范

任何 OCR、retrieval、filesystem、database、domain calculation 能力都应通过 host
工具暴露给 `goagent`，并遵守以下规则：

- tool input 使用 host-owned id，例如 `document_id`、`artifact_ref`、`query_id`。
- 不允许模型直接传任意 filesystem path。
- 大结果写入 artifact store。
- `ForLLM` 只返回 bounded preview、summary 或 ref。
- `ForUser` 可以返回完整内容，但 host 要决定是否持久化。
- 需要后续读取时，提供 read/search tool，而不是一次性把全文塞回上下文。
- mutating tools 必须声明 permission，并通过 policy/approval gate。

`contextkit` 的定位是 model-facing projection，不是 session truth。原始消息和完整
tool payload 仍归 host store 或 artifact store 管。

## HTTP Surface

Host API v1 最小 surface：

- `POST /workflows`
- `GET /workflows/{id}`
- `POST /workflows/{id}/approve`
- `GET /workflows/{id}/llm-routes`
- `GET /agent-runs/{id}`
- `GET /llmkit/models`

暂不增加：

- workflow event stream。
- SSE/WebSocket。
- command bus。
- replay endpoint。
- background worker endpoint。

原因是当前底层 contract 先要把 durable refs 和 strict audit 顺序跑通。

## 测试策略

设计验收优先看跨模块行为，而不是单个 happy path。

必测路径：

- 创建 workflow 后重建 server，仍可 `GET /workflows/{id}`。
- 重建后 approval 可以继续 workflow 并生成 final artifact。
- agent run record、events、terminal summary 可重建读取。
- route audit 可按 workflow/task id 查询。
- `simple_local` 路由到本地候选。
- `high_success` 路由到 advanced 候选。
- `queued` 返回 `unsupported_run_mode`，且不创建误导性工作流。
- artifact file store reopen 后可读取 payload。
- strict audit wrapper 中 route trace 写入失败会阻止 provider call。

验证入口：

```bash
(cd artifactkit && GOWORK=off go test ./...)
(cd runkit && GOWORK=off go test ./...)
(cd workflowkit && ./scripts/verify-e2e.sh)
(cd examples/host-api && GOWORK=off go test ./...)
(cd examples/host-runtime && GOWORK=off go test ./...)
./scripts/verify-all.sh
```

## 当前实现状态

- Phase 1 durable host baseline 已落地在 `examples/host-api`：`HOST_RUNTIME_HOME`
  组合 `workflowkit/sqlitestore`、`runkit/sqlitestore`、`artifactkit.FileStore` 和
  `$HOST_RUNTIME_HOME/.llmkit`，并已有 reopen/resume 测试。
- `examples/host-runtime` 保持轻量内存 skeleton，但同样展示 workflow、artifact、
  run audit 和 llmkit route/outcome audit 的组合方式。
- Phase 2 的关键 strict persistence 语义已覆盖到 `examples/host-api` 和
  `examples/host-runtime`：agent output artifact 或 terminal summary 写入失败时，
  agent review step 返回失败，不进入 `waiting_approval`。
- Phase 3 的 capability/ref 接入原则已有 `workflowkit/examples/ocr-review` 和
  `goagent/examples/artifacts` 作为示例：raw payload 存 artifact，model-facing 内容使用
  bounded preview 或 ref。
- Phase 4 queued worker 和 Phase 5 `hostkit` 抽取仍未进入实现阶段，按设计继续推迟。

## 分阶段实施建议

### Phase 1: 固定 durable host baseline

- 确认 `artifactkit.FileStore`、`workflowkit/sqlitestore`、`runkit/sqlitestore` 和
  `LLMKIT_HOME` 能组成同一个 runtime home。
- 补 reopen/resume 测试。
- 保持 `sync` 执行模型。

### Phase 2: Strict audit runner

- 在 host example 内增加 explicit runner/wrapper。
- 用 `Agent.Stream` 或等价 host wrapper 控制 agent events、terminal summary 和 artifacts 的写入顺序。
- 覆盖审计写入失败的回归测试。

### Phase 3: Capability wiring contract

- 选择一个真实 capability，例如 OCR 或 retrieval。
- 按 `artifact_ref + bounded preview + read/search tool` 模式接入。
- 验证 raw payload 不进入 workflow/run metadata。

### Phase 4: Queued execution design

- 只有当前三步稳定后，再设计 worker claim/lease。
- 不在没有 lease contract 的情况下实现假队列。

### Phase 5: 决定是否抽模块

只有当 `examples/host-api` 和真实业务 host 都重复需要相同 runner/config/store 组合时，
再评估抽 `hostkit`。抽象前需要满足：

- 至少两个 host 复用同一套 contract。
- strict audit 顺序稳定。
- queued/worker 是否需要进入公共 API 已经明确。
- 认证、租户、业务权限没有被误抽进通用模块。

## 当前可执行的下一步

建议下一步只做 Phase 1 的实现计划，文件可以放在：

```text
docs/plans/2026-05-09-host-runtime-v1-implementation-plan.md
```

计划应拆成小任务：

1. 补齐/核对 durable store conformance。
2. 增加 host runtime reopen/resume 测试。
3. 收紧 route/profile/approval outcome 的测试。
4. 更新 README 和 OpenAPI contract。
5. 跑 `./scripts/verify-all.sh`。

在 Phase 1 通过前，不建议修改 `goagent` core。
