# Host API MVP Stability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 Host queued 执行收敛为单槽 worker pump，并在 Apple M1 Pro 上建立可重复的 3×50 真实进程稳定性门禁。

**Architecture:** `Server` 使用容量为 1 的 wake channel 和一次性启动保护，queued create/requeue 只唤醒唯一 worker loop。默认测试用可控 step 证明最大执行并发为 1；带 `hostapisystemsmoke` 标签的真实进程测试通过 loopback Provider/OIDC、SQLite、OIDC final approval、进程重启以及 `ps`/`lsof` 资源采样完成 150 个 workflow。

**Tech Stack:** Go 1.26.1、Gin-free `net/http` Host、SQLite、workflowkit、runkit、llmkit、macOS Keychain、`ps`、`lsof`、Go testing。

## Global Constraints

- 目标环境固定为 Apple M1 Pro、10 核、16GB、macOS 26.5.1。
- 不使用真实 Qwen 或云 Provider；Provider、OIDC 和凭证均为 loopback/合成测试夹具。
- 不新增生产 HTTP endpoint、并发配置、数据库迁移、外部依赖或多 worker 调度。
- queued 请求仍立即返回 `202 pending`；SQLite 是唯一队列事实源。
- 单个提交最长 2 秒；每轮提交和审批各 10 秒；waiting/succeeded 收敛各 30 秒。
- 真实进程 FD 以暖机基线 `+5` 为硬门禁，RSS 以 `+64 MiB` 为宽松硬门禁；goroutine
  由进程内取消回归独立验证。
- 第二轮相对第一轮新增 FD 不超过 2，新增 RSS 不超过 32 MiB。
- 不保存请求正文、token、API key、Keychain 内容、本机绝对信任路径或原始数据库。
- 所有生产代码改动必须先有可观察的 RED；不得用 sleep、固定输出或特定 workflow ID 绕过真实行为。
- 提交信息使用简体中文，按 worker 修复、稳定性门禁和文档三个语义边界提交。

---

### Task 1: 把 queued worker 收敛为单执行槽

**Files:**
- Modify: `examples/host-api/server.go`
- Modify: `examples/host-api/server_test.go`

**Interfaces:**
- Produces: `Server.workerWake chan struct{}`
- Produces: `Server.workerStart sync.Once`
- Produces: `func (s *Server) signalQueuedWorker()`
- Preserves: `func (s *Server) StartQueuedWorker(context.Context)`
- Preserves: `func (s *Server) runOneQueuedWorkflow(context.Context) (bool, error)`

- [ ] **Step 1: 写单槽和重复启动失败测试**

在 `server_test.go` 增加线程安全的计数 step。它必须按 workflow ID 计数，并在 release channel
关闭前阻塞，使旧实现有机会暴露多个同时运行的 step：

```go
type singleSlotStep struct {
	mu        sync.Mutex
	active    int
	maxActive int
	counts    map[string]int
	started   chan struct{}
	release   <-chan struct{}
}

func newSingleSlotStep(release <-chan struct{}, capacity int) *singleSlotStep {
	return &singleSlotStep{
		counts:  make(map[string]int),
		started: make(chan struct{}, capacity),
		release: release,
	}
}

func (s *singleSlotStep) Name() string { return "single_slot" }

func (s *singleSlotStep) Run(ctx context.Context, run workflowkit.WorkflowRun) (workflowkit.StepResult, error) {
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.counts[run.ID]++
	s.mu.Unlock()

	s.started <- struct{}{}
	select {
	case <-ctx.Done():
		return workflowkit.StepResult{}, ctx.Err()
	case <-s.release:
	}

	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return workflowkit.StepResult{
		Status:        workflowkit.StatusWaitingApproval,
		ApprovalRef:   "approval:" + run.ID,
		WaitingReason: "single-slot test completed",
	}, nil
}

func (s *singleSlotStep) snapshot() (int, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int, len(s.counts))
	for id, count := range s.counts {
		counts[id] = count
	}
	return s.maxActive, counts
}
```

增加 `TestHostAPIQueuedWorkerUsesSingleExecutionSlot`：

