# host-api execution model 设计文档

## 结论

`examples/host-api` 下一阶段继续保持简洁：默认采用 **同步执行模型**，但在
API 和内部类型上预留 `RunMode`，以后可以扩展为 queued worker。

不抽 `hostkit`。不引入 worker lease。先把单进程 durable host API 做清楚。

## 当前模型

当前 HTTP 调用会同步推进 workflow：

```text
POST /workflows
  -> 写 input artifact
  -> workflow executor 同步运行到 waiting_approval
  -> 返回 workflow 状态

POST /workflows/{id}/approve
  -> 写 approval metadata
  -> workflow executor 同步 resume
  -> 返回 succeeded/failed 状态
```

这个模型适合当前阶段，因为：

- 行为直观，测试简单。
- durable store 已经能覆盖重启恢复。
- approval 流程仍然可审计。
- 没有后台 worker 的锁、重试、可见性和清理复杂度。

## RunMode

新增概念，但第一版只实现 `sync`：

```text
RunModeSync   = "sync"
RunModeQueued = "queued"   // 预留，不实现
```

来源：

- request body 可选 `run_mode`
- 为空时默认 `sync`

第一版行为：

- `sync`：立即执行，保持现有行为。
- `queued`：返回 `400 unsupported_run_mode`，不做半成品队列。

这样 API 语义先稳定下来，但不会假装已经有 worker。

## API 边界

保持现有 endpoint：

- `POST /workflows`
- `GET /workflows/{id}`
- `POST /workflows/{id}/approve`
- `GET /agent-runs/{id}`
- `GET /llmkit/models`

暂不增加：

- `POST /workflows/{id}/resume`
- `GET /workflows/{id}/events`
- SSE
- command bus

原因：当前 durable stores 里还没有 workflow-level event store。强行加 events 会让
API 看起来比底层能力更完整。

## 后续 queued 模型

以后如果要支持 queued，建议新增明确的 worker contract：

```text
POST /workflows
  -> 保存 pending workflow
  -> 返回 202

worker
  -> claim pending/runnable workflow
  -> run until waiting/terminal
  -> release claim
```

queued 需要先设计：

- claim/lease 字段
- lease timeout
- worker heartbeat
- retry policy
- stuck run recovery
- idempotent resume

这些不进入当前阶段。

## 下一步实现

只做一个小实现：

1. 给 `POST /workflows` request 增加 `run_mode`。
2. 给 response 增加 `run_mode`。
3. 默认 `sync`。
4. `queued` 返回 `400 unsupported_run_mode`。
5. README 说明当前只支持同步执行。

## 验收

- 未传 `run_mode` 时，现有测试不变。
- 传 `run_mode: "sync"` 时，行为与默认一致。
- 传 `run_mode: "queued"` 时，返回明确错误。
- durable reopen/resume 测试继续通过。
- `./scripts/verify-all.sh` 通过。
