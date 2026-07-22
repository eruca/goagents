# Task 12 实施报告

## 范围

- 将可信项目 Scope 持久化到 workflow metadata，并在 sync、queued、tool approval checkpoint 与 resume 中恢复。
- 在 Host Agent 组合中接入 memory guard、自动召回 projector 和 request-scoped memory tools。
- 增加 Host 管理的 embedding/extraction 两个 ticker loop，并接入 drain/force-stop 生命周期。
- 成功 Agent 输出完成持久化后，以确定性 UUIDv5 enqueue extraction job；enqueue 失败仅记录无内容降级事件。
- 增加 artifact-only、有界且超限拒绝的 extraction SourceReader。
- 使用 `memorystore` 完成同项目/跨项目、candidate review、correction、forget 和恶意记忆隔离 E2E。

## RED 证据

首批 focused 测试在生产实现前失败，缺失点包括：

- `memoryRuntimeConfig.ExtractionJobs`、`ExtractorID`、`newMemoryRuntime`、可信 Scope resolver 和 worker lifecycle 方法不存在；
- workflow 尚未接受/授权 `project_id` 与 `memory_write_intent`，也未恢复可信 RunRequest；
- Agent 尚未插入 Memory View 或注册 memory tools；
- Host service 尚未启动/等待 memory workers；
- artifact SourceReader 和稳定 extraction enqueue 尚不存在。

后续回归 RED：

- memory read/write 授权失败时，测试观察到 input Artifact 已被写入；授权被前移到 execution 登记和任何持久化之前后转绿。
- worker dependency 返回自身 `context.DeadlineExceeded` 时，ticker loop 错误退出；改为只在 caller-owned execution context 实际取消时退出后转绿。
- E2E 原先对含函数 ToolSchema 的整份 ChatRequest 做 JSON marshal，形成空断言；改为只检查 Messages，并用不同 query marker 与正文重新证明 project/forget 隔离。
- runtime 接受无法容纳 `extractor:` actor 或 36-rune Host RunID 的 metadata 上限；增加 raw/prefixed extractor 与 UUID 边界校验后转绿。
- 同一确定性 extraction job 在两次 enqueue 时因 `Now()` 变化产生不同 payload；改为校验并读取 succeeded Agent Run 的持久 `UpdatedAt` 后转绿。
- sync 与 resume 两个完成路径使用 workflow-scoped output ref，同 workflow requeue 会覆盖旧 pending job 的 source；改为 Agent RunID-scoped ref 后，旧 source 内容保持不变。

## 实现决策

- `Server.memory` 升级为独立 `*memoryRuntime`；runtime 嵌入 caller config 的顶层副本，但不取得 memory Store 所有权。
- 用户确认采用显式 `ExtractionJobs memorykit.ExtractionJobStore` 和必填 `ExtractorID`；预构建 ExtractionWorker 不改成 factory。调用方必须用同一 durable queue 构造 worker，这是无法通过私有 worker 反射验证的显式 invariant。
- SourceReader 只接受 `Kind=artifact` 且 `artifact:` ref；正文超过 `Limits.MaxContentRunes` 直接拒绝，不截断。
- extraction job 的 `SourceAgentID` 使用成功 Agent RunID；ID 对 Scope、RunID 与 output ref 做域隔离 UUIDv5。
- enqueue 会重新读取 succeeded Agent Run，校验其 RunID、状态和 ContentRef 后使用持久 `UpdatedAt`；因此同一持久状态的重试完全幂等且不依赖 Host `Now()`。
- Agent 输出 ref 固定为 `artifact:<AgentRunID>:agent-output`；runtime 同时验证 Source.Ref 与 SourceAgentID 的精确绑定。
- Observer 只落 event type、policy version、item count、memory IDs 与 degraded channels；query、content、vector、provider/error payload 均不进入事件。
- CLI/main 不提供 memory 环境配置；只有显式 `Config.Memory` 才启用 routes、Agent adapters 与 workers。
- E2E Agent 统一通过 `routingAgentRunner.RunDetailed/newAgent` 的 production composition；Agent B 使用独立 session，恶意记忆、Scope 和权限不再由手工 Agent 组装证明。临时移除 production Projector 时 E2E 在 Memory View 门禁处按预期失败，恢复后通过。

## 验证证据

- 独立审查修复后 `go test -race ./examples/host-api/... -count=1`：通过（19.292s）。
- 独立审查修复后 standalone `GOWORK=off go test -race ./... -run 'TestMemoryRuntime|TestAgentMemory|TestHostMemoryEndToEnd' -count=1` 与 workspace/standalone `go vet`：通过。
- `go test -race ./... -run 'TestWorkflowMemoryScope|TestAgentMemory|TestMemoryRuntime|TestHostMemoryEndToEnd|TestHostAPIService'`：通过。
- 服务级 lifecycle 测试直接证明 graceful drain 等待当前 memory batch，force-stop 会取消当前 batch 且等待两个 worker 收敛。
- `go test -race ./...`：通过。
- `go vet ./...`：通过。
- `GOWORK=off go test -race ./... -run 'TestWorkflowMemoryScope|TestAgentMemory|TestMemoryRuntime|TestHostMemoryEndToEnd|TestHostAPIService'`：通过。
- `GOWORK=off go vet ./...`：通过。
- `bash scripts/verify-all.sh`：通过，最终输出 `goagents workspace verification passed`。
- Ruby YAML 解析 `examples/host-api/openapi.yaml`：通过。
- 未触及 PostgreSQL 实现；E2E 使用 `memorystore`，因此未新增 real-PG gate。

## 保留边界

- caller 必须保证 `ExtractionJobs` 与预构建 `ExtractionWorker` 使用同一 durable queue。
- caller-owned memory Store 不由 `Server.Close` 关闭。
- V1 projector 的 `Next` 仍为 nil；未引入 contextkit。
- 失败 Agent run 不 enqueue extraction job。
- 未开始 Task 13。
