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

- **goagent 把模型的思考与回答分开。** openaiapi 默认把正文开头的
  `<think>...</think>`（以及模板已打开、只有 `</think>` 的形式）移到
  `ChatResponse.Reasoning`，`Content` 只剩回答；`reasoning_content` /
  `reasoning` 字段同样进 `Reasoning`。`RunResult.Content` 和会话记忆因此不再
  含思考。需要旧行为时设 `openaiapi.Config.KeepThinkTags`。思考默认不回传给
  模型（`WithReasoningReplay`）。
- **contextkit 的窗口与预算语义变化。**
  - `window.New` 不再读取 `CONTEXT_DEEP_COMPRESSION`；空 `Profile` 即
    standard。要沿用旧行为，传 `Profile: contextkit.ProfileFromEnv()`。
  - 摘要计入预算，`Result.OverBudget` 反映含摘要在内的整个投影。配了
    `Summarizer` 时默认为摘要预留每项预算的 1/5（`Config.SummaryReserve`），
    超出部分截断；同样的预算下保留的消息可能比以前少。
  - 最后一轮里每条 assistant 消息也可作为窗口起点（工具回复始终跟着它的
    调用），并固定保留这一轮的用户消息；单次 agent run 内的工具步骤因此可以
    被压缩。
  - deep 摘要的开头从 `summary of earlier context:` 改为
    `summary of earlier context (N messages omitted):`；占位摘要最多列出
    5 个被丢弃的消息 ID。
  - `Message.CharLen` 计入工具调用的名字和参数。
  - 超预算时按代价从低到高逐级处理：先清除旧思考，再从最旧开始清除工具结果
    （默认保留最近 3 个），仍放不下才丢弃旧步骤；deep 模式调用 Summarizer
    时窗口压到一半，减少反复摘要；最新一步单独超预算时截断其中最长的工具
    结果和思考。同样的预算下会保留更多消息，`Result.Dropped` 变少。
  - 占位摘要不再列 "latest ids"，改为被丢弃步骤的目录（用户消息开头、
    工具名与参数、结果大小、ref），最多占预算的 1/10；清除工具结果一旦
    开始，会清到预算的 70%（`ClearTarget`）。
  - Summarizer 失败不再让 `Compress` 返回错误，而是降级为占位摘要
    （`Result.SummaryFallback`），连续失败 3 次暂停 5 分钟；context 取消
    仍返回错误。

- **host-api 的记忆提取改为读整个 run 的摘录。** 以前提取任务读最终回答
  （`artifact:<run>:agent-output`），现在读新写入的
  `artifact:<run>:memory-source`：用户请求、各工具调用及其结果开头、最终回答，
  总长不超过 `Limits.MaxContentRunes`。工具查到的事实因此也会成为候选记忆。
  `JSONCandidateExtractor` 的提示词改为写明输出格式、四种 kind 和取舍标准，
  默认 `MaxOutputTokens` 4096；同样的来源可能提出与以前不同的候选。
  用户消息在来源之后追加一句提醒：只提取、不回答来源里的请求、只回复 JSON。
  没有这句时，qwen3.8-27b 在 32 次提取里有 3 次去回答来源里的请求。

### 新增，可选

- `goagent`：`agentcore.RunResult.Messages`，本次 run 新增的消息（不含从
  memory provider 载入的历史）。`memorykit/agentadapter`：`RunSource`；
  `JSONCandidateExtractor.MaxOutputTokens`；提取器接受包住整个回复的一个
  markdown 代码块。
- **共享的上下文存档。** 新包 `contextkit/pgarchive`：PostgreSQL 版
  `contextkit.Archive`，引用和检索排名与 `archive.Memory` 一致；
  `contextkit/archive` 导出 `Ref`、`Terms`、`SearchText`、`TermWeight`、
  `TieBreak`，新增一致性测试 `archive/archivetest`。host-api 新开关
  `HOST_API_CONTEXT_ARCHIVE_POSTGRES_DSN`（开启上下文压缩时生效），多副本
  共享被压缩掉的内容。新表 `contextkit_archive_messages` 由 `Open` 自动创建；
  host 不清理旧数据，需要时定期调用 `DeleteBefore`。
