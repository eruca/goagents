# Host Runtime Queued Execution 设计文档

## 结论

`queued` 应分两步推进：

1. 在 `examples/host-api` 中实现一个最小 in-process queued proof，证明 HTTP API、
   workflow state、artifact refs 和 agent audit 可以从同步调用解耦。
2. durable queued worker 的第一步是扩展 workflow claim/lease contract，并在
   `examples/host-api` 提供最小 worker loop；当前已落地
   `workflowkit.QueueStore.ClaimRunnable`、`QueueLeaseStore.ExtendLease` /
   `ReleaseLease`、`Server.StartQueuedWorker`、执行期 heartbeat loop 和进程内
   worker 状态 API，但 worker crash supervision 和多 worker 调度仍未实现。

因此当前阶段把 `queued` 从“明确不支持”推进为“同进程后台执行 proof + 明确 lease
生命周期 + 执行期续租 + 同 runtime home 的 pending/expired lease 恢复”。跨进程
worker supervision、stuck recovery 和多 worker 调度仍是后续设计。

## 目标

- `POST /workflows` 支持 `run_mode: "queued"`。
- queued 请求先保存 input artifact 和 pending workflow，然后立即返回 `202`。
- 同一进程内的后台 worker 继续执行 workflow，最终进入 `waiting_approval` 或 terminal。
- host-api 启动 worker loop 后，可恢复同一 runtime home 中已有的 pending/expired lease workflow。
- `GET /workflows/{id}` 可观察 queued workflow 的状态变化。
- `GET /workflows` 可按 status 返回有界 workflow 列表，用于运营 queued worker。
- `GET /workflows/{id}/llm-routes` 和 `GET /agent-runs/{id}` 在后台执行完成后可读取审计。
- `GET /workers/queued` 可观察进程内 worker claim、completion、idle 和 error 计数。
- 不把该 proof 抽进 `workflowkit` core。

## 非目标

- 不实现 durable queue。
- 不实现 durable worker metrics。
- 不实现跨进程 worker crash supervision。
- 不实现多 worker 并发调度。
- 不把 background execution 放进 `goagent` core。

## 当前 Store 边界

`workflowkit.Store` 的基础 contract 是：

```go
Save(ctx, WorkflowRun) error
Get(ctx, id) (WorkflowRun, error)
Update(ctx, id, mutate func(WorkflowRun) (WorkflowRun, error)) (WorkflowRun, error)
```

这足够支持：

- 保存 pending workflow。
- 按 id 读取 workflow。
- 对已知 id 做状态更新。

`workflowkit.QueueStore` 当前提供 claim contract：

```go
ClaimRunnable(ctx, workerID, lease) (WorkflowRun, error)
```

`workflowkit.QueueLeaseStore` 嵌入 `QueueStore`，并提供 lease lifecycle contract：

```go
ExtendLease(ctx, workflowID, workerID, lease) (WorkflowRun, error)
ReleaseLease(ctx, workflowID, workerID) (WorkflowRun, error)
```

它支持：

- 查找 pending workflow。
- 跳过未过期 lease。
- 回收已过期 lease。
- 写入 `LeaseOwner` 和 `LeaseUntil`。
- 仅允许当前未过期 owner 续租。
- 仅允许当前 owner 释放 lease。

它仍不支持：

- stuck recovery loop。
- 多 worker 调度策略。

此外，heartbeat loop 不是 store 原语，而是 host worker 的执行职责。

## In-Process Proof 语义

`POST /workflows` 行为：

```text
run_mode omitted/sync:
  write input artifact
  executor.Run synchronously until waiting_approval or terminal
  return resulting workflow

run_mode queued:
  write input artifact
  save WorkflowRun{Status: pending, InputRef, Metadata}
  worker loop or request wakeup uses QueueLeaseStore.ClaimRunnable(...)
  executor.Run(ctx, claimedRun)
  worker releases QueueLeaseStore lease
  return pending workflow immediately
```

后台执行要求：

