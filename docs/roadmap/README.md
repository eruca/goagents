# GoAgents Roadmap

- 更新日期：2026-11-19
- 对应代码：`main` @ `b0dabe1`（v0.8.0）
- 状态：多副本部署和轨迹评测已完成；生产化 host runtime 因没有调用方而搁置。

本文以当前 `main` 为准。2026-07 的 memorykit/v0.1.0 产品基线、roadmap 和
ADR-0001 是在分 kit 发版、旧 CJK 检索方案下写的，已经不适用，不再维护；保留它们的
备份分支已在 v0.7.0 时删除（最后提交 `59e7fb2`）。

## 长期方向

沿用旧产品基线中仍然成立的部分：

- GoAgents 是一组可嵌入的 Go 库。业务宿主保留业务事实源和授权边界，按需复用
  记忆、工作流、工具和评测能力。
- 公共 API 由真实宿主遇到的问题推动，不预先建通用 Host 平台。
- 优先把已有 kit 做扎实，不继续拆更多小模块。
- 只有出现明确的跨 Agent 业务流程，才在 host/workflow 层考虑 handoff，不扩张
  `goagent` 核心。

## 当前状态

- 单 module `github.com/eruca/goagents`，按 `vX.Y.Z` 整体发版；kit 之间的依赖
  由 `internal/boundary` 在测试中强制检查（见 [`docs/modules.md`](../modules.md)）。
- PR #2–#4 完成三轮审查修复（P0–P3），覆盖租约、审批绑定、记忆写入原子性、
  流式错误统计和 host-api 安全加固；随后 contextkit 支持按 token 计预算。
- memorykit 中英文召回评测集 v2（#8）已合入：exact+FTS 召回 19/22，
  禁止召回 1/30（词面匹配的已知限制，见 #9）。用本机 Ollama bge-m3 跑了真实
  向量评测（[`docs/evals`](../evals/memorykit-v2-ollama-bge-m3.md)）：三路融合、
  阈值 0.5 时召回 22/22，同义改写从 1/4 到 4/4；#9 的草稿主题误召回向量也
  挡不住，需要 reranker。全文通道改为 BM25（IDF 加权 + 长度归一），排名不再
  依赖记忆 ID，三路融合在低阈值下的误召回减少。
- v0.7.0 发版，升级说明见 [`docs/upgrading.md`](../upgrading.md)。其中从旧分支
  移植并合入：llmkit `CascadeExecutor`（#5，按验收测试逐级升级模型，门槛可配置）、
  `examples/memory-ingest`（#6）、memory worker 可观测性与 `GET /workers/memory`（#10）。
- 多副本部署已完成：五个后端都有 PostgreSQL 实现（`workflowkit/pgstore`
  #13、`llmkit/pghealth` #14、`artifactkit/pgstore` #17、`runkit/pgstore`
  run 存储 #18、`llmkit/pgaudit` #20），加上静态审批 keyring（#12）、
  embedding 互斥执行（#19）和带随机后缀的 input_ref（#16）。host-api 打开
  六个开关后可以多进程部署，有两进程黑盒测试（含执行中 SIGKILL）覆盖。
- 轨迹评测：evalkit 有固定的轨迹词汇和现成的评分器（工具必用/禁用/顺序、无失败
  步骤、预算、结果状态）及 `RecordedHarness`（#24）；`evalkit/runtrace` 从
  workflowkit、runkit 和 llmkit 审计的存储记录生成无正文轨迹（#25）；host-api
  提供 `GET /workflows/{id}/trace`，`examples/trajectory-eval` 可对一批生产
  记录离线打分并作为回归门禁（#26）。
- `examples/host-api` 仍是示例而不是生产网关：每个进程的 worker 一次处理一个
  任务，没有限流，也没有独立的可执行程序和镜像。

| Kit | 成熟度 | 已知缺口 |
|---|---|---|
| goagent | 高 | — |
| memorykit | 高 | 词面和向量都分不清"草稿"与"已批准"这类语义相近、结论不同的记忆（#9），需要宿主侧 reranker |
| workflowkit (+agentstep) | 高 | — |
| llmkit | 高 | 没有项目/账户级预算扣减 |
| runkit | 高 | — |
| contextkit | 中 | 已通过 `contextkit/agentadapter` 接入 goagent，单次 run 内可压缩；还没有在 host-api 或真实宿主里启用，也没有用真实 LLM 摘要器验证过摘要质量 |
| ocrs / skillkit / mcpkit | 中 | — |
| evalkit (+runtrace) | 中 | 不导出 OpenTelemetry；没有 model-as-judge 评分器 |
| artifactkit / hostkit | 基础 | artifactkit 没有对象存储（S3）实现，等真实宿主遇到 artifact 体积问题再做 |

## 下一阶段

1. ~~**多副本部署**~~：已完成（见"当前状态"）。剩余的小问题记在 host-api
   README 的 "Shared backends and multiple replicas" 一节：创建时的 worker
   唤醒只在本进程、`/workers/*` 只报告本进程、审计统计每次全量重算、审计表
   只增不减。
2. **生产化 host runtime**（搁置）：原 Week 18 计划中的 wire/client/server、
   UDS/mTLS、Control/Tool Bridge、Host 可执行程序和 OCI 镜像。2026-08 已做出
   `hostruntime` 模块的大部分（协议、两种传输、Control Bridge、RuntimeService；
   binary/OCI 做了一部分），后因 AI Todo 决定首版只用进程内 `goagent + llmkit`
   而中止。代码在本地分支 `backup/week18-wip-2d15`（另有
   `backup/week18-inprocess-release`），基于旧的多 module 布局。目前唯一的本地
   宿主 todo 仍是进程内用法，按"不预建通用 Host 平台"的原则暂不恢复；出现需要
   独立 Host 进程的宿主时，再按它的调用方式重新定范围。
3. ~~**补强基础 kit：轨迹评测**~~：已完成（见"当前状态"）。按维护者决定不接
   OpenTelemetry、不做 artifactkit 对象存储。

另有几件小事：

- todo 仍依赖分 kit 时代的 v0.1.x 模块；把它迁到单 module 的最新版本，是获得
  真实宿主反馈、决定后续公共 API 的最直接途径（需要改 todo 仓库，另行授权）。
- 项目/账户级成本预算扣减（边界见 [`docs/llmkit-budget-governance.md`](../llmkit-budget-governance.md)）。
- 更新 [`docs/llmkit-orchestration-design-review.md`](../llmkit-orchestration-design-review.md)
  中"是否为 llmkit 单独打 tag"等单 module 之前的结论。

## 维护方式

方向或顺序变化时更新本文，并改"更新日期"和"对应代码"。已完成的条目移到
"当前状态"，不单独保留历史；历史以 git log 和 `docs/devlog` 为准。

完成一项工作不等于发版。发版条件和版本号规则见根目录 README 的
[Release](../../README.md#release) 一节。