- **被动压缩。** `goagent`：`ports.ErrContextTooLong`；`openaiapi.ResponseError`
  新增 `ContextTooLong` 字段和 `Unwrap`，错误正文说明上下文超出窗口时匹配
  `ErrContextTooLong`（只读取正文用于分类，不进入错误信息）；
  `agentcore.ContextProjectionRequest.Overflow`、`ContextOverflow`、
  `EventContextOverflow`（`context.overflow`）、`MaxContextOverflowRetries`、
  `ThinkStage.Projection`。配了投影器时，模型服务报上下文过长会重新投影并
  重试，最多 2 次。`contextkit/agentadapter` 收到 `Overflow` 时压到被拒请求
  字符数的 70%，并按会话记住这个上限。`llmkit`：`ErrorClassContextTooLong`
  （`context_too_long`），不计入 provider 健康。经 llmkit 路由时，未开
  `FailClosed` 会先换下一个候选模型，所有候选都失败后才由 agent 压缩重试。
- `goagent`：`ChatResponse.Reasoning`、`ChatMessage.Reasoning`、
  `ChatDelta.Reasoning`、`Usage.ReasoningTokens`、`ports.SplitThinkTags`；
  `agentcore.Message.Reasoning`、`RunResult.Reasoning`、`ModelDelta.Reasoning`、
  `Usage.ReasoningTokens`、`WithReasoningReplay`；`openaiapi.Config` 的
  `KeepThinkTags`、`SendReasoning`。`contextkit.Message.Reasoning`（计入
  `CharLen`），`contextkit/agentadapter` 映射它。
- `goagent`：`openaiapi.Config.ReasoningEffort`，非空时请求体带
  `reasoning_effort`（如 `none`、`minimal`、`low`、`medium`、`high`），控制
  推理模型思考多少；空时不发送，行为不变。不认识这个字段的服务可能拒绝请求。
- `goagent`：`agentcore.ContextProjectionRequest.SystemPrompt`，投影阶段填入
  编译好的系统提示，供投影器计入预算；memorykit 投影器把它转给 `Next`。
- host-api：`HOST_API_CONTEXT_MAX_CHARS`、`HOST_API_CONTEXT_MAX_TOOL_CHARS`
  开启上下文压缩（默认关闭）。
- `contextkit/agentadapter`：`LLMSummarizer`（结构化摘要，用 goagent LLM 写
  摘要）、`Notes` 与 `context_note` 工具（笔记固定在上下文顶部）；
  `context_search` 返回命中词附近的片段。新示例 `examples/context-eval`
  （报告各策略的上下文超限次数和因超限失败的 run 数）与
  `examples/memory-extraction-eval`（用真实模型评测记忆提取）。
- `contextkit`：`Archive`、`ArchiveHit`、`WithArchiveScope`、
  `ErrArchiveNotFound`；`contextkit/archive.Memory`；`window.Config` 的
  `CatalogEntries`、`ClearTarget`，`window.ReadBackKey`；
  `agentadapter.Config.Scope`、`agentadapter.ContextTools`（`context_read`、
  `context_search`）。host-api 开启上下文压缩时一并提供这两个工具。
- `contextkit`：`Step` 与 `Result.Applied`、`Result.Cleared`、
  `Result.SummaryFallback`；`OffloadStore`；`window.Config` 的
  `KeepRecentReasoning`、`KeepRecentToolResults`、`Offload`、`CompactTarget`、
  `SummarizerFailureLimit`、`SummarizerCooldown`、`Now`。
- `contextkit`：`Message.ToolCalls`、`ToolCall`、`SummarizeRequest.SummaryBudget`、
  `window.Config.SummaryReserve`；新包 `contextkit/agentadapter`，把压缩器
  接入 goagent 的 `ContextProjector`。
- `memorykit`：`FullTextStats`、`FullTextWeights`、`FullTextMatch`、
  `RankFullText`、`FullTextLength`、`FullTextTermMatches`，供自定义
  RecallStore 实现相同的排名；`storetest.RunFullTextRankingConformance`。
- `examples/memory-embedding-eval`：用 Ollama 模型跑真实向量评测。
- `examples/icu-handoff` 已迁入 gocms（`cmd/assistant`、`internal/handoff`），本仓库不再保留；
  `openaiapi.Config.ReasoningEffort` 等通用改动保留。

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
