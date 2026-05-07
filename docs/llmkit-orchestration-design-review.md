# llmkit orchestration design review

## 结论

当前 `llmkit` 编排链路已经形成闭环：host 可以声明任务画像，`llmkit`
基于候选模型、账号、价格、本地性、延迟、健康状态和历史 outcome 做确定性路由，
并把 route trace、provider outcome、模型统计暴露给 host 审计。

这已经满足最初的第一阶段目标：**让一个任务更可能选择合适的 LLM，而不是固定调用单一
provider**。

但当前边界仍应定义为 **host-side orchestration kit + canonical example**，
不是生产级 LLM gateway。下一阶段应优先稳定 contract、解释性和 host 接入文档，
不要马上扩展成 dashboard、队列系统或多租户网关。

## 当前设计闭环

```text
host task
  -> task_profile_preset + task_profile override
  -> effective TaskProfile
  -> llmkit candidates from config.yaml or host defaults
  -> provider clients from host wiring
  -> RoutePolicy filter + score
  -> selected provider/model/account
  -> route-events.jsonl
  -> outcomes.jsonl
  -> model-stats.json
  -> ModelStats feeds future routing
  -> host audit endpoint explains the decision
```

这个闭环的关键点是：`llmkit` 不从 prompt 文本里猜测业务风险，host 必须显式提供任务画像。
这让策略可测试、可审计，也避免 LLM 自己决定应该调用哪个 LLM。

## 已落地能力

### 独立模块边界

- `llmkit` 是独立 Go module。
- `llmkit/llmkit` 不依赖 `goagent`。
- `llmkit/adapters/goagent` 是唯一的 goagent adapter 边界。
- `goagent` core 仍只依赖 `ports.LLMClient`，不吸收多 provider 编排。
- `examples/host-api` 是 host composition 示例，负责组合 `workflowkit`、`runkit`、
  `artifactkit`、`goagent` 和 `llmkit`。

这个边界是正确的。LLM 账号、API key、价格、失败历史和项目策略都属于 host/project
层，不应进入 `goagent` small core。

### 配置与 provider

- `LLMKIT_HOME/config.yaml` 可以定义账号和模型。
- 配置保存 `api_key_env`，不保存明文 key。
- host-api 在配置文件不存在时回退到内置 demo candidates/providers。
- host-api 在配置文件存在但引用的 API key 环境变量缺失时启动失败。
- OpenAI-compatible provider 可以由 `llmkit/adapters/goagent` 从配置构建。

这使真实 host 项目可以按固定 workspace 路径管理自己的 LLM 配置和审计历史。

### 任务画像

host-api 当前支持：

- `task_profile_preset`
  - `simple_local`
  - `balanced`
  - `high_success`
  - `local_only`
- `task_profile` override
- 有效任务画像写入 route audit response
- 不可路由 profile 在创建 workflow 时返回 `invalid_task_profile`

这已经能表达最早讨论的策略：简单、低风险、无严格成功要求的任务优先本地；复杂、高失败成本、
需要推理的任务优先高级 LLM。

### 路由评分

当前 `RoutePolicy` 是确定性的 filter-then-score：

- hard filter
  - `local_only`
  - capability 不足
  - 不支持 tools/JSON/long context
  - provider quota exhausted
  - provider cooldown
  - concurrency full
- score
  - capability
  - price
  - local preference
  - latency
  - recent reliability
  - provider health

这种设计比“让 LLM 自己选择 provider”更可控，也更适合审计。

### 审计与历史反哺

`LLMKIT_HOME` 下的审计文件是当前事实源：

- `route-events.jsonl`
- `outcomes.jsonl`
- `model-stats.json`

host-api 启动时会验证并初始化 `model-stats.json`。运行中，host-api 通过
`ModelStatsProvider` 在每次路由前刷新统计，`GET /llmkit/models` 也会刷新后返回。
adapter 会在当前 `TaskProfile` 确定后应用历史统计，因此历史是 task-type aware 的。

`GET /workflows/{id}/llm-routes` 可以查看一次 workflow 的 route trace、score breakdown、
effective profile 和 outcome。

`GET /llmkit/models` 可以查看当前 candidates、health snapshot 和历史 stats 摘要，包括：

