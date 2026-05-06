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

host-api 启动时会刷新 `model-stats.json`，并把 `ModelStats` 注入 goagent adapter。
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

## 当前仍然缺席的核心能力

### 1. 运行中 stats 不是实时增量刷新

当前 `model-stats.json` 在 host-api 启动时刷新一次，之后 adapter 会继续写新的 outcome，
但内存中的 `ModelStats` 不会自动增量更新。

影响：

- 进程内连续任务主要依靠 `MemoryHealthStore` 反映近期失败和 cooldown。
- 跨进程、重启后，历史 outcome 才会稳定进入 `ModelStats`。

判断：

- 这对第一阶段是可接受的。
- 如果要用于长时间运行服务，下一步需要一个 `StatsProvider` 或定期 refresh 策略。

### 2. provider health 仍是单进程内存态

`MemoryHealthStore` 能处理 in-flight、cooldown、quota 和 failure streak，但它不是分布式状态。

影响：

- 单进程 host-api 示例足够。
- 多副本部署时，不同实例不会共享 provider health。

判断：

- 不应现在把它直接做成数据库强依赖。
- 更合适的下一步是把 `HealthStore` contract 文档化，并提供 host 可替换实现建议。

### 3. 成本预算还是评分因素，不是硬约束

当前成本主要体现在 `price_class` 和 `estimated_cents` 审计。

缺口：

- 没有 per-task max cost。
- 没有 per-project budget。
- 没有按账号、模型、时间窗口的预算扣减。

判断：

- 对“选择更合适的 LLM”已经够用。
- 对生产成本治理不够。

### 4. fallback 是 adapter 行为，还不是可配置 policy

adapter 当前在 provider 失败后移除该 candidate，再让 policy 选择下一个候选。

缺口：

- 没有按错误类型配置 fallback 次数。
- 没有区分 timeout、rate limit、invalid JSON、policy blocked 等 fallback 语义。
- 没有 host-facing fallback chain summary。

判断：

- 这已经比单次调用可靠。
- 下一步应先暴露 fallback trace，再考虑配置化。

### 5. outcome 语义仍偏 provider-level

当前 `TaskOutcome.Success` 主要表示 provider call 是否成功。

但用户最初关心的“成功概率更高”最终应是业务成功，例如：

- schema validation passed
- human accepted
- test passed
- tool completed
- final answer accepted

判断：

- provider success 是必要基础，但不是最终成功。
- 后续需要 host 写入 business outcome，不能让 `llmkit` 自己猜。

### 6. 候选未选原因还不够完整

route audit 当前有 selected route 的 score breakdown 和 candidate aliases。
`RouteDecision` 内部有候选评分，但 host route response 没有完整暴露所有候选的 score/reason。

影响：

- 能解释“为什么选了它”。
- 还不能完整解释“为什么没选另一个”。

判断：

- 这是解释性上的下一块高价值工作。

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

## 是否可以继续实现

可以继续，但应进入 **contract stabilization** 阶段，而不是继续扩 runtime 大功能。

建议优先级：

### P0: 固化文档与示例

- 更新 `llmkit/README.md`，补 host-api 当前配置加载、stats 反哺和 audit endpoint 示例。
- 更新 `examples/host-api/README.md`，明确 `LLMKIT_HOME/config.yaml`、默认 fallback、
  fail-closed API key 规则和 `/llmkit/models.stats`。
- 给 host-api 增加一份最小 OpenAPI 或 endpoint contract 文档。

价值：

- 让外部 host 项目知道怎么接。
- 避免能力已经实现但使用路径不清楚。

### P1: 暴露完整候选评分

在 route audit response 中增加 candidates 的完整评分和过滤原因。

价值：

- 直接回答“为什么没选本地模型/为什么没选高级模型”。
- 对调参和生产审计很关键。

### P2: 运行中 stats refresh 策略

引入轻量 `StatsProvider` 或 host-api 周期 refresh。

价值：

- 长进程服务不必等重启才能让新 outcome 反哺路由。

### P3: business outcome contract

设计 host 如何写入业务成功信号，而不是只记录 provider call outcome。

价值：

- 把“调用成功”升级为“任务成功概率”。

### P4: 成本/预算硬约束

在 `TaskProfile` 或 host policy 中增加预算约束。

价值：

- 让“价格因素”从软评分变成可治理 contract。

## 下一步建议

下一步不要再直接加 provider 或 workflow 能力。最稳的是先做 P0：

1. 更新 `llmkit/README.md`
2. 更新 `examples/host-api/README.md`
3. 增加 host-api endpoint contract 文档

完成后再做 P1：完整候选评分解释。

这样顺序最清楚：先让已经实现的能力可理解、可接入，再增强解释性和生产能力。
