# GoAgents Roadmap

- 更新日期：2026-11-18
- 对应代码：`main` @ `c9c3dcd`（v0.6.0 之后）
- 状态：草案。"下一阶段"一节的方向和顺序要等维护者确认后才算定稿。

本文以当前 `main` 为准。2026-07 的 memorykit/v0.1.0 产品基线、roadmap 和
ADR-0001 是在分 kit 发版、旧 CJK 检索方案下写的，已经不适用，原文保留在分支
`backup/memorykit-v0.1.0-week01` 中备查，不再维护。

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
  禁止召回 1/30（词面匹配的已知限制，见 #9）。
- v0.6.0 之后从旧分支移植并合入：llmkit `CascadeExecutor`（#5，按验收测试逐级
  升级模型，门槛可配置）、`examples/memory-ingest`（#6）、memory worker
  可观测性与 `GET /workers/memory`（#10）。
- `examples/host-api` 是单机 MVP：有进程级黑盒验收和 3×50 工作流的稳定性测试，
  worker 一次处理一个任务，不是生产网关。

| Kit | 成熟度 | 已知缺口 |
|---|---|---|
| goagent | 高 | — |
| memorykit | 高 | 全文检索是词面匹配，分不清词面相同、语义不同的记忆（#9）；还没用真实 embedding 跑过评测集 |
| workflowkit (+agentstep) | 高 | 只有 SQLite store，没有 PostgreSQL store |
| llmkit | 高 | HealthStore 只有进程内存实现；没有项目/账户级预算扣减 |
| runkit | 高 | 审批 checkpoint 密钥来源只有 macOS Keychain |
| contextkit / ocrs / skillkit / mcpkit | 中 | — |
| artifactkit / evalkit / hostkit | 基础 | artifactkit 只有 FileStore |

## 下一阶段候选（待决定）

下面三个方向互相独立，需要选定先后顺序：

1. **支持多副本部署**：workflowkit PostgreSQL store；可共享的 llmkit HealthStore
   实现（契约见 [`docs/llmkit-healthstore-contract.md`](../llmkit-healthstore-contract.md)）；
   非 macOS 的 checkpoint 密钥来源。这是第 2 项的前提。
2. **生产化 host runtime**：原 Week 18 计划中的 wire/client/server、UDS/mTLS、
   Control/Tool Bridge、Host 可执行程序和 OCI 镜像。目前完全没有开始。
3. **补强基础 kit**：artifactkit 对象存储、evalkit 和 production tracing /
   trajectory evaluation（旧基线中列为 memorykit 之后的下一项横向能力）。

另有几件小事：

- 接入真实 embedding provider 后用评测集验证向量检索能不能挡住已知误召回（#9 的后续，需要另外授权调用费用）。
- 项目/账户级成本预算扣减（边界见 [`docs/llmkit-budget-governance.md`](../llmkit-budget-governance.md)）。
- 为 v0.6.0 的 breaking changes 写升级说明：`MemoryProvider.Append`、
  `GET /agent-runs/{id}` 改为 snake_case、mcpkit 工具名先校验再注册。
- 更新 [`docs/llmkit-orchestration-design-review.md`](../llmkit-orchestration-design-review.md)
  中"是否为 llmkit 单独打 tag"等单 module 之前的结论。

## 维护方式

方向或顺序变化时更新本文，并改"更新日期"和"对应代码"。已完成的条目移到
"当前状态"，不单独保留历史；历史以 git log 和 `docs/devlog` 为准。