1. 创建真实 SQLite-backed `NewServer`，把 executor 换为 `singleSlotStep`；
2. 记录 `runtime.NumGoroutine()` 暖机基线；
3. 调用一次 `StartQueuedWorker(ctx)`；
4. 用 10 个 goroutine 并发 POST 20 个 queued workflow，并等待所有 POST 返回；
5. 等待第一个 step 进入后，使用 500ms deadline 观察是否出现第二个活跃 step，然后关闭 release；
6. 等待 20 个 workflow 全部 `waiting_approval`，断言 `maxActive == 1` 且每个 ID 只执行一次；
7. cancel worker、关闭两个 SQLite store，并在 2 秒 deadline 内等待 goroutine 数不超过基线 `+5`。

测试不得用固定 sleep 判定完成；等待都使用状态、channel 或带诊断的 deadline。

再增加 `TestHostAPIQueuedWorkerStartsOnce`：对同一 Server 连续调用两次
`StartQueuedWorker(ctx)`，提交同样的阻塞 workflow 集合，并复用 `maxActive == 1` 断言。

- [ ] **Step 2: 运行测试确认 RED**

Run:

```bash
cd examples/host-api
go test ./... \
  -run '^(TestHostAPIQueuedWorkerUsesSingleExecutionSlot|TestHostAPIQueuedWorkerStartsOnce)$' \
  -count=1
```

Expected: FAIL；当前实现会观察到 `maxActive > 1`，或重复 `StartQueuedWorker` 创建第二个执行循环。

- [ ] **Step 3: 实现容量为 1 的 worker pump**

在 `Server` 增加：

```go
workerWake  chan struct{}
workerStart sync.Once
```

在 `NewServer` 构造值中初始化：

```go
workerWake: make(chan struct{}, 1),
```

把 create 和 requeue 末尾的 `s.runQueuedWorkflow(run)` 替换为：

```go
s.signalQueuedWorker()
```

删除按请求创建 goroutine 的 `runQueuedWorkflow`，改为非阻塞唤醒：

```go
func (s *Server) signalQueuedWorker() {
	select {
	case s.workerWake <- struct{}{}:
	default:
	}
}
```

使 `StartQueuedWorker` 只启动一次：

```go
func (s *Server) StartQueuedWorker(ctx context.Context) {
	s.workerStart.Do(func() {
		s.worker.markStarted(queuedWorkerID)
		go s.runQueuedWorkerLoop(ctx)
	})
}
```

在 worker loop 没有 claim 到任务时，同时等待 wake、100ms poll 或取消：

```go
func (s *Server) runQueuedWorkerLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		ran, _ := s.runOneQueuedWorkflow(ctx)
		if ran {
			continue
		}
		timer := time.NewTimer(queuedPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.workerWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}
```

不得增加第二个执行 goroutine、semaphore 配置或新的 worker 类型。

- [ ] **Step 4: 显式启动依赖后台执行的现有测试**

以下测试当前依赖 create 自动启动 goroutine，改为在 POST 前创建 context 并调用
`server.StartQueuedWorker(ctx)`：

- `TestHostAPIQueuedWorkerExtendsLeaseWhileWorkflowRuns`
- `TestHostAPIRunModeSyncAndQueuedSemantics`

Run:

```bash
rg -n -B 8 -A 18 'waitForWorkflowStatus|waitForWorkflowLease' examples/host-api/server_test.go
```

Expected: 除上述两个测试外，其余等待 queued 状态变化的测试已经在 POST/requeue 前显式调用
`StartQueuedWorker`；只检查 `pending`、list 或持久化的测试不启动 worker。

- [ ] **Step 5: 运行 GREEN、默认回归和 race**

Run:

