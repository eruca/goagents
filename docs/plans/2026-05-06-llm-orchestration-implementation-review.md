# LLM orchestration implementation review

## 结论

当前实现已经支撑最初的核心目标：host 可以按任务画像选择更合适的 LLM，
简单低风险任务默认走本地免费模型，复杂或高失败成本任务可以走高级云模型，
并且每次选择都能通过审计接口复盘。

但它还不是生产级 LLM gateway。当前稳定边界应保持为：

- `llmkit` 提供 routing、health、audit、stats 和 goagent adapter。
- `examples/host-api` 作为 host-side composition 示例，展示 workflow、
  artifact、run audit、LLM routing 如何组合。
- `goagent` core 继续只依赖 `ports.LLMClient`，不吸收 LLM routing 或 host
  orchestration。

下一阶段不建议立刻抽 `hostkit`。更稳妥的优先级是先把 host-api 的 contract
补齐成可接入示例，包括 OpenAPI、真实 config/provider wiring 示例，以及更清晰
的 profile/audit response。

## 设计目标对照

### 已实现

- 独立 `llmkit` module 已存在，核心包不依赖 `goagent`。
- `llmkit/adapters/goagent` 把 routing 暴露成 `ports.LLMClient`。
- route policy 已支持确定性 filter-then-score：
  - capability
  - price
  - local preference
  - latency
  - recent reliability
  - provider health
- `TaskProfile` 已覆盖 complexity、failure cost、privacy、reasoning/tools/JSON/long context。
- `ModelCapability` 已覆盖 local/cloud、capability level、context window、price、latency、tools/JSON。
- JSONL audit 已存在：
  - `route-events.jsonl`
  - `outcomes.jsonl`
- `model-stats.json` 可由 audit 生成，并可回灌到 candidates。
- `MemoryHealthStore` 已覆盖 provider availability、quota、in-flight、cooldown、failure streak。
- `examples/host-api` 已使用 durable runtime：
  - `workflowkit/sqlitestore`
  - `runkit/sqlitestore`
  - `artifactkit.FileStore`
  - `$HOST_RUNTIME_HOME/.llmkit`
- `POST /workflows` 支持：
  - `run_mode`
  - `task_profile_preset`
  - `task_profile`
- `GET /workflows/{id}/llm-routes` 已能返回 route reason、score breakdown、候选模型和 outcome。
- `queued` run mode 已明确保留并返回 `unsupported_run_mode`，没有伪实现队列。

### 部分实现

- 设计中要求 route audit 能解释完整任务画像。当前 audit 只暴露 `task_type`，
  没有完整返回 complexity、failure cost、privacy 等 profile 字段。
- 设计中提到 fallback chain。当前 adapter 支持 provider 失败后换候选重试，
  但 host-api demo 只有 static provider，尚未展示真实 fallback 链路。
- 设计中提到多账号 API key 和 config。`llmkit` 已有 config loader 和
  OpenAI-compatible provider wiring，但 `examples/host-api` 仍使用内置
  `defaultCandidates()` 和 `staticProvider`。
- 设计中提到历史成功率。`model-stats.json` 已能计算并应用 recent failure/latency，
  但 host-api 没有 endpoint 或启动流程刷新/展示 stats。
- 设计中提到 outcome 应代表任务成功信号。当前 `TaskOutcome.Success` 主要代表 provider
  调用成功，尚未区分 human accepted、schema passed、tool completed 等业务成功信号。

### 未实现

- 真实 provider config 接入 host-api。
- OpenAPI contract。
- 认证、多租户、权限、配额。
- workflow-level events 或 SSE。
- queued worker、lease、heartbeat、retry recovery。
- 分布式 provider health。
- dashboard 或 UI。
- 发布 tag 和 external consumer 示例。

## 当前 Workflow

同步路径如下：

```text
POST /workflows
  -> parse run_mode
  -> parse task_profile_preset + task_profile override
  -> validate profile can route against current candidates
  -> write input artifact
  -> workflow executor runs ingest
  -> agentstep builds RunRequest with workflow_id + task_profile
  -> llmkit adapter selects provider/model/account
  -> route/outcome audit written under LLMKIT_HOME
  -> runkit records goagent events and terminal summary
  -> workflow waits for approval

GET /workflows/{id}/llm-routes
  -> read route-events.jsonl + outcomes.jsonl
  -> filter by task_id == workflow id
  -> return sanitized route/outcome view

POST /workflows/{id}/approve
  -> resume workflow
  -> finalize artifact
```

