# Host Runtime Queued Execution 设计文档

## 结论

`queued` 应分两步推进：

1. 在 `examples/host-api` 中实现一个最小 in-process queued proof，证明 HTTP API、
   workflow state、artifact refs 和 agent audit 可以从同步调用解耦。
2. durable queued worker 的第一步是扩展 workflow claim/lease contract；当前已落地
   `workflowkit.QueueStore.ClaimRunnable`，但 heartbeat、release 和 stuck recovery 仍未实现。

因此本阶段只把 `queued` 从“明确不支持”推进为“同进程后台执行 proof”。进程重启后
自动恢复 pending workflow、跨进程 worker heartbeat 和 stuck recovery 仍是后续设计。

## 目标

- `POST /workflows` 支持 `run_mode: "queued"`。
- queued 请求先保存 input artifact 和 pending workflow，然后立即返回 `202`。
- 同一进程内的后台 worker 继续执行 workflow，最终进入 `waiting_approval` 或 terminal。
- `GET /workflows/{id}` 可观察 queued workflow 的状态变化。
- `GET /workflows/{id}/llm-routes` 和 `GET /agent-runs/{id}` 在后台执行完成后可读取审计。
- 不把该 proof 抽进 `workflowkit` core。

## 非目标

- 不实现 durable queue。
- 不实现 worker heartbeat。
- 不实现进程重启后自动恢复 pending workflow。
- 不实现多 worker 并发调度。
- 不新增 workflow list API。
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

`workflowkit.QueueStore` 当前增加了第一步 claim/lease contract：

```go
ClaimRunnable(ctx, workerID, lease) (WorkflowRun, error)
```

它支持：

- 查找 pending workflow。
- 跳过未过期 lease。
- 回收已过期 lease。
- 写入 `LeaseOwner` 和 `LeaseUntil`。

它仍不支持：

- heartbeat。
- worker release。
- stuck recovery loop。
- 多 worker 调度策略。

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
  start goroutine
  goroutine uses QueueStore.ClaimRunnable(...)
  executor.Run(ctx, claimedRun)
  return pending workflow immediately
```

后台执行要求：

- 使用 detached context，避免 HTTP request context 结束后取消后台 workflow。
- 后台执行错误只写入 workflow state，不回写 HTTP response。
- 如果 goroutine 启动后失败，workflow 应最终可通过 `GET /workflows/{id}` 看到 `failed`。

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
  "run_mode": "sync",
  "input_ref": "artifact:wf-queued-1:input",
  "output_ref": "artifact:wf-queued-1:agent-output",
  "agent_run_id": "...",
  "approval_ref": "approval:wf-queued-1"
}
```

当前 `WorkflowRun` 没有持久化 run mode 字段，所以 `GET /workflows/{id}` 仍可返回
`run_mode: "sync"` 作为兼容 fallback。后续如果需要准确保留 submitted run mode，应在
workflow metadata 或正式 DTO 中增加 `run_mode`。

## 后续 Durable Worker Contract

真正 durable queued worker 需要新增 contract，例如：

```go
type QueueStore interface {
    ClaimRunnable(ctx context.Context, workerID string, lease time.Duration) (WorkflowRun, error)
    Heartbeat(ctx context.Context, workflowID string, workerID string, leaseUntil time.Time) error
    Release(ctx context.Context, workflowID string, workerID string, result WorkflowRun) error
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
store conformance。其余字段仍待后续 durable worker 设计。

## 测试计划

本阶段 proof 覆盖：

- `POST /workflows` with `run_mode: queued` returns `202` and `pending` immediately.
- polling `GET /workflows/{id}` eventually reaches `waiting_approval`。
- queued workflow completion exposes `agent_run_id` and `output_ref`。
- queued workflow records `LeaseOwner` and `LeaseUntil` through `QueueStore`。
- after queued execution reaches waiting approval, `POST /workflows/{id}/approve` succeeds。
- `GET /workflows/{id}/llm-routes` returns route audit after background execution。

不测试：

- process restart recovery。
- multi-worker claim。
- lease timeout。
- worker crash recovery。

## 完成标准

- `examples/host-api` 支持 in-process queued proof。
- README、OpenAPI、`docs/host-api-contract.md` 明确 queued 当前是 in-process proof。
- `./scripts/verify-all.sh` 通过。