```bash
cd examples/host-api
go test ./... \
  -run '^(TestHostAPIQueuedWorkerUsesSingleExecutionSlot|TestHostAPIQueuedWorkerStartsOnce|TestHostAPIQueuedWorkerExtendsLeaseWhileWorkflowRuns|TestHostAPIRunModeSyncAndQueuedSemantics)$' \
  -count=1
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

Expected: 全部 PASS；race detector 无数据竞争。

- [ ] **Step 6: 提交单槽修复**

```bash
git add examples/host-api/server.go examples/host-api/server_test.go
git commit -m "fix(host-api): 限制队列为单槽执行"
```

### Task 2: 增加 3×50 真实进程稳定性门禁

**Files:**
- Create: `examples/host-api/host_mvp_stability_smoke_test.go`

**Interfaces:**
- Consumes: `hostProcess`, `startHostProcessWithEnv`, `stopHostProcess`, `mvpProviderStub`
- Produces: `TestHostAPIProcessMVPStability`
- Produces: `processResourceSnapshot{RSSBytes int64, FileDescriptors int}`
- Produces: `stabilityWaveResult` with submit/approve latencies and convergence durations
- Produces: `func assertStabilityRuntime(*testing.T, string, []string)`

- [ ] **Step 1: 写稳定性测试骨架并确认 RED**

创建带现有 build tag 的 `host_mvp_stability_smoke_test.go`：

```go
//go:build darwin && cgo && hostapisystemsmoke

package main

func TestHostAPIProcessMVPStability(t *testing.T) {
	requireInteractiveLoginKeychain(t)
	requireStabilityResourceTools(t)

	const (
		waves       = 3
		perWave     = 50
		concurrency = 10
	)
	if waves*perWave != 150 {
		t.Fatal("stability workload must remain 3x50")
	}

	// Setup binary, loopback Provider/OIDC, runtime home, synthetic key and
	// isolated Keychain identity. Run two waves, restart with the same home,
	// verify the first 100 workflows, then run the third wave.
}
```

先引用尚未实现的 `requireStabilityResourceTools`、`runStabilityWave`、
`sampleProcessResources` 和 `assertStabilityRuntime`。

Run:

```bash
cd examples/host-api
go test -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPStability$' \
  -count=1 ./...
```

Expected: 编译 FAIL，提示上述稳定性 helper 未定义。

- [ ] **Step 2: 实现无 `testing.T` goroutine 终止的并发 HTTP helper**

在新文件实现通用调用结果；并发 goroutine 只返回 error，不调用 `t.Fatal`：

```go
type stabilityCall[T any] struct {
	Value    T
	Status   int
	Duration time.Duration
	Err      error
}

func callProcessJSON[T any](process *hostProcess, method, path string, body any, authorization string) stabilityCall[T]
```

该 helper 必须限制响应体为 1MiB、解码错误响应、记录 duration，并由上层按 workflow ID
汇总错误。实现固定 10 个 worker 的 job channel，不为 50 个请求各创建一个不受控 goroutine。

实现 `percentileDuration`：复制并排序 duration，p50 使用 0.50、p95 使用 0.95 的向上取整
索引；空切片返回 0。

- [ ] **Step 3: 实现进程资源采样**

```go
type processResourceSnapshot struct {
	RSSBytes        int64
	FileDescriptors int
}

func requireStabilityResourceTools(t *testing.T) {
	t.Helper()
	for _, command := range []string{"ps", "lsof"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("MVP stability requires %s: %v", command, err)
		}
	}
}

func sampleProcessResources(pid int) (processResourceSnapshot, error)
```

`sampleProcessResources` 使用 `ps -o rss= -p <pid>` 解析 KiB 后乘 1024；使用
`lsof -nP -a -p <pid> -Fn` 统计以 `f` 开头的记录。PID 不存在、输出为空或解析失败必须返回
error，不能返回零值伪装成功。

实现断言：

```go
const (
	maxFDOverBaseline       = 5
	maxFDGrowthBetweenWaves = 2
	maxRSSOverBaseline      = int64(64 << 20)
	maxRSSGrowthSecondWave  = int64(32 << 20)
)
```

再实现：

```go
func assertProcessResourceBounds(
	t *testing.T,
	label string,
	baseline processResourceSnapshot,
	current processResourceSnapshot,
)

func assertSecondWaveResourceGrowth(
	t *testing.T,
	first processResourceSnapshot,
	second processResourceSnapshot,
)
```

第一个 helper 检查 FD `+5` 和 RSS `+64 MiB`；第二个检查 wave 2 相对 wave 1 的 FD `+2`
和 RSS `+32 MiB`。错误信息必须同时输出 baseline、current 和 delta。

- [ ] **Step 4: 实现一轮完整 workflow 生命周期**

```go
type stabilityWaveResult struct {
	IDs                 []string
	SubmitLatencies     []time.Duration
	ApproveLatencies    []time.Duration
	WaitingConvergence  time.Duration
	SuccessConvergence  time.Duration
	Resources           processResourceSnapshot
}