这个 workflow 是清晰的，也和“host 拥有业务流程，llmkit 只做模型选择和审计”的边界一致。

## 模块边界判断

### 保持正确的边界

- `goagent` core 没有引入 `llmkit`、`workflowkit`、`runkit` 或 `artifactkit`。
- `llmkit/llmkit` 没有依赖 `goagent`。
- `llmkit/adapters/goagent` 是正确的适配层。
- `examples/host-api` 依赖多个模块是合理的，因为它是 host composition 示例。
- durable runtime 没有被提前抽成 `hostkit`，这符合当前设计判断。

### 需要注意的边界

- `task_profile_preset` 当前在 `examples/host-api` 中实现。这是合理的，因为 preset
  是 host policy，不一定属于 `llmkit` core。
- 如果多个 host 都需要同一套 preset，后续可以抽一个 host-side helper 包，但现在不应提前抽。
- `/workflows/{id}/llm-routes` 是 host API，不应放进 `llmkit`。`llmkit` 只提供
  `ReadRouteAudits` 这类读侧 helper 即可。

## 风险

1. **审计解释还不够完整**

   现在用户能看到 score breakdown 和模型选择，但看不到完整生效 profile。若一个 host
   传了 preset + override，只能通过 `task_type` 间接判断，复盘仍不够稳。

2. **host-api demo 仍是 static provider**

   这有利于测试稳定，但会让接入者看不到真实 config、API key env、OpenAI-compatible
   provider 的组合方式。

3. **profile override 的 bool 字段不能表达“继承 preset 的 true”**

   当前 `needs_reasoning: false` 和未传字段在 Go zero value 上不可区分。现有测试通过，
   但长期 contract 若要支持精确 override，应该改成 pointer bool 或局部 patch 结构。

4. **task outcome 语义偏 provider-level**

   当前 success 更接近“provider call succeeded”。如果要优化“任务成功概率”，最终还需要
   host 写入业务 outcome。

5. **profile validation 使用当前候选集**

   这能提前拒绝不可路由 profile，但也意味着 host-api demo 的 preset 合法性绑定了
   `defaultCandidates()`。真实 host 应把这视为启动/配置时的 policy 校验。

## 下一步优先级

### P0: 暂停新增复杂 runtime 能力

不要现在做 queued worker、SSE、dashboard 或 hostkit 抽取。当前更需要把现有 contract
打磨清楚。

### P1: 暴露有效任务画像

在 route audit response 中增加最终生效的 profile 摘要，至少包括：

- `task_type`
- `complexity`
- `failure_cost`
- `privacy`
- `needs_reasoning`
- `needs_tools`
- `needs_json`
- `needs_long_context`

这能直接回答“这次为什么选它”，比继续加新 endpoint 更有价值。

### P2: host-api 接入 llmkit config/provider

让 host-api 可从 `LLMKIT_HOME/config.yaml` 构建 candidates 和 OpenAI-compatible
providers，同时保留 static provider 作为测试默认。这样接入者才能看到真实项目如何
配置本地模型、云模型、多账号和 API key env。

### P3: OpenAPI contract

补 `examples/host-api/openapi.yaml` 或 docs contract，固定：

- create workflow request
- task profile preset 和 override
- workflow response
- llm route audit response
- error response

### P4: outcome 语义升级

给 host-api 增加业务 outcome 写入路径或示例，例如 approval 后把 human accepted 写入
`TaskOutcome` 的扩展字段。这个应先设计，不要直接改 audit schema。

## Readiness 判断

作为框架内部 canonical example：当前已可用。

作为真实 host 项目的参考实现：基本可接入，但还缺真实 provider config 示例和 OpenAPI。

作为生产服务：还不够。缺认证、多租户、限流、worker、observability、真实 secret 管理和
deployment contract。

## 建议

下一步做 P1：把最终生效的 task profile 写入 route audit 或 host route response。
这是最小但最高杠杆的改动，可以把“选择依据”从 score 解释推进到完整 policy 解释。
