# 升级说明

按版本从新到旧排列。每节只列升级时需要处理的变化；完整改动见对应 tag 和
`git log`。

## Unreleased

### 行为变化

- **memorykit 全文召回改用 BM25 排名。** 以前按命中的查询词个数排序，
  命中数不到最佳一半的丢弃；现在每个词按它在作用域内的 IDF 加权，按权重和
  决定保留（不到最佳一半的丢弃），再按长度归一后的分数排序。常见词带来的
  召回会减少，只命中一个少见词的记忆可能新被召回。公共 API 没有变化，
  数据库 schema 不变，不需要迁移。依赖具体全文召回结果的宿主测试可能需要
  更新预期。

### 新增，可选

- `memorykit`：`FullTextStats`、`FullTextWeights`、`FullTextMatch`、
  `RankFullText`、`FullTextLength`、`FullTextTermMatches`，供自定义
  RecallStore 实现相同的排名；`storetest.RunFullTextRankingConformance`。
- `examples/memory-embedding-eval`：用 Ollama 模型跑真实向量评测。

## v0.7.x → v0.8.0

### 新增，可选

- `evalkit`：轨迹步骤类型与状态常量（`StepToolCall`、`StepModelCall` 等）、
  轨迹评分器 `RequireTools`、`ForbidTools`、`ToolOrder`、`NoFailedSteps`、
  `Budget{}.Grader()`、`OutcomeStatus`，以及对已记录结果打分的
  `RecordedHarness`。
- `evalkit/runtrace`：`BuildWorkflow` / `BuildRun` 从 workflowkit、runkit 和
  llmkit 审计的存储记录生成无正文轨迹。
- host-api：`GET /workflows/{id}/trace`；新示例 `examples/trajectory-eval`。

没有需要处理的不兼容变化。

## v0.6.x → v0.7.0

### 需要处理

- **host-api：`input_ref` 不再是固定值。** 以前是 `artifact:{id}:input`，现在是
  `artifact:{id}:input:` 后跟每次请求随机生成的后缀。客户端应把它当作不透明字符串，
  从创建响应或 `GET /workflows/{id}` 中读取，不要自己拼接。两个 Host 同时创建同一
  id 时，失败的一方返回 `409`，工作流保留成功方的输入。用旧格式 `input_ref`
  的工作流（v0.6.x 创建、升级后才执行 agent 步骤或恢复审批的）在启用 memory 时
  会被拒绝，升级前请先让在途工作流跑完。

### 新增，可选

- 新的 PostgreSQL 实现，都提供 `Open(ctx, dsn)` 和 `OpenDB(ctx, *sql.DB)`（后者
  共用调用方的连接池，`Close` 不会关闭它）：
  - `workflowkit/pgstore`：工作流、排队租约与恢复；
  - `artifactkit/pgstore`：artifact 存在 `bytea` 列中；
  - `runkit/pgstore`：除 checkpoint 外，现在完整实现 `runkit.Store`（run 与事件），
    已有数据库直接升级；
  - `llmkit/pghealth`：跨进程共享 provider 健康状态和并发名额；
  - `llmkit/pgaudit`：路由与结果审计及模型统计，语义与 JSONL 文件版一致。
- `runkit/approvalcrypto`：静态 keyring（`ParseKeyring`、`NewStaticKeyProvider`），
  可以不依赖 Keychain。
- `llmkit/adapters/goagent`：`CascadeExecutor` 按验收测试逐级升级模型，门槛可配置。
- `llmkit`：导出 `BuildModelStatsFromAudit`、`SanitizeRouteTrace`、`SanitizeTaskOutcome`；
  `llmkit/healthtest` 提供 `HealthStore` 一致性测试。
- `memorykit`：`EmbeddingWorkerConfig.RunGate`，`memorykit/pgstore.Store` 实现它，
  多个进程中同一时刻只有一个在跑 embedding；worker 可观测性，以及 host-api 的
  `GET /workers/memory`。
- host-api：六个开关全部打开时可以多进程部署，见
  [`examples/host-api/README.md`](../examples/host-api/README.md) 的
  "Shared backends and multiple replicas"：

  | 环境变量 | 作用 |
  |---|---|
  | `HOST_API_WORKFLOW_POSTGRES_DSN` | 工作流与队列 |
  | `HOST_API_AGENT_RUN_POSTGRES_DSN` | agent run、事件与审批 checkpoint |
  | `HOST_API_ARTIFACT_POSTGRES_DSN` | artifact |
  | `HOST_API_LLMKIT_AUDIT_POSTGRES_DSN` | LLM 审计与模型统计 |
  | `HOST_API_LLMKIT_HEALTH_POSTGRES_DSN` | provider 健康状态与并发名额 |
  | `HOST_API_AGENT_APPROVAL_KEYRING` | 审批加密 keyring，各副本必须相同 |

  不设置时行为与 v0.6.0 相同。多副本之间需要时钟同步。同一 DSN 的后端共用一个
  连接池。
- 新示例 `examples/memory-ingest`。

### 数据库

- `memorykit/pgstore` schema 不变（仍为 3）。
- 新增的 PostgreSQL store 各自建表并记录 schema 版本，首次打开时自动迁移；遇到
  比自身新的版本会拒绝打开。

## v0.5.x → v0.6.0

### 需要处理

- **goagent**：`ports.MemoryProvider.Save` 改为 `Append`，只传本次 run 新增的消息。
  run checkpoint 升到版本 2，版本 1 的 checkpoint 仍可恢复。
- **workflowkit/agentstep**：`ResultMapper` 签名改为
  `func(run, result, err) (StepResult, error)`；默认结果改为继续下一步，不再结束
  工作流。
- **mcpkit**：`Registry` 需要实现 `Get`；server 的工具名先校验再注册，不能覆盖已
  注册的工具。`officialsdk` 的 stdio server 默认只继承 `DefaultInheritEnv` 中的
  环境变量，需要更多变量时用 `InheritEnv` / `Env`。
- **host-api**：
  - `GET /agent-runs/{id}` 的 summary 和 events 字段改为 snake_case；
  - 新增 `409` / `413` 错误码；`500` 响应不再带内部错误信息；
  - 启用 memory 时，读取已存储工作流的接口需要授权，`GET /workflows` 必须带
    `project_id`。

### 数据库

- `memorykit/pgstore` schema 从 2 升到 3（`search_words`、抽取任务的
  `next_attempt_at`）。升级后的数据库不能再被 v0.5.x 及更早版本打开，升级前请备份。