func runStabilityWave(
	t *testing.T,
	process *hostProcess,
	token string,
	wave int,
	count int,
	concurrency int,
) stabilityWaveResult
```

每个 ID 使用 `wf-mvp-stability-w<波次>-<三位序号>`。提交 payload 固定为 queued、simple、
cloud_allowed、无 tools。`runStabilityWave` 必须：

1. 10 路并发 POST，逐个要求 `202 pending`、单请求 `<=2s`、整轮 `<=10s`；
2. 在 30 秒 deadline 内按批查询，全部达到 `waiting_approval`，失败时列出未收敛 ID；
3. 10 路并发 final approve，逐个要求 `200`、整轮 `<=10s`；
4. 在 30 秒 deadline 内全部达到 `succeeded`；
5. 每个 workflow 必须有非空且唯一的 AgentRunID、最终 OutputRef 和恰好一个成功 route；
6. `/workers/queued` 的 error/heartbeat error 必须为 0，claimed/completed 至少覆盖本进程已跑数量；
7. 输出 submit/approve p50/p95、两个收敛耗时、RSS 和 FD。

- [ ] **Step 5: 完成三轮、重启和只读持久化核对**

完整测试流程：

1. 构建 binary，启动 Provider/OIDC，写 llmkit config，创建独立 `.smoke.stability.` Keychain identity；
2. 第一进程把 HTTP client timeout 设为 2 秒，通过
   `runStabilityWave(t, process, token, 0, 1, 1)` 执行一个不计入正式负载的暖机 workflow，
   并把该结果的资源快照作为 baseline；
3. 执行 wave 1 和 wave 2，分别检查资源；wave 2 还检查相对 wave 1 的增长；
4. 停止第一进程，以相同 runtime home 启动第二进程；
5. 30 秒内逐个 GET 前 100 个 ID，全部仍为 succeeded；
6. 第二进程把 HTTP client timeout 设为 2 秒，通过 wave 4/count 1/concurrency 1 执行独立
   暖机 workflow、采集 baseline，再执行正式 wave 3；
7. Provider 请求总数等于两个暖机 workflow 加 150 个正式 workflow；每个请求都使用合成 Authorization；
8. 停止第二进程，通过 SQLite read-only connection 读取 150 个 workflow，断言 succeeded、lease owner 为空、lease until 为零；
9. 扫描两个进程的完整输出，不得包含合成 API key 或 OIDC bearer token；精确清理测试 Keychain pair。

最终核对必须使用 SQLite `mode=ro` 连接直接查询，不得调用会执行 migration 的 store opener，
也不得执行任何写语句。

持久化 helper 的完整签名和语义：

```go
func assertStabilityRuntime(t *testing.T, runtimeHome string, ids []string)
```

它以 SQLite `mode=ro` 打开 `$HOST_RUNTIME_HOME/workflow.db`，逐行 `QueryRowContext`，断言
status 为 succeeded、lease owner 为空、lease until 为零，然后关闭 read-only connection。
`ids` 只传正式 wave 1-3 的 150 个 ID，不包含 wave 0/4 暖机。

- [ ] **Step 6: 运行稳定性门禁并确认 GREEN**

Run:

```bash
cd examples/host-api
go test -tags hostapisystemsmoke \
  -run '^(TestWaitForStabilityWorkflowsRejectsMatchAfterDeadline|TestOpenStabilityDatabaseReadOnly)$' \
  -count=1 ./...
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPStability$' \
  -count=1 ./...
```

Expected: PASS；三轮各 50 个 workflow；无 SKIP；日志打印每轮 p50/p95、收敛耗时、RSS、FD；
Provider 请求数为 152；worker/heartbeat errors 为 0；所有 lease 清空。

连续再运行一次同一命令。Expected: 再次 PASS，证明阈值不是偶然命中。

- [ ] **Step 7: 运行现有 MVP 黑盒回归**

Run:

```bash
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPBlackBoxClosure$' \
  -count=1 ./...
