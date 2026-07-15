# Host API MVP 单机稳定性设计

**日期：** 2026-07-15

**状态：** 已实现并验收

## 1. 决策摘要

第 4 阶段只验证当前单机 Host 的稳定性，不测试真实 Qwen 吞吐，也不把结果外推为生产
SLO。目标环境固定为 Apple M1 Pro、10 核、16GB、macOS 26.5.1；Provider 和 OIDC
均使用 loopback 测试服务，凭证使用合成值。

本阶段采用两层门禁：

1. 修正 queued workflow 会按请求创建执行 goroutine 的无界并发问题，把每个 `Server`
   收敛为一个执行槽；
2. 用真实 Host 二进制、HTTP、SQLite、OIDC 和进程重启完成三轮负载，再用进程内回归
   测试验证单槽执行和 goroutine 回落。

不增加生产 HTTP endpoint、并发配置、数据库迁移、依赖或多 worker 调度。

## 2. 已确认的问题

当前 `POST /workflows` 和 requeue 在保存 pending workflow 后调用 `runQueuedWorkflow`，后者
为每个请求启动一个 goroutine；`StartQueuedWorker` 同时还启动轮询 goroutine。
`ClaimRunnable` 能保证同一 workflow 不被同时 claim，但不会限制同一个 `worker_id` 同时
持有多个 workflow。因此请求并发可以直接转化为执行并发和 goroutine 数量，与现有
“单进程、单 worker proof”的边界不一致。

这不是要补多 worker 调度，而是要让现有 MVP 边界真实成立。修复必须保持：

- queued 请求仍立即返回 `202 pending`；
- worker 能恢复启动前已有的 pending workflow；
- claim、heartbeat、release、requeue 和 worker status 语义不变；
- `main` 继续显式启动 worker。

## 3. 单槽 worker pump

`Server` 增加一个容量为 1 的唤醒通道和一次性启动保护。`NewServer` 只初始化它，不启动
后台执行。`StartQueuedWorker(ctx)` 每个 Server 生命周期只启动一个 worker loop；重复调用
不得创建第二个执行循环。

创建 queued workflow 和 requeue 成功后，只做非阻塞唤醒：

```text
save pending workflow
  -> try send wake signal to size-1 channel
  -> return 202

single worker loop
  -> claim one runnable workflow
  -> run with heartbeat
  -> release lease
  -> continue draining until no runnable workflow
  -> wait for wake, poll interval, or context cancellation
```

容量 1 只表达“队列可能有新工作”，不按 workflow 计数；真正的事实源仍是 SQLite。
worker 每次执行完继续 claim，直到 store 返回 `ErrNoRunnableWorkflow`，因此合并唤醒不会
丢任务。100ms 轮询保留为恢复兜底。

若宿主没有调用 `StartQueuedWorker`，queued workflow 保持 `pending`，唤醒信号留在通道中，
直到 worker 启动后统一 drain。测试中凡是期待后台执行的场景必须显式启动 worker；只检查
pending、查询或持久化的测试不需要启动。

一个 Server 的 worker context 取消后不在同一实例上重启；进程重启或 worker 生命周期重建
使用新的 Server。这与当前 Host 进程模型一致。

## 4. 进程内并发回归门禁

在默认 Host 测试中增加一个可控阻塞 step，并发提交一组 queued workflow：

- step 记录当前活跃数、最大活跃数和每个 workflow 的执行次数；
- worker 启动后并发提交，旧实现应观察到最大活跃数大于 1；
- 新实现必须始终 `max_active == 1`；
- 每个 workflow 的执行次数必须为 1；
- 所有 workflow 收敛后取消 worker context，关闭测试 store，并等待 goroutine 数回落到
  暖机基线 `+5` 以内。

该测试直接证明单槽语义和无重复执行，不依赖操作系统采样，也不需要暴露 debug endpoint。

另加一次重复 `StartQueuedWorker` 回归，证明重复启动不会形成第二个执行槽。

## 5. 真实进程稳定性门禁

新增一个 `darwin && cgo && hostapisystemsmoke` 测试，复用现有 Host 二进制、loopback OIDC、
loopback OpenAI-compatible Provider、合成 API key、临时 runtime home 和隔离 `.smoke.`
Keychain service/account。

测试夹具可以做一次不计入正式负载的暖机 workflow，使 HTTP 连接、SQLite 和 Go runtime
进入稳定状态。正式负载固定为三轮，每轮 50 个 workflow，共 150 个：

1. 10 路并发提交 queued workflow；
2. 等待全部进入 `waiting_approval`；
3. 10 路并发发送 OIDC final approval；
4. 等待全部进入 `succeeded`；
5. 核对 workflow、AgentRun、output ref、路由审计和 worker 计数。

第一、二轮在同一 Host 进程完成。第二轮完成并确认队列空闲后正常停止进程，再用同一
runtime home 启动新 Host：逐个查询前 100 个 workflow 仍为 `succeeded`，然后执行第三轮。
重启发生在完整波次之间，避免把 provider 调用中断后的至少一次恢复误判成重复执行。

