# host-api durable runtime 设计文档

## 背景

`examples/host-api` 已经证明 host 可以把 `workflowkit`、`goagent`、
`llmkit`、`artifactkit` 和 `runkit` 组合成 HTTP API。但当前实现仍主要是
内存态 skeleton：进程重启后 workflow、artifact、agent run 和 provider
health 都会丢失。

下一步目标不是把 example 抽成新的框架，而是把这个 host 组合示例推进到
可恢复、可审计、可长期运行的形态。只有当 durable example 稳定后，再判断
是否有必要抽出 `hostkit`。

## 目标

- 让 `examples/host-api` 支持重启后恢复 workflow 和 agent run 审计记录。
- 使用已有 durable store：`workflowkit/sqlitestore` 和 `runkit/sqlitestore`。
- 明确 runtime 目录、数据库文件、artifact 目录和 `LLMKIT_HOME` 的来源。
- 保持 API 行为不变，先替换底层持久化组合。
- 保留 host-owned 边界：业务 API、鉴权、多租户、真实 provider client 仍由 host 拥有。

## 非目标

- 不在本阶段抽 `hostkit` module。
- 不把 `examples/host-api` 变成生产级服务器。
- 不实现认证、租户隔离、限流、后台 worker、dashboard 或 OpenAPI。
- 不在数据库中保存明文 prompt、response、tool output 或 API key。
- 不把 provider health 第一版做成分布式共享状态。

## Runtime Home

新增 `HOST_RUNTIME_HOME`，作为 host API 的总运行目录：

```bash
HOST_RUNTIME_HOME=/srv/my-agent/runtime
```

目录布局：

```text
$HOST_RUNTIME_HOME/
  workflow.db
  agent-runs.db
  artifacts/
  .llmkit/
    route-events.jsonl
    outcomes.jsonl
    model-stats.json
```

解析规则：

1. 如果显式设置 `HOST_RUNTIME_HOME`，使用它。
2. 如果未设置，示例程序启动时创建临时目录，保持当前 demo 体验。
3. 如果同时设置 `LLMKIT_HOME`，优先使用显式 `LLMKIT_HOME`。
4. 如果未设置 `LLMKIT_HOME`，默认使用 `$HOST_RUNTIME_HOME/.llmkit`。

这样可以把 host runtime 的持久化目录和 llmkit 审计目录统一起来，同时仍然允许
生产 host 把 llmkit 审计放到单独位置。

## Store 组合

`examples/host-api` 当前组合：

```text
workflowkit.MemoryStore
runkit.MemoryStore
artifactkit.MemoryStore
llmkit.MemoryHealthStore
```

下一步替换为：

```text
workflowkit/sqlitestore.Open($HOST_RUNTIME_HOME/workflow.db)
runkit/sqlitestore.Open($HOST_RUNTIME_HOME/agent-runs.db)
artifactkit.FileStore($HOST_RUNTIME_HOME/artifacts)
llmkit.MemoryHealthStore
```

其中 `artifactkit.FileStore` 需要先补，因为 `artifactkit` 目前只有
`MemoryStore`。它应仍然只存 artifact payload，不承担 workflow 或 run 查询。

## Artifact 持久化

`artifactkit.FileStore` 的最小合同：

- 实现现有 `artifactkit.Store`。
- `Put(ctx, Artifact)` 写入内容和 metadata。
- `Get(ctx, ref)` 读取 artifact。
- 路径由 ref 的安全编码生成，不能把 ref 直接拼入文件路径。
- copy-on-read/write 语义保持不变。
- 遵守 context cancellation。

建议文件布局：

```text
$ARTIFACT_ROOT/
  objects/
    <url-safe-base64-ref>.json
```

JSON 内容：

```json
{
  "ref": "artifact:wf-1:input",
  "content_type": "text/plain",
  "content_base64": "...",
  "metadata": {},
  "created_at": "..."
}
```

第一版可以采用单文件 JSON，避免过早拆 blob/meta。后续如果 artifact 很大，再演进为
metadata JSON + blob 文件。

## HTTP API 行为

现有 API 不改路径：

- `POST /workflows`
- `GET /workflows/{id}`
- `POST /workflows/{id}/approve`
- `GET /agent-runs/{id}`
- `GET /llmkit/models`

新增 durable 验收行为：

- 创建 workflow 后重建 `Server`，`GET /workflows/{id}` 仍能读到 waiting 状态。
- 重建后 `POST /workflows/{id}/approve` 可以继续 resume 并完成。
- 重建后 `GET /agent-runs/{id}` 仍能读到 terminal summary 和 events。
- 重建后 artifact refs 仍可解析，finalize step 能读取 agent output artifact。

## Provider Health

第一版继续使用 `llmkit.MemoryHealthStore`。

理由：

- provider health 是 runtime 状态，不等同于 durable audit。
- 单进程 host API 的 in-flight 并发只能在进程内准确维护。
- 真正的分布式 provider health 需要 lease、心跳、过期策略和多 worker 协调，不适合塞进当前阶段。

但 API 要保持 `HealthStore` 依赖注入，未来可替换为 durable/shared 实现。

重启后丢失 provider in-flight 状态是可接受的；历史成功率和失败率由 `llmkit`
audit files + `model-stats.json` 恢复。

## 错误与恢复

- workflow DB 打不开：server 启动失败。
- agent run DB 打不开：server 启动失败。
- artifact root 不可写：server 启动失败。
- 单个 workflow 执行失败：返回 workflow error，不删除已写入的 audit/artifact。
- approval resume 失败：保留当前 workflow 状态，返回错误。
- route/outcome JSONL 写入失败：当前仍按 adapter 错误返回，避免静默丢审计。

## 测试计划

新增测试应先覆盖 durable 语义，而不是只测 HTTP happy path：

- `artifactkit.FileStore` store conformance。
- `artifactkit.FileStore` reopen 后可读取 artifact。
- `examples/host-api` 使用固定 runtime home：
  - create workflow
  - new server with same runtime home
  - get workflow
  - approve workflow
  - get agent run
  - verify final artifact ref exists
- `GOWORK=off go test ./...` for `artifactkit` and `examples/host-api`。
- `./scripts/verify-all.sh`。

## 实施顺序

1. 在 `artifactkit` 增加 `FileStore` 和共享 store conformance 测试。
2. 在 `examples/host-api` 增加 runtime home config：
   - `RuntimeHome`
   - `WorkflowDBPath`
   - `AgentRunDBPath`
   - `ArtifactRoot`
   - `LLMKitHome`
3. 将 `examples/host-api` 默认 server composition 改为 durable stores。
4. 增加 reopen/resume HTTP 测试。
5. 更新 README、`docs/modules.md` 和 `scripts/verify-all.sh`。

## 暂不抽 hostkit 的判断

现在抽 `hostkit` 还早。原因是公共 API 边界尚未稳定：

- artifact 持久化策略刚开始落地。
- provider health 是否需要共享 store 还没验证。
- HTTP API 是否需要 background worker、SSE 或 dashboard 还未定。
- host 的认证、租户、权限模型很可能由具体项目决定。

因此本阶段继续把能力放在 `examples/host-api`，把它作为 canonical host
composition。等 durable example 能支撑真实 host 的一轮接入，再评估是否抽
`hostkit`。

## 完成标准

- `examples/host-api` 重启后可以恢复并完成 waiting approval workflow。
- workflow、agent run、artifact 三类关键状态都有 durable backend。
- llmkit audit 继续写入 `LLMKIT_HOME`。
- provider health 仍可参与 routing，但明确为进程内 runtime 状态。
- 所有验证脚本通过。