```

Expected: 三个既有子场景全部 PASS，无 SKIP。

- [ ] **Step 8: 提交稳定性门禁**

```bash
git add examples/host-api/host_mvp_stability_smoke_test.go
git commit -m "test(host-api): 增加单机稳定性门禁"
```

### Task 3: 发布稳定性命令并完成第 4 阶段验收

**Files:**
- Modify: `README.md`
- Modify: `examples/host-api/README.md`
- Modify: `docs/superpowers/specs/2026-07-14-mvp-validation-design.md`
- Modify: `docs/superpowers/specs/2026-07-15-host-api-mvp-stability-design.md`

**Interfaces:**
- Documents: 单槽 worker 生命周期、3×50 命令、目标机器和非生产 SLO 边界
- Produces: 第 4 阶段脱敏验收记录

- [ ] **Step 1: 更新公开文档**

在 Host README 的 Local MVP acceptance 后增加 `Single-host stability gate`：

```markdown
## Single-host stability gate

The stability gate targets an Apple M1 Pro with 10 CPU cores, 16 GiB RAM,
and macOS 26.5.1. It runs three waves of 50 queued workflows with 10 concurrent
HTTP submitters, performs OIDC final approval, restarts the real Host process,
and checks SQLite convergence plus RSS/file-descriptor bounds:

```bash
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPStability$' \
  -count=1 ./...
```

This is a correctness and leak-regression gate for the current single-process,
single-slot worker. It is not a production throughput SLO. A `SKIP` means the
required macOS, Keychain, `ps`, or `lsof` environment is unavailable and is not
a passing acceptance result.
```

根 README 只增加该章节链接和命令，不复制全部阈值。文档必须说明：Host 使用一个执行槽，
`StartQueuedWorker` 每个 Server 生命周期只启动一次；未启动 worker 时 queued workflow 保持
pending。

- [ ] **Step 2: 更新设计状态和 MVP 验收设计**

把本阶段设计状态改为 `已实现并验收`，追加实际目标 commit、两次 stability 输出摘要和资源
观测值。在 `2026-07-14-mvp-validation-design.md` 的并发与稳定性章节记录已经确认的目标机器、
3×50/10 路负载、阈值和命令；不得把这些值表述为生产容量。

- [ ] **Step 3: 运行格式、默认和带标签静态检查**

Run:

```bash
gofmt -w examples/host-api/server.go \
  examples/host-api/server_test.go \
  examples/host-api/host_mvp_stability_smoke_test.go
git diff --check
cd examples/host-api
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go vet -tags hostapisystemsmoke ./...
```

Expected: 全部退出 0；无格式错误、race 或 vet 问题。

- [ ] **Step 4: 再次执行两个真实进程门禁**

Run:

```bash
go test -v -tags hostapisystemsmoke \
  -run '^(TestHostAPIProcessMVPBlackBoxClosure|TestHostAPIProcessMVPStability|TestWaitForStabilityWorkflowsRejectsMatchAfterDeadline|TestOpenStabilityDatabaseReadOnly)$' \
  -count=1 ./...
```

Expected: 两条快速负向回归、黑盒三个子场景和稳定性三轮全部 PASS，无 SKIP。

- [ ] **Step 5: 运行完整 workspace 验证**

Run from repository root:

```bash
bash ./scripts/verify-all.sh
git diff --check
git status --short --branch
```

Expected: `goagents workspace verification passed`；diff check 无输出；只有计划内文档尚未提交。

- [ ] **Step 6: 提交文档和验收记录**

```bash
git add README.md examples/host-api/README.md \
  docs/superpowers/specs/2026-07-14-mvp-validation-design.md \
  docs/superpowers/specs/2026-07-15-host-api-mvp-stability-design.md
git commit -m "docs(mvp): 记录单机稳定性验收"
```

- [ ] **Step 7: 提交后最终验证**

Run:

```bash
git diff --check main..HEAD
git status --short --branch
git log --oneline --decorate -6
```

Expected: 分支干净；最近提交按设计、worker 修复、稳定性门禁和验收文档分离。