- route attempts
- outcome count
- pending outcomes
- successes / failures
- success rate / failure rate
- average latency
- average token usage
- average estimated cost

这已经把“历史记录”从单纯审计推进到下一次路由决策依据。

## 当前剩余边界

这几项的后续设计已经收敛到
`docs/plans/2026-05-07-llmkit-host-contract-followups-design.md`。总体原则是：
先稳定 host-facing contract，不把生产 gateway 能力直接塞进 `goagent` core 或
`llmkit` routing core。

### 1. 运行中 stats refresh 已补齐到 host/adapter 边界

goagent adapter 支持 `ModelStatsProvider`，host-api 在每次路由前刷新
`model-stats.json`，`GET /llmkit/models` 也会刷新后返回。因此长进程中新 outcome
可以进入后续路由，不再需要重启。

仍然不是分布式实时流式统计；多副本场景应由 host 提供共享 stats provider。

### 2. provider health 仍是单进程内存态

`MemoryHealthStore` 能处理 in-flight、cooldown、quota 和 failure streak，但它不是分布式状态。

影响：

- 单进程 host-api 示例足够。
- 多副本部署时，不同实例不会共享 provider health。

判断：

- 不应现在把它直接做成数据库强依赖。
- 更合适的下一步是把 `HealthStore` contract 文档化，并提供 host 可替换实现建议。
  正式 contract 见 `docs/llmkit-healthstore-contract.md`。

### 3. 成本预算已有 per-task 硬约束

`TaskProfile.MaxEstimatedCents` 可以作为任务级硬约束。模型配置或历史
`AvgEstimatedCents` 提供已知成本后，超过预算的候选会在评分前被过滤。

仍然没有 project/account 级预算扣减，这属于 host 成本治理。
正式边界见 `docs/llmkit-budget-governance.md`。

### 4. fallback 已有显式 adapter policy

adapter 支持 `FallbackPolicy.MaxAttempts`。provider 失败后会记录失败 outcome，
移除该候选，并按 policy 限制尝试后续候选。每次 fallback attempt 都有 route trace
和 attempt 编号。

仍然没有按错误类型分别配置 timeout/rate-limit/schema-failure 的策略；这应在下一阶段
建立错误分类后再做。
设计见 `docs/plans/2026-05-07-llmkit-host-contract-followups-design.md`。

### 5. business outcome contract 已建立

`TaskOutcome` 已区分 provider success 与业务成功信号：

- `business_outcome`
- `success_signal`
- `failure_reason`

host-api approval 会把 selected route 标记为 `business_outcome=success`、
`success_signal=human_accepted`。这让“provider 调用成功”和“业务上被接受”可以分开审计。

### 6. 候选未选原因已补齐

route audit 和 host route response 都暴露完整 `candidates`，每个候选包含
`available`、`score`、`score_breakdown` 和 `reason`。因此现在可以解释为什么没有选本地、
便宜或高级模型。

## 当前设计边界判断

### 不应该进入 goagent core 的内容

- 多账号配置
- `LLMKIT_HOME`
- provider price/cost
- route audit
- model stats
- health store
- host task profile preset

这些都应留在 `llmkit` 或 host composition 层。

### 可以留在 host-api 示例中的内容

- HTTP endpoint shape
- workflow lifecycle
- approval workflow
- artifact refs
- task profile preset
- demo static provider fallback
- durable SQLite runtime

这些是 host-facing product choices，不是 `llmkit` core。

### 应该在 llmkit 中稳定的内容

- `TaskProfile`
- `ModelCapability`
- `Candidate`
- `RoutePolicy`
- `RouteTrace`
- `TaskOutcome`
- `ModelStats`
- `HealthStore`
- config loader
- goagent adapter

这些是可复用能力，应保持小而稳定。

## 下一步建议

当前第一阶段 LLM 编排闭环已经完整。下一步不应继续把 host-api 做成生产网关，而应进入
release/readiness 检查：

1. 检查各 module 的 README、go.mod replace、独立测试和发布边界。
2. 决定是否为 `llmkit` 打 tag 或先保持 workspace 内部模块。
3. 如需生产化，再单独设计认证、多租户、分布式 health、项目预算和 worker 队列。