- 使用 detached context，避免 HTTP request context 结束后取消后台 workflow。
- 后台执行错误只写入 workflow state，不回写 HTTP response。
- 如果 goroutine 启动后失败，workflow 应最终可通过 `GET /workflows/{id}` 看到 `failed`。
- `Server.StartQueuedWorker(ctx)` 按固定间隔 claim runnable workflow，能够处理启动前遗留的 pending/expired lease workflow。

## HTTP Contract

`POST /workflows` queued response：

```json
{
  "id": "wf-queued-1",
  "status": "pending",
  "run_mode": "queued",
  "input_ref": "artifact:wf-queued-1:input"
}
```

状态推进后：

```json
{
  "id": "wf-queued-1",
  "status": "waiting_approval",
  "run_mode": "queued",
  "input_ref": "artifact:wf-queued-1:input",
  "output_ref": "artifact:wf-queued-1:agent-output",
  "agent_run_id": "...",
  "approval_ref": "approval:wf-queued-1"
}
```

当前 submitted `run_mode` 持久化在 workflow metadata 中，所以 queued workflow 在
后续 `GET /workflows/{id}`、`GET /workflows` 和 approval response 中仍返回
`run_mode: "queued"`。

`GET /workflows?status=pending&limit=50` 返回有界运营列表：

```json
{
  "workflows": [
    {
      "id": "wf-queued-1",
      "status": "pending",
      "run_mode": "queued",
      "input_ref": "artifact:wf-queued-1:input"
    }
  ]
}
```

`GET /workers/queued` 返回进程内 worker 诊断：

```json
{
  "started": true,
  "worker_id": "host-api-inprocess-worker",
  "claim_attempts": 3,
  "claimed": 1,
  "completed": 1,
  "idle": 2,
  "errors": 0,
  "lease_extensions": 4,
  "heartbeat_errors": 0,
  "last_heartbeat_workflow_id": "wf-queued-1",
  "last_workflow_id": "wf-queued-1"
}
```

这些计数重启后清零，只用于本地诊断，不是持久 metrics contract。

## 后续 Durable Worker Contract

真正 durable queued worker 仍需要更完整的 host-side worker contract，例如：

```go
type WorkerLoop interface {
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Heartbeat(ctx context.Context) error
    RecoverStuck(ctx context.Context) error
}
```

对应 workflow 字段可能包括：

- `LeaseOwner`
- `LeaseUntil`
- `Attempt`
- `NextRunAt`
- `QueuedAt`
- `StartedAt`

`LeaseOwner` 和 `LeaseUntil` 已进入 `WorkflowRun`、memory store、SQLite store 和
store conformance，并已具备 claim/extend/release 语义。`examples/host-api`
已提供最小 `StartQueuedWorker` loop 和进程内 worker 状态 API。其余字段仍待后续
durable worker 设计。

## 测试计划

本阶段 proof 覆盖：

- `POST /workflows` with `run_mode: queued` returns `202` and `pending` immediately.
- polling `GET /workflows/{id}` eventually reaches `waiting_approval`。
- `GET /workflows` filters by status and preserves submitted `run_mode`。
- queued workflow completion exposes `agent_run_id` and `output_ref`。
- queued worker claims through `QueueLeaseStore` and clears the lease after execution stops。
- queued worker extends the lease while a workflow is still running。
- reopening with the same runtime home and starting the worker loop recovers pending workflow。
- `GET /workers/queued` exposes process-local worker counters and latest execution error。
- after queued execution reaches waiting approval, `POST /workflows/{id}/approve` succeeds。
- `GET /workflows/{id}/llm-routes` returns route audit after background execution。

不测试：

- multi-worker claim。
- worker crash recovery。

## 完成标准

- `examples/host-api` 支持 in-process queued proof。
- README、OpenAPI、`docs/host-api-contract.md` 明确 queued 当前是 in-process worker proof。
- worker 状态 API 明确是进程内诊断，不是 durable metrics。
- `./scripts/verify-all.sh` 通过。