每个正式 workflow 必须只有一个成功 AgentRun 引用和一个成功 Provider route；Provider
请求总数必须与暖机加正式 workflow 数一致。负载场景不注册写工具，不制造 150 次外部
副作用；不可重复执行的工具审批语义继续由第 2 阶段黑盒 smoke 负责。

进程停止后只读核对 SQLite workflow 记录，确认 150 个正式 workflow 均成功且
`lease_owner`、`lease_until` 已清空。测试不得修改 SQLite 来制造通过状态。

## 6. 延迟与收敛阈值

这些阈值是当前目标机器上的 MVP 卡死/严重退化门禁，不是生产性能承诺：

- 单个 workflow 提交请求最长 2 秒；
- 每轮 50 个提交在 10 秒内全部返回；
- 每轮从提交完成到全部 `waiting_approval` 最长 30 秒；
- 每轮 50 个 final approval 在 10 秒内全部返回；
- 每轮从审批开始到全部 `succeeded` 最长 30 秒；
- Host 重启后 30 秒内完成 readiness、前 100 条状态复核并可继续接收第三轮；
- 每轮记录提交和审批的 p50/p95 以及两个收敛耗时，但不设置更紧的 p95 硬门禁。

任何超时都必须报告所在波次、阶段和未收敛 workflow ID，不延长超时掩盖卡死。

## 7. 资源门禁

真实进程通过 `hostProcess.cmd.Process.Pid` 采样，不新增生产观测接口：

- `ps -o rss=` 读取 RSS；
- `lsof -nP -a -p <pid> -Fn` 统计打开的文件描述符；
- 缺少命令或无权限时测试状态为环境 `SKIP`，不能视为 MVP 验收通过。

每个进程在暖机并空闲后记录基线。硬门禁为：

- 每轮空闲后的 FD 不超过该进程暖机基线 `+5`；
- 第二轮相对第一轮新增 FD 不超过 2；
- RSS 不超过该进程暖机基线 `+64 MiB`；
- 第二轮相对第一轮新增 RSS 不超过 32 MiB。

RSS 允许 Go runtime 保留已申请内存，因此不要求精确回落。每轮输出基线、当前值和差值，
第 5 阶段再把观测值写入 MVP 验收记录。

## 8. 错误、安全与可审计性

- 任一非 `202` 提交、非 `200` 审批、Provider 错误、worker error 或 heartbeat error 都使
  测试失败；
- 所有 workflow ID 必须唯一，重复 ID 不通过重试掩盖；
- Provider 只接受合成 Authorization，Host 输出不得包含合成 API key、OIDC token、
  Skill 根路径或 Keychain 内容；
- Keychain 清理只删除本测试精确的 `.smoke.` service/account，不接触生产默认 item；
- 负载指标和报告只记录计数、耗时、状态与脱敏 ID，不保存请求正文、token 或原始数据库。

## 9. 验收命令

默认回归必须通过：

```bash
cd examples/host-api
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

真实进程稳定性门禁使用独立测试名，避免与较短的 MVP 黑盒闭环混淆：

```bash
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPStability$' \
  -count=1 ./...
```

最终还必须通过：

```bash
bash ./scripts/verify-all.sh
git diff --check
```

## 10. 非目标

- 真实 Qwen 或云 Provider 性能；
- 多 worker、跨进程 worker、动态并发度或限流配置；
- crash-at-provider 的 exactly-once 语义；
- 生产压测工具、dashboard、pprof endpoint 或长期遥测；
- 超过 150 个 workflow 的容量承诺；
- 为满足阈值硬编码 workflow ID、sleep 或 Provider 响应。

## 11. 实现与稳定性观测

实现提交：

- `a8c4130`：queued create/requeue 改为有界唤醒，`StartQueuedWorker` 每个 Server 只启动
  一个执行槽；默认回归记录旧实现最大活跃执行数为 14，修复后为 1；
- `438fe30`：增加真实进程 3×50 稳定性门禁、OIDC final approval、同 runtime home 重启、
  Provider/AgentRun/route 唯一性检查、SQLite lease 核对和 `ps`/`lsof` 资源采样。

2026-07-15 在目标机器连续执行两次稳定性门禁，均为 PASS 且无 SKIP：

- 三个正式波次的 submit p95 为 9.87–13.91ms；
- waiting-approval 收敛为 1.19–1.38s；
- approve p95 为 34.08–198.02ms，approval-to-success 收敛为 139.64–329.88ms；
- 暖机 RSS 为 25.3–27.7MB，正式波次 RSS 为 32.8–35.5MB；
- 暖机 FD 为 13，三个正式波次均为 14，第二轮未继续增长；
- 150 个正式 workflow 均 succeeded，Provider 请求总数与 2 个暖机加 150 个正式
  workflow 完全一致，worker/heartbeat error 为 0，最终 lease 全部清空。

上述数值是本机 MVP 回归证据，不是生产 SLO。Host 默认测试、race、默认/带标签 vet、
两个带标签真实进程门禁和 `scripts/verify-all.sh` 均已通过。
