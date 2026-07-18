# Host 生命周期硬化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增独立 `hostkit/v0.1.0` 生命周期模块，并把 `examples/host-api` 从 `panic`/直接 `ListenAndServe` 收正为可分类启动失败、可优雅 drain、可限时强制收口、可稳定重启恢复的 Host。

**Architecture:** `hostkit` 只用标准库协调单个 `Service` 的 `Start -> Drain/ForceStop -> Close` 状态机；Host API adapter 持有 intake/execution 两棵 context、HTTP listener、worker/janitor 完成信号、execution registry 和 store 关闭权。业务状态收口留在 Host，按活动操作类型用现有 workflow、checkpoint、AgentRun store 契约做条件式更新。

**Tech Stack:** Go 1.26.1、`net/http`、`os/signal`、SQLite store、现有 `workflowkit`/`runkit`/agent approval adapter、Go build-tag 真实进程测试、Bash 发布门禁。

## Global Constraints

- 实现必须符合已批准设计 `docs/superpowers/specs/2026-07-17-host-lifecycle-hardening-design.md`，不得顺带实现 Skill 动态激活、多 Worker、分布式 lease、GitHub Actions 升级或许可证 SHA-256。
- `hostkit` 的 module path 固定为 `github.com/eruca/goagents/hostkit`，首次版本固定为 `v0.1.0`；产品里程碑仍称 Host 生命周期 `v0.1.1`。
- `hostkit` 只能导入 Go 标准库，不得导入任何 GoAgents workspace module。
- 第一信号停止 HTTP 接单、queued claim 和 janitor 新扫描；当前 execution 使用独立 context 继续运行。
- drain timeout 默认 `30s`，由 `HOST_API_SHUTDOWN_TIMEOUT` 配置且必须大于零；ForceStop 与 Close 共用固定 `5s` cleanup budget。
- 第二信号只跳过剩余 drain，不跳过 ForceStop/Close。
- 强制收口固定写入 `host_shutdown_timeout`，不得自动 requeue、自动重放工具或创建新 checkpoint。
- 终态 workflow 和已完整持久化的稳定 `waiting_approval` 不得被 shutdown cleanup 覆盖。
- 非零退出只允许向 stderr 写一条固定 schema JSON；不得输出 panic stack、token、密钥、checkpoint、Prompt、模型响应或原始 Provider payload。
- 所有并发测试使用 channel/barrier 驱动；不得用任意 `time.Sleep` 让时序碰巧成立。
- 不修改历史文档中“v0.1.0 发布 12 个模块”的历史事实。
- 每个任务先看到目标测试失败，再写最小实现；每个任务结束运行列出的局部门禁并按语义提交中文 commit。

---

### Task 1: 固化 `hostkit` 错误分类、退出码与 JSON 契约

**Files:**
- Create: `hostkit/go.mod`
- Create: `hostkit/result.go`
- Create: `hostkit/result_test.go`

**Interfaces:**
- Consumes: Host 提供的安全错误 message 和原始 cause。
- Produces: `Code`、`Fail`、`Result`、`WriteError`，以及 code 到退出码的唯一映射。

- [ ] **Step 1:** 创建只声明 module path 和 Go 版本的 `hostkit/go.mod`：

```go
module github.com/eruca/goagents/hostkit

go 1.26.1
```

- [ ] **Step 2:** 在 `hostkit/result_test.go` 使用 `package hostkit` 写表驱动失败测试，以便验证包内映射而不扩大公开 API；精确覆盖：

```go
var exitCases = []struct {
    code Code
    exit int
}{
    {CodeInternalError, 1},
    {CodeConfigFailed, 2},
    {CodeInitializationFailed, 2},
    {CodeListenFailed, 3},
    {CodeServeFailed, 4},
    {CodeShutdownTimeout, 5},
    {CodeShutdownCleanupTimeout, 5},
}
```

测试还必须证明：

- `Fail(code, safeMessage, cause)` 可被 `errors.Is` 追溯到 cause；
- 未分类 error 和未知 `Code` 都归一为 `internal_error/1`，未分类 cause 不得进入 JSON，
  但 `errors.Is(result.Err(), cause)` 仍成立；
- `Result` 只有私有 fields 和 `ExitCode()`、`Code()`、`Err()` 只读方法，调用方不能自定义
  exit code、code 或注入 raw cause；
- `WriteError` 对 canonical `Result` 的 `ExitCode() == 0` 不写任何字节；
- 非零结果只写一行，例如 `{"level":"error","event":"host_exit","code":"listen_failed","message":"safe listen failure"}\n`；
- message 中的 JSON 特殊字符被标准 encoder 正确转义，不产生第二行或嵌套 error。

- [ ] **Step 3:** 运行失败测试并确认失败原因是 package/API 尚不存在：

```bash
(cd hostkit && go test ./... -run 'Test(CodeExitMapping|Failure|WriteError)' -count=1)
```

预期：退出非零，编译错误只涉及尚未实现的 `Code`、`Fail`、`Result` 或 `WriteError`。

- [ ] **Step 4:** 在 `hostkit/result.go` 写最小公开契约：

```go
type Code string

const (
    CodeInternalError           Code = "internal_error"
    CodeConfigFailed            Code = "config_failed"
    CodeInitializationFailed    Code = "initialization_failed"
    CodeListenFailed            Code = "listen_failed"
    CodeServeFailed             Code = "serve_failed"
    CodeShutdownTimeout         Code = "shutdown_timeout"
    CodeShutdownCleanupTimeout  Code = "shutdown_cleanup_timeout"
)

type Result struct {
    // fields are private
}

func (Result) ExitCode() int
func (Result) Code() string
func (Result) Err() error

func Fail(code Code, safeMessage string, cause error) error
func WriteError(w io.Writer, result Result) error
```

`Fail` 返回的具体类型保持包内私有，`Error()` 只返回 `safeMessage`，`Unwrap()` 返回 cause。
`Result` fields 保持私有；`resultFromError(error) Result` 和后续 `Run` 是非零 Result 的唯一构造
路径，零值 Result 仅表示成功。增加包内穷尽的 `exitCode(Code) int`；分类必须用 `errors.As`，
禁止解析错误字符串。未知 Code 与未分类 error 都构造 canonical `internal_error/1`，其中未分类
error 使用固定安全 message 保留 cause；`WriteError` 只读取该 canonical Result。

- [ ] **Step 5:** 运行：

```bash
(cd hostkit && go test ./... -run 'Test(CodeExitMapping|Failure|WriteError)' -count=1)
(cd hostkit && go vet ./...)
```

预期全部退出 0。

- [ ] **Step 6:** 提交：

```bash
git add hostkit/go.mod hostkit/result.go hostkit/result_test.go
git commit -m "feat(hostkit): 固化生命周期退出契约"
```

---

### Task 2: 用确定性状态机实现 `hostkit.Run`

**Files:**
- Create: `hostkit/lifecycle.go`
- Create: `hostkit/lifecycle_test.go`
- Modify: `hostkit/result.go`

**Interfaces:**
- Consumes: 一个 `Service`、一个 interrupt channel 和 drain/cleanup timeout。
- Produces: 同步 `Run` 结果；所有 lifecycle hook 最多调用一次。

- [ ] **Step 1:** 在 `hostkit/lifecycle_test.go` 创建由 barrier 控制的 fake service，记录调用顺序和调用次数：

```go
type fakeService struct {
    startErr error
    done     chan error

    drainStarted chan struct{}
    allowDrain   chan struct{}
    forceStarted chan struct{}
    allowForce   chan struct{}
    closeStarted chan struct{}
    allowClose   chan struct{}

    mu    sync.Mutex
    calls []string
}
```

测试不得依赖固定 sleep；等待动作使用 `<-started`，释放动作使用 `close(allowX)`，仅用 context deadline 防止测试永久挂起。

- [ ] **Step 2:** 先写以下失败测试：

```text
TestRunDrainSuccess
TestRunDrainTimeoutForcesAndCloses
TestRunSecondInterruptForcesWithoutWaitingForDrainTimeout
TestRunForceStopAndCloseShareCleanupBudget
TestRunCleanupTimeoutWinsOverServeFailure
TestRunStartFailureStillCloses
TestRunUnexpectedDoneErrorForcesThenPreservesServeFailure
TestRunUnexpectedNilDoneIsInternalError
TestRunLifecycleMethodsAreCalledAtMostOnce
TestRunRejectsNonPositiveTimeoutsWithoutStarting
```

每个测试同时断言精确调用序列：

```text
正常：start, drain, close
强制：start, drain, force_stop, close
启动失败：start, close
运行态异常：start, force_stop, close
```

- [ ] **Step 3:** 运行并确认因 `Service`、`Options`、`Run` 尚不存在而失败：

```bash
(cd hostkit && go test ./... -run '^TestRun' -count=1)
```

- [ ] **Step 4:** 在 `hostkit/lifecycle.go` 增加已批准公开接口：

```go
type Service interface {
    Start(context.Context) error
    Done() <-chan error
    Drain(context.Context) error
    ForceStop(context.Context) error
    Close(context.Context) error
}

type Options struct {
    DrainTimeout   time.Duration
    CleanupTimeout time.Duration
}

func Run(
    ctx context.Context,
    service Service,
    interrupts <-chan struct{},
    options Options,
) Result
```

实现约束：

- `Drain` 必须在 goroutine 中执行，使主状态机能同时观察第二信号和 drain deadline；
- `ctx.Done()` 与第一次 interrupt 使用同一 drain 路径，不赋予额外退出 code；
- `DrainTimeout <= 0` 或 `CleanupTimeout <= 0` 在调用 Start 前返回 `internal_error/1`；
- drain 超时或第二信号先取消 drain context，再进入 force；
- force 路径只创建一个 cleanup deadline，并把同一个剩余 deadline context 依次传给 `ForceStop`、`Close`；
- 若 ForceStop 未返回，不得并发调用 Close；
- cleanup deadline 先到时直接返回 `shutdown_cleanup_timeout/5`，不再并发触碰 service；
- Start 已被调用后，无论其成功或失败，最多调用一次 Close；
- Start/Done/Drain/ForceStop 返回的 `Fail` 保留分类；未分类错误转 `internal_error`；
- drain timeout 或第二信号在 cleanup 成功时固定返回 `shutdown_timeout/5`；
- 运行中 `Done` 的非预期 `nil` 归类为 `internal_error`。
- draining 中收到的 `Done(nil)` 是预期 serve 终止信号，最终结果仍由 Drain 决定；draining 中的非 nil Done error 要保留并在 cleanup 后按其分类返回。

- [ ] **Step 5:** 增加 race-oriented 重复测试，循环驱动“Drain 返回”和“第二 interrupt”竞争，断言没有重复 ForceStop/Close、没有 channel double close：

```go
func TestRunDrainAndSecondInterruptRace(t *testing.T) {
    for range 200 {
        // 每轮使用全新 fake 和 barrier；并发释放 drain/发送第二 interrupt。
    }
}
```

- [ ] **Step 6:** 运行：

```bash
(cd hostkit && go test ./... -count=1)
(cd hostkit && go test -race ./... -count=1)
```

预期全部退出 0。

- [ ] **Step 7:** 提交：

```bash
git add hostkit/lifecycle.go hostkit/lifecycle_test.go hostkit/result.go
git commit -m "feat(hostkit): 实现可强制收口的生命周期状态机"
```

---

### Task 3: 建立 Host execution registry 与条件式 workflow cleanup

**Files:**
- Create: `examples/host-api/execution_registry.go`
- Create: `examples/host-api/execution_registry_test.go`
- Create: `examples/host-api/lifecycle_cleanup.go`
- Create: `examples/host-api/lifecycle_cleanup_test.go`
- Modify: `examples/host-api/server.go`
- Modify: `workflowkit/sqlitestore/store.go`
- Modify: `workflowkit/sqlitestore/store_test.go`

**Interfaces:**
- Consumes: workflow ID、operation kind、当前 workflow store 状态。
- Produces: drain 后拒绝新长操作、活动快照、可等待完成信号和幂等 shutdown cleanup。

- [ ] **Step 1:** 在 `execution_registry_test.go` 先写失败测试：

```text
TestExecutionRegistryBeginAndWait
TestExecutionRegistryRejectsBeginAfterDrain
TestExecutionRegistrySnapshotSurvivesConcurrentDone
TestExecutionHandleDoneIsIdempotent
TestExecutionRegistryBeginDrainAndBeginRace
```

race 测试必须证明不存在 `sync.WaitGroup.Add` 与 `Wait` 竞态；registry 使用“活动数从 0 变 1 时创建新 idle channel、回到 0 时关闭”的模型。

- [ ] **Step 2:** 在 `execution_registry.go` 实现 Host 私有最小类型：

```go
type executionKind string

const (
    executionSyncWorkflow   executionKind = "sync_workflow"
    executionQueuedWorkflow executionKind = "queued_workflow"
    executionFinalApproval  executionKind = "final_approval"
    executionAgentApproval  executionKind = "agent_approval"
)

type executionCleanup func(context.Context) error

type executionSnapshot struct {
    workflowID string
    kind       executionKind
    done       <-chan struct{}
    cleanup    executionCleanup
}

type executionRegistry struct {
    mu        sync.Mutex
    accepting bool
    nextID    uint64
    active    map[uint64]*executionEntry
    idle      chan struct{}
}

func newExecutionRegistry() *executionRegistry
func (r *executionRegistry) Begin(workflowID string, kind executionKind, cleanup executionCleanup) (*executionHandle, bool)
func (r *executionRegistry) BeginDrain()
func (r *executionRegistry) Snapshot() []executionSnapshot
func (r *executionRegistry) Wait(context.Context) error
```

`executionHandle.Done()` 用 `sync.Once` 移除 entry 并关闭其 done。不要增加 participant graph 或通用 callback registry。

- [ ] **Step 3:** 运行 registry 局部测试：

```bash
(cd examples/host-api && go test ./... -run '^TestExecutionRegistry' -count=1)
(cd examples/host-api && go test -race ./... -run '^TestExecutionRegistry' -count=1)
```

预期全部退出 0。

- [ ] **Step 4:** 先为 SQLite Store.Update 写真实并发 RED，再做最小事务化实现：

```text
TestStoreUpdateSerializesConcurrentSave
TestStoreUpdateRollsBackCallbackError
TestStoreUpdatePropagatesContextCancellation
```

并发测试使用 callback barrier 和带 deadline 的并发 Save，不使用 sleep。旧实现的 RED
必须证明同一 Store 的 Save 可以在 callback 阻塞时成功，随后被 Update 的旧快照覆盖。
实现时保持 `workflowkit.Store.Update` 公共 contract 不变，提取 tx-compatible 私有
get/save helper，并在同一事务中完成 read → mutate → write → readback → commit。
callback error 必须 rollback；context cancellation 和 commit error 原样传播。

该原子性依赖 Host 持有同一个 Store 实例以及当前 `SetMaxOpenConns(1)`；不得宣称覆盖多个
独立 Store 实例。

运行：

```bash
(cd workflowkit && go test ./sqlitestore -run 'Test.*Update' -count=1)
(cd workflowkit && go test -race ./sqlitestore -count=1)
```

- [ ] **Step 5:** 在 `lifecycle_cleanup_test.go` 先写 workflow 收口失败测试：

```text
TestFinalizeWorkflowShutdownFailsActiveWorkflow
TestFinalizeWorkflowShutdownClearsQueuedLease
TestFinalizeWorkflowShutdownPreservesTerminalWorkflow
TestFinalizeWorkflowShutdownPreservesStableWaitingApproval
TestFinalizeWorkflowShutdownPreservesHistoryAndReferences
TestFinalizeWorkflowShutdownIsIdempotent
```

使用真实现有 workflow store contract 创建状态，不得直接用测试 SQL 篡改记录。断言 active `pending/running` 变为：

```go
run.Status = workflowkit.StatusFailed
run.Error = "host_shutdown_timeout"
run.LeaseOwner = ""
run.LeaseUntil = time.Time{}
```

同时断言 step history、`InputRef`、`OutputRef`、`AgentRunID`、`AuditRef`、
`CurrentStep`、`StepAttempts` 和 `Metadata` 原样保留。terminal 与稳定
`waiting_approval` 必须补真实 SQLite Store 证据。

- [ ] **Step 6:** 在 `lifecycle_cleanup.go` 实现：

```go
const hostShutdownTimeoutCode = "host_shutdown_timeout"

func (s *Server) finalizeWorkflowShutdown(ctx context.Context, workflowID string) error
func waitAndCleanupExecutions(ctx context.Context, snapshots []executionSnapshot) error
```

`finalizeWorkflowShutdown` 必须重新读取持久化状态并通过原子
`workflowkit.Store.Update` 做条件式更新；终态与稳定 approval wait 返回 nil。
若 outer Get 后状态已并发稳定，Update callback 必须返回 Host 私有 unchanged sentinel
触发事务 rollback，外层将 sentinel 归一为 nil；禁止 no-op upsert 或刷新 `UpdatedAt`。
`waitAndCleanupExecutions` 必须先等对应 operation 的 done，再以同一个 context 调用其
cleanup；context 到期立即返回且不关闭 store，cleanup error 原样传播。

- [ ] **Step 7:** 在 `Server` 增加由 `NewServer` 初始化的 registry：

```go
executions *executionRegistry
```

该字段加入现有 `Server`，不复制或重排其他字段。

仅使用 `NewServer` 初始化；现有只构造 `hostAgentStep` 的窄单元测试不应被迫创建完整 lifecycle。

- [ ] **Step 8:** 运行：

```bash
(cd workflowkit && go test ./sqlitestore -run 'Test.*Update' -count=1)
(cd workflowkit && go test -race ./sqlitestore -count=1)
(cd examples/host-api && go test ./... -run 'Test(ExecutionRegistry|FinalizeWorkflowShutdown|WaitAndCleanup)' -count=1)
(cd examples/host-api && go test -race ./... -run 'Test(ExecutionRegistry|FinalizeWorkflowShutdown|WaitAndCleanup)' -count=1)
(cd workflowkit && go test ./... -count=1)
(cd examples/host-api && go test ./... -count=1)
(cd workflowkit && go vet ./...)
(cd examples/host-api && go vet ./...)
git diff --check
```

- [ ] **Step 9:** 提交 Task 3 原子性 follow-up：

```bash
git add workflowkit/sqlitestore/store.go workflowkit/sqlitestore/store_test.go \
  examples/host-api/execution_registry_test.go examples/host-api/lifecycle_cleanup_test.go \
  docs/superpowers/specs/2026-07-17-host-lifecycle-hardening-design.md \
  docs/superpowers/plans/2026-07-17-host-lifecycle-hardening.md
git commit -m "fix(workflowkit): 事务化工作流条件更新"
```

---

### Task 4: 分离 worker/janitor intake 与 execution 生命周期

**Files:**
- Modify: `examples/host-api/server.go`
- Modify: `examples/host-api/server_test.go`
- Modify: `examples/host-api/agent_approval_expiry.go`
- Modify: `examples/host-api/agent_approval_expiry_test.go`

**Interfaces:**
- Consumes: `intakeCtx` 控制 claim/扫描，`executionCtx` 控制已领取 workflow。
- Produces: worker/janitor 可等待 done；drain 后 worker 不 claim 第二项，当前项可自然完成。

- [ ] **Step 1:** 在 `server_test.go` 用现有 `singleSlotStep` barrier 增加失败测试：

```text
TestQueuedWorkerDrainFinishesCurrentWorkflowWithoutClaimingNext
TestQueuedWorkerForceContextCancelsCurrentWorkflow
TestQueuedWorkerWaitReturnsAfterIntakeStops
```

场景固定为两个 pending workflow：第一项进入 step 后取消 intake，释放第一项并等待 worker 退出；断言第一项成功、第二项仍为 pending 且从未进入 step。

- [ ] **Step 2:** 保留现有兼容入口，新增双 context 入口：

```go
func (s *Server) StartQueuedWorker(ctx context.Context) {
    s.StartQueuedWorkerWithContexts(ctx, ctx)
}

func (s *Server) StartQueuedWorkerWithContexts(
    intakeCtx context.Context,
    executionCtx context.Context,
)

func (s *Server) WaitQueuedWorker(context.Context) error
```

`runQueuedWorkerLoop` 只用 intake context 等 wake/ticker 和执行 claim；claim 成功后的 executor 与 heartbeat 使用 execution context。若 intake 在 claim 返回后、登记 execution 前已进入 drain，必须释放刚取得的 lease 并退出，不执行该 workflow。

- [ ] **Step 3:** queued workflow claim 后立即登记 `executionQueuedWorkflow`；cleanup 绑定 `finalizeWorkflowShutdown(workflowID)`，operation 返回时 defer `handle.Done()`。正常 lease release 不再用无限制 `context.Background()`，而使用短、可控的独立 cleanup context并处理错误。

- [ ] **Step 4:** 在 `agent_approval_expiry_test.go` 先写：

```text
TestAgentApprovalJanitorWaitsForCurrentScan
TestAgentApprovalJanitorDoesNotStartScanAfterIntakeCancellation
TestAgentApprovalJanitorStartsOnlyOnce
```

- [ ] **Step 5:** 给 janitor 增加 once/done 与等待 API：

```go
func (s *Server) StartAgentApprovalJanitor(ctx context.Context)
func (s *Server) WaitAgentApprovalJanitor(context.Context) error
```

保持原入口签名；每次 reconciliation 使用 intake context，取消后不再开始新扫描。`NewServer` 初始化 done channel，启动失败/禁用场景也必须有明确可等待语义。

- [ ] **Step 6:** 运行：

```bash
(cd examples/host-api && go test ./... -run 'Test(QueuedWorkerDrain|QueuedWorkerForce|QueuedWorkerWait|AgentApprovalJanitor)' -count=1)
(cd examples/host-api && go test -race ./... -run 'Test(QueuedWorkerDrain|QueuedWorkerForce|QueuedWorkerWait|AgentApprovalJanitor)' -count=1)
```

预期全部退出 0，且既有 queued worker/approval expiry 测试继续通过。

- [ ] **Step 7:** 提交：

```bash
git add examples/host-api/server.go examples/host-api/server_test.go \
  examples/host-api/agent_approval_expiry.go examples/host-api/agent_approval_expiry_test.go
git commit -m "feat(host-api): 分离接单与执行生命周期"
```

---

### Task 5: 登记 HTTP workflow，并收口 agent approval lease/AgentRun

**Files:**
- Modify: `examples/host-api/server.go`
- Modify: `examples/host-api/server_test.go`
- Modify: `examples/host-api/approval_handler_test.go`
- Modify: `examples/host-api/agent_approval.go`
- Modify: `examples/host-api/agent_approval_test.go`
- Modify: `examples/host-api/lifecycle_cleanup.go`
- Modify: `examples/host-api/lifecycle_cleanup_test.go`
- Modify: `examples/host-api/openapi.yaml`

**Interfaces:**
- Consumes: 活动 HTTP operation、approval checkpoint、显式 lease owner。
- Produces: drain 后长操作返回 `503 host_draining`；force 后 checkpoint/AgentRun/workflow 一致失败。

- [ ] **Step 1:** 在 handler 测试先覆盖登记边界：

```text
TestCreateSyncWorkflowRejectedWhileDraining
TestApproveFinalWorkflowRejectedWhileDraining
TestApproveAgentToolRejectedWhileDraining
TestCreateQueuedWorkflowRemainsShortHandlerTransaction
TestRequeueWorkflowRemainsShortHandlerTransaction
```

只对 sync workflow、final approval continuation、agent approval/resume 登记；纯查询、queued create、requeue 不登记。被拒绝的长操作返回：

```json
{"error":{"code":"host_draining","message":"host is draining"}}
```

- [ ] **Step 2:** 修改 handlers：

- `handleCreateWorkflow` 的 sync 分支在调用 executor 前登记 `executionSyncWorkflow`；
- `handleApproveWorkflow` 在 continuation 前登记 `executionFinalApproval`；
- `handleApproveAgentTool` 只在允许并准备 resume 的分支登记 `executionAgentApproval`；
- 每项普通 workflow cleanup 绑定 `finalizeWorkflowShutdown`；
- handler 的 defer 必须在持久化最终结果后才 `Done()`。

同时在 `openapi.yaml` 记录 `503 host_draining`，不改变现有 request/response schema。

- [ ] **Step 3:** 把 approval lease owner 从 service 内部生成改为 handler 显式创建并传入：

```go
leaseOwner := "host-api:" + agentcore.NewRunID().String()

next, result, err := s.agentApprovals.ApproveAndResume(
    r.Context(),
    workflowID,
    *approval,
    identity.Subject,
    resolutions,
    leaseOwner,
)
```

`hostAgentApprovalService.ApproveAndResume` 必须把该值原样交给现有 adapter，不得在内部生成第二个 owner。execution cleanup closure 捕获同一个 `leaseOwner`。

- [ ] **Step 4:** 在 `lifecycle_cleanup_test.go` 用真实 run/checkpoint store contract 写失败测试：

```text
TestFinalizeAgentApprovalShutdownBeforeLeaseIsNoOp
TestFinalizeAgentApprovalShutdownFailsOwnedLeaseAndRunningAgentRun
TestFinalizeAgentApprovalShutdownLeavesCompetingLeaseUntouched
TestFinalizeAgentApprovalShutdownKeepsConsumedCheckpointConsumed
TestFinalizeAgentApprovalShutdownPreservesCompletedResume
TestFinalizeAgentApprovalShutdownIsIdempotent
```

精确断言：

- 未取得 lease 或 lease owner 不匹配时不改 checkpoint/AgentRun；
- 自有 leased checkpoint 调用以下现有 contract 后 lease 清空：

```go
checkpoints.FailLease(ctx, runkit.CheckpointLeaseCompletion{
    CheckpointID: approval.CheckpointID,
    TenantID:     checkpoint.TenantID,
    LeaseOwner:   leaseOwner,
    FailureCode:  hostShutdownTimeoutCode,
    Now:          time.Now(),
})
```
- 仅 `runkit.StatusRunning` 的 AgentRun 完成到 failed；
- consumed checkpoint 保持 consumed，不回退 pending、不重放工具；
- workflow 仍与该 pending approval 匹配且未稳定完成时才失败；
- 已完整持久化到 final approval wait 的 workflow 保持不变。

- [ ] **Step 5:** 在 `agent_approval.go`/`lifecycle_cleanup.go` 实现专属 cleanup：

```go
func (s *Server) finalizeAgentApprovalShutdown(
    ctx context.Context,
    workflowID string,
    approval agentApprovalResponse,
    leaseOwner string,
) error
```

实现只组合既有 `GetCheckpoint`、`FailLease`、run store `Get/Complete` 和 workflow store `Update`。不得新增直接 SQL、不得调用工具、不得创建 checkpoint。对 adapter 在 cancellation 后使用已取消 request context 导致的 lease 清理失败，由这里用 lifecycle cleanup context 修复。

- [ ] **Step 6:** 运行：

```bash
(cd examples/host-api && go test ./... -run 'Test(CreateSyncWorkflowRejected|ApproveFinalWorkflowRejected|ApproveAgentToolRejected|CreateQueuedWorkflowRemains|RequeueWorkflowRemains|FinalizeAgentApprovalShutdown)' -count=1)
(cd examples/host-api && go test -race ./... -run 'Test(FinalizeAgentApprovalShutdown|ApproveAgentToolRejected)' -count=1)
```

- [ ] **Step 7:** 提交：

```bash
git add examples/host-api/server.go examples/host-api/server_test.go \
  examples/host-api/approval_handler_test.go examples/host-api/agent_approval.go \
  examples/host-api/agent_approval_test.go examples/host-api/lifecycle_cleanup.go \
  examples/host-api/lifecycle_cleanup_test.go examples/host-api/openapi.yaml
git commit -m "feat(host-api): 收口审批恢复与活动HTTP执行"
```

---

### Task 6: 实现 `hostAPIService` adapter

**Files:**
- Create: `examples/host-api/lifecycle.go`
- Create: `examples/host-api/lifecycle_test.go`
- Modify: `examples/host-api/server.go`

**Interfaces:**
- Consumes: `Server`、监听地址、stdout、worker/janitor 配置。
- Produces: 满足 `hostkit.Service` 的 Host adapter，拥有 listener、双 context、HTTP server、registry 和 stores。

- [ ] **Step 1:** 在 `lifecycle_test.go` 先写 in-process 失败测试：

```text
TestHostAPIServiceStartBindsBeforeStartingBackgroundComponents
TestHostAPIServiceReportsUnexpectedServeFailure
TestHostAPIServiceDrainStopsIntakeAndWaitsExecutions
TestHostAPIServiceForceStopCancelsExecutionsAndRunsCleanup
TestHostAPIServiceCloseClosesStoresInReverseOpenOrderOnce
TestHostAPIServiceCleanupTimeoutDoesNotCloseStoresUnderActiveExecution
TestHostAPIServicePartialStartFailureCanClose
TestHostAPIServiceAllOwnedGoroutinesExit
```

使用 loopback `127.0.0.1:0`、受控 fake closer/barrier 和真实 `http.Server`。监听失败测试必须断言 worker/janitor 从未启动。
`TestHostAPIServiceForceStopCancelsExecutionsAndRunsCleanup` 必须以表驱动方式分别进入 sync workflow、queued workflow、final approval 和 agent approval 四类 execution；`TestHostAPIServiceAllOwnedGoroutinesExit` 通过 worker/janitor/serve/registry done channel 证明退出，不比较易波动的全局 goroutine 数量。

- [ ] **Step 2:** 在 `lifecycle.go` 创建：

```go
const hostCleanupTimeout = 5 * time.Second

type hostAPIService struct {
    server *Server
    addr   string
    stdout io.Writer

    intakeCtx       context.Context
    cancelIntake    context.CancelFunc
    executionCtx    context.Context
    cancelExecution context.CancelFunc

    listener   net.Listener
    httpServer *http.Server
    done       chan error // 构造时初始化为 make(chan error, 1)。

    drainOnce sync.Once
    forceOnce sync.Once
    closeOnce sync.Once
}

func newHostAPIService(server *Server, addr string, stdout io.Writer) *hostAPIService
```

- [ ] **Step 3:** 实现 `Start`：

1. 先 `net.Listen("tcp", addr)`，失败返回 `hostkit.Fail(CodeListenFailed, safeMessage, err)`；
2. 构建 HTTP server，BaseContext 固定返回 `executionCtx`：

```go
httpServer := &http.Server{
    Handler: server.Handler(),
    BaseContext: func(net.Listener) context.Context {
        return service.executionCtx
    },
}
```

3. listener 成功后启动 worker/janitor；
4. 单独 goroutine 调用 `Serve`，将预期 `http.ErrServerClosed` 归一为 nil，其他错误包装为 `serve_failed` 并只向稳定 `done` channel 发送一次；
5. 绑定成功后才向 stdout 写 `host_api_addr=<actual address>\n`。

- [ ] **Step 4:** 实现 `Drain`：

```text
registry.BeginDrain
cancelIntake
并发执行 httpServer.Shutdown、WaitQueuedWorker、WaitAgentApprovalJanitor、registry.Wait
任一返回真实错误则返回该错误
全部完成后返回 nil
```

这里不得取消 `executionCtx`。`Drain` 不关闭 stores，资源释放统一留给 `Close`。

- [ ] **Step 5:** 实现 `ForceStop`：

```text
registry.BeginDrain
snapshot := registry.Snapshot()
cancelIntake
cancelExecution
httpServer.Close
waitAndCleanupExecutions(snapshot)
WaitQueuedWorker
WaitAgentApprovalJanitor
```

所有等待/cleanup 使用传入 context。任何一步到期都返回 context error，让 `hostkit` 报 `shutdown_cleanup_timeout`；不得在此超时后另起 goroutine继续碰 store。

- [ ] **Step 6:** 给 `Server` 增加幂等、context-aware 关闭方法：

```go
func (s *Server) Close(ctx context.Context) error
```

关闭顺序固定为 workflow store 后 run store（打开顺序的逆序）。如 store 现有 `Close` 不接 context，在调用前后检查 context，且只在 execution registry 已 idle 时调用。多个错误用标准库 `errors.Join`，但每个 store 最多关闭一次。

- [ ] **Step 7:** 运行：

```bash
(cd examples/host-api && go test ./... -run '^TestHostAPIService' -count=1)
(cd examples/host-api && go test -race ./... -run '^TestHostAPIService' -count=1)
```

- [ ] **Step 8:** 提交：

```bash
git add examples/host-api/lifecycle.go examples/host-api/lifecycle_test.go examples/host-api/server.go
git commit -m "feat(host-api): 接入可排空与强制停止的Host服务"
```

---

### Task 7: 拆分配置/初始化，并让 CLI 使用稳定 signal 与退出契约

**Files:**
- Modify: `examples/host-api/main.go`
- Modify: `examples/host-api/main_test.go`
- Modify: `examples/host-api/go.mod`
- Modify: `examples/host-api/go.sum`

**Interfaces:**
- Consumes: Host 环境变量、Skill loader、OIDC/store 初始化、SIGINT/SIGTERM。
- Produces: `runHost` 稳定退出码；stderr 精确单行 JSON；无 panic。

- [ ] **Step 1:** 在 `main_test.go` 先写纯配置失败测试：

```text
TestLoadHostSettingsDefaultsShutdownTimeout
TestLoadHostSettingsParsesShutdownTimeout
TestLoadHostSettingsRejectsInvalidShutdownTimeout
TestLoadHostSettingsRejectsNonPositiveShutdownTimeout
TestLoadHostSettingsRejectsInvalidKeychainBeforeOIDC
TestInitializeHostConfigRejectsInvalidSkillRootBeforeOIDC
```

用记录调用次数的 OIDC loader 证明明显无效 Keychain、Skill root 与 shutdown 配置在 discovery 前失败。

- [ ] **Step 2:** 把当前 `loadHostConfig` 拆成：

```go
type hostSettings struct {
    addr            string
    shutdownTimeout time.Duration
    // 其余只来自环境、尚未执行外部 I/O 的字段。
}

func loadHostSettings(getenv func(string) string) (hostSettings, error)
func initializeHostConfig(
    ctx context.Context,
    settings hostSettings,
    getenv func(string) string,
    loadApprovalAuthenticator func(
        context.Context,
        func(string) string,
    ) (*OIDCApprovalAuthenticator, error),
) (Config, error)
```

`loadHostSettings` 缺省 `HOST_API_SHUTDOWN_TIMEOUT=30s`，解析失败或 `<=0` 返回普通配置错误。`initializeHostConfig` 先调用现有 `loadHostSkillConfig(getenv)`，再调用注入的 OIDC loader，因此 Skill root 失败仍早于 discovery；Skill catalog 读取、OIDC discovery、Provider composition 和 store 打开在 CLI 边界归类为 `initialization_failed`。保持既有 fail-closed 校验顺序。

- [ ] **Step 3:** 在 `main_test.go` 写 `runHost` 分类测试，依赖注入 listener/service factory，不启动真实网络：

```text
TestRunHostConfigFailureWritesOneJSONLineAndReturns2
TestRunHostInitializationFailureWritesOneJSONLineAndReturns2
TestRunHostCleanDrainWritesNoErrorAndReturns0
TestRunHostInternalFailureWritesOneJSONLineAndReturns1
```

错误输出精确解析为只有四个 key：`level`、`event`、`code`、`message`。

- [ ] **Step 4:** 实现 production 薄入口和可注入执行函数：

```go
func main() {
    os.Exit(runHost())
}

type hostDependencies struct {
    getenv                      func(string) string
    loadApprovalAuthenticator   func(context.Context, func(string) string) (*OIDCApprovalAuthenticator, error)
    newServer                   func(Config) (*Server, error)
    newService                  func(*Server, string, io.Writer) hostkit.Service
    stdout                      io.Writer
    stderr                      io.Writer
    interrupts                  <-chan struct{}
}

func runHost() int {
    interrupts, stopSignals := osSignalInterrupts()
    defer stopSignals()

    return runHostWithDeps(context.Background(), hostDependencies{
        getenv:                    os.Getenv,
        loadApprovalAuthenticator: loadOIDCApprovalAuthenticator,
        newServer:                 NewServer,
        newService: func(server *Server, addr string, stdout io.Writer) hostkit.Service {
            return newHostAPIService(server, addr, stdout)
        },
        stdout:     os.Stdout,
        stderr:     os.Stderr,
        interrupts: interrupts,
    })
}
```

同时实现 `func runHostWithDeps(ctx context.Context, deps hostDependencies) int`。`osSignalInterrupts` 返回 `(<-chan struct{}, func())`，使用 buffer 为 2 的内部 `interrupts` channel 和 buffer 为 2 的 `signals` channel，调用 `signal.Notify(signals, os.Interrupt, syscall.SIGTERM)`，只把信号转换为 `struct{}{}`；返回的 stop 函数调用 `signal.Stop(signals)`。`runHostWithDeps`：

1. 配置错误包装为 `hostkit.CodeConfigFailed`；
2. 初始化错误包装为 `hostkit.CodeInitializationFailed`；
3. 构建 `hostAPIService`；
4. 调用 `hostkit.Run`，options 为配置 drain timeout 和固定 `5s` cleanup；
5. 仅非零结果调用 `hostkit.WriteError(stderr, result)`；
6. 返回 `result.ExitCode()`。

错误 message 在 Host 边界生成安全摘要；不得把完整 Provider response、环境值或 checkpoint 注入 message。

- [ ] **Step 5:** 在 `examples/host-api/go.mod` 增加：

```go
require github.com/eruca/goagents/hostkit v0.1.0

replace github.com/eruca/goagents/hostkit => ../../hostkit
```

运行 `go mod tidy`；确认只产生必要的 `go.mod/go.sum` 变化。

- [ ] **Step 6:** 运行：

```bash
(cd examples/host-api && go test ./... -run 'Test(LoadHostSettings|RunHost)' -count=1)
(cd examples/host-api && go test ./... -count=1)
(cd examples/host-api && go vet ./...)
```

- [ ] **Step 7:** 提交：

```bash
git add examples/host-api/main.go examples/host-api/main_test.go \
  examples/host-api/go.mod examples/host-api/go.sum
git commit -m "feat(host-api): 输出稳定启动错误并处理系统信号"
```

---

### Task 8: 硬化真实进程测试框架并覆盖配置/监听失败

**Files:**
- Modify: `examples/host-api/host_process_smoke_test.go`
- Modify: `examples/host-api/host_process_smoke_cleanup_test.go`
- Create: `examples/host-api/host_lifecycle_process_smoke_test.go`

**Interfaces:**
- Consumes: 真实 Host binary、独立 stdout/stderr、OS signal。
- Produces: 可请求 signal、可断言精确退出码/日志且不会把强杀误报成功的测试 helper。

- [ ] **Step 1:** 先为 helper 写失败测试：

```text
TestHostProcessCapturesStdoutAndStderrSeparately
TestWaitHostExitAcceptsExpectedNonZeroExit
TestWaitHostExitFailsWhenCleanupKillWasRequired
TestHostExitErrorIsExactlyOneJSONLine
```

保留现有敏感值 redaction/扫描能力。

- [ ] **Step 2:** 将 `hostProcess` 输出拆分为：

```go
command *exec.Cmd
stdout  *lockedBuffer
stderr  *lockedBuffer
output  *lockedBuffer // 仅供兼容诊断，由 MultiWriter 汇总。
```

以上字段替换现有 `hostProcess` 的 command/output 部分，现有 URL、client 和 redaction 字段原样保留。

把原 `stopHostCommand` 拆为：

```go
func signalHostProcess(process *hostProcess, signal os.Signal) error
func waitHostProcess(process *hostProcess, timeout time.Duration) (exitCode int, err error)
func cleanupKilledHostProcess(t *testing.T, process *hostProcess)
```

`waitHostProcess` 应从 `ProcessState.ExitCode()` 返回真实 code；预期非零时 `exec.ExitError` 不是 helper 自身失败。超时后的 Kill 只能在 `t.Cleanup` 防泄漏，并且原测试必须失败。

- [ ] **Step 3:** 在带现有 `darwin && cgo && hostapisystemsmoke` build tag 的 `host_lifecycle_process_smoke_test.go` 增加真实配置失败：

```text
TestHostAPILifecycleProcessConfigFailure
```

使用无效 `HOST_API_SHUTDOWN_TIMEOUT=not-a-duration` 和一个 sentinel 敏感值，断言：

- exit `2`；
- stdout 不打印地址；
- stderr 恰好一行 `config_failed` JSON；
- 不含 `panic`、`goroutine`、sentinel。

- [ ] **Step 4:** 增加监听失败：

```text
TestHostAPILifecycleProcessListenFailure
```

测试先持有 loopback listener，再用有效 Host 配置和同一地址启动真实 binary，断言 exit `3`、code `listen_failed`、stderr 一行、worker/janitor 没有可观察副作用。

- [ ] **Step 5:** 运行：

```bash
(cd examples/host-api && go test ./... -run 'TestHost(Process|Exit)' -count=1)
(cd examples/host-api && go test -tags=hostapisystemsmoke -run 'TestHostAPILifecycleProcess(ConfigFailure|ListenFailure)' -count=1)
```

预期全部退出 0，无 `SKIP`。若真实 Keychain 前置条件缺失，应明确修复测试环境；不得把 SKIP 当通过。

- [ ] **Step 6:** 提交：

```bash
git add examples/host-api/host_process_smoke_test.go \
  examples/host-api/host_process_smoke_cleanup_test.go \
  examples/host-api/host_lifecycle_process_smoke_test.go
git commit -m "test(host-api): 精确断言真实进程退出边界"
```

---

### Task 9: 覆盖优雅 drain、超时、第二信号与重启恢复

**Files:**
- Modify: `examples/host-api/host_mvp_provider_smoke_test.go`
- Modify: `examples/host-api/host_lifecycle_process_smoke_test.go`
- Modify: `examples/host-api/host_process_smoke_test.go`

**Interfaces:**
- Consumes: 真实单槽 queued worker、可控 loopback Provider、SIGINT/SIGTERM、同一 runtime home。
- Produces: 进程级 drain/force/restart 证据。

- [ ] **Step 1:** 给现有 Provider stub 增加 barrier 模式，不增加生产开关：

```go
type providerBarrier struct {
    entered chan struct{}
    release chan struct{}
}
```

对应 handler 在收到请求后只关闭一次 `entered`，然后 select：

```go
select {
case <-barrier.release:
    // 返回正常 Provider 响应。
case <-r.Context().Done():
    // 记录 cancellation 并返回。
}
```

请求次数使用 mutex/atomic 记录；测试等待 `entered`，不得用 sleep 猜测已进入执行。

- [ ] **Step 2:** 增加真实优雅 drain 测试：

```text
TestHostAPILifecycleProcessGracefulDrainAndRestart
```

步骤：

1. 启动进程 A，提交两个 queued workflow；
2. 等第一项进入 Provider barrier；
3. 发送第一次 SIGTERM；
4. 轮询确认 listener 拒绝新连接；
5. 释放第一项；
6. 断言进程 A exit `0`、stderr 为空；
7. 只读检查第一项稳定停在 `waiting_approval`、第二项仍 pending 且无 lease；
8. 同一 runtime home 启动进程 B；
9. 断言第二项执行至 `waiting_approval`，第一项保持逐字段不变；
10. SIGINT 优雅停止进程 B，断言 exit `0`。

这里的 `waiting_approval` 是真实生产 workflow 在 Provider 成功后的既定安全边界；
测试不得通过生产开关、测试专用 executor 或 listener 关闭后的审批请求绕过最终人工审批。

- [ ] **Step 3:** 增加 drain timeout 测试：

```text
TestHostAPILifecycleProcessDrainTimeoutFailsActiveWorkflow
```

使用短但有余量的 `HOST_API_SHUTDOWN_TIMEOUT`；Provider 只响应 request context cancellation。断言 exit `5`、code `shutdown_timeout`，并从 store 公共读取路径确认：

```text
workflow.status = failed
workflow.error = host_shutdown_timeout
lease_owner = ""
lease_until = zero
```

使用同一 runtime home 重启后，在 operator 未 requeue 前 Provider 请求数不再增加。

- [ ] **Step 4:** 增加第二信号测试：

```text
TestHostAPILifecycleProcessSecondSignalForcesImmediately
```

配置长 drain timeout，等待第一信号已停止 listener 后发送第二信号；断言进程明显早于完整 drain timeout 退出，但不使用脆弱的毫秒级阈值：用一个“完整 timeout 到达”timer channel 做竞争，必须先收到 process exit。最终 exit `5`、code `shutdown_timeout`，workflow 同样失败并清 lease。

- [ ] **Step 5:** 运行：

```bash
(cd examples/host-api && go test -tags=hostapisystemsmoke -run 'TestHostAPILifecycleProcess(GracefulDrainAndRestart|DrainTimeoutFailsActiveWorkflow|SecondSignalForcesImmediately)' -count=1)
(cd examples/host-api && go test -race ./... -run 'Test(QueuedWorkerDrain|HostAPIService|ExecutionRegistry)' -count=1)
```

预期全部 PASS，无 SKIP；测试清理不得出现 Kill 被当作成功。

- [ ] **Step 6:** 提交：

```bash
git add examples/host-api/host_mvp_provider_smoke_test.go \
  examples/host-api/host_lifecycle_process_smoke_test.go \
  examples/host-api/host_process_smoke_test.go
git commit -m "test(host-api): 验证信号停机与重启恢复"
```

---

### Task 10: 用测试专属子进程证明外部副作用边界

**Files:**
- Create: `examples/host-api/host_side_effect_process_smoke_test.go`
- Modify: `examples/host-api/lifecycle_cleanup_test.go`

**Interfaces:**
- Consumes: 测试专属 step、真实 Host handler/worker、loopback side-effect sink、稳定 ToolCallID、真实 HTTP requeue。
- Produces: “不自动重放 + 显式重试由外部幂等去重”的边界证据。

- [ ] **Step 1:** 在 build-tag 测试文件中实现 loopback sink：

```go
type sideEffectSink struct {
    mu sync.Mutex
    requestsByToolCallID map[string]int
    appliedToolCallIDs   map[string]struct{}
    committed            chan struct{}
}
```

每个请求都增加 request count；只有首次出现的 ToolCallID 增加 applied count；首次真实生效后关闭 `committed` barrier。所有断言通过 sink 的只读 snapshot API，不暴露生产接口。

- [ ] **Step 2:** 在同一 `_test.go` 中实现 test helper subprocess。它必须：

- 复用真实 `Server.Handler()`、queued worker、execution registry、`hostAPIService` 和 `hostkit.Run`；
- `NewServer` 完成真实 store 组合后，仅在 helper process 内把 `server.executor` 替换为
  `workflowkit.NewExecutor(server.workflows, []workflowkit.Step{testSideEffectStep})`；
- step 用持久化在 workflow metadata/AgentRun 请求中的稳定 ToolCallID 调用 sink；
- sink 返回成功后，在本地 workflow 完成持久化前阻塞于测试 barrier；
- 不修改生产 binary 的路由、工具注册或环境变量解析。

helper process 通过 Go test 的标准 helper-process pattern 启动；测试控制参数只属于 `_test.go` 子进程。

- [ ] **Step 3:** 增加主测试：

```text
TestHostSideEffectBoundaryExplicitRequeueIsExternallyIdempotent
```

严格执行：

1. 进程 A 执行 workflow，等待 sink `committed`；
2. 发送第一信号进入 drain，再发送第二信号强制收口；
3. execution context 取消后释放测试 barrier，让 operation 返回；
4. 断言进程 A exit `5`；
5. 通过现有 store API 检查 workflow failed、AgentRun failed，checkpoint 若已 consumed 保持 consumed、若仍 leased 则 failed 且 lease 清空；
6. 使用同一 runtime home 启动进程 B，不 requeue，等待一个由“无请求观察窗口”控制的 context deadline，断言 sink request count 不增加；
7. 通过真实 `POST /workflows/{id}/requeue` 显式 requeue；
8. 断言 sink 对同一 ToolCallID 的 request count 增加，而 applied count 仍为 `1`；
9. 优雅停止进程 B。

第 6 步的 deadline 只证明一段有界时间内未自动执行，不用于调度正确性的碰运气；在此之前必须先由 Host ready/worker status barrier 证明进程 B 已进入可 claim 状态。

- [ ] **Step 4:** 增加负面断言：

- production `main.go`、`server.go` 不出现 test step/sink 名称；
- 无任何新的生产环境变量启用测试工具；
- cleanup 不调用 sink、不执行 tool retry、不创建 checkpoint；
- 测试文字明确“不证明 Host 跨系统 exactly-once”。

- [ ] **Step 5:** 运行：

```bash
(cd examples/host-api && go test -tags=hostapisystemsmoke -run '^TestHostSideEffectBoundaryExplicitRequeueIsExternallyIdempotent$' -count=1)
(cd examples/host-api && go test -race ./... -run 'TestFinalizeAgentApprovalShutdown' -count=1)
```

预期全部 PASS，无 SKIP。

- [ ] **Step 6:** 提交：

```bash
git add examples/host-api/host_side_effect_process_smoke_test.go \
  examples/host-api/lifecycle_cleanup_test.go
git commit -m "test(host-api): 固化外部副作用与显式重试边界"
```

---

### Task 11: 纳入 workspace、发布布局与用户文档，并执行全量验收

**Files:**
- Create: `hostkit/README.md`
- Modify: `go.work`
- Modify: `scripts/verify-all.sh`
- Modify: `scripts/verify-release-layout.sh`
- Modify: `docs/modules.md`
- Modify: `README.md`
- Modify: `examples/host-api/README.md`
- Modify: `docs/host-api-contract.md`

**Interfaces:**
- Consumes: 完成的 `hostkit` 与 Host lifecycle 行为。
- Produces: 第 13 个发布模块声明、可复现验证入口、准确的运维契约。

- [ ] **Step 1:** 先修改发布布局门禁测试清单，把 expected module 从 12 增到 13，但暂不改 `go.work`；运行：

```bash
bash ./scripts/verify-release-layout.sh
```

预期失败，明确报告缺少 `hostkit` workspace use/replace 或发布 manifest 项，而不是其他历史模块错误。

- [ ] **Step 2:** 在 `go.work` 增加：

```text
use ./hostkit
replace github.com/eruca/goagents/hostkit v0.1.0 => ./hostkit
```

在 `scripts/verify-release-layout.sh` 的 `published_modules` 增加：

```text
hostkit|github.com/eruca/goagents/hostkit|hostkit/v0.1.0
```

确认总数为 13，历史发布记录文件不修改。

- [ ] **Step 3:** 在 `scripts/verify-all.sh` 加入：

```bash
run_in "$ROOT/hostkit" go test ./...
```

位置放在其他基础模块测试之前或相邻位置；不改变其他门禁的 fail-closed 语义。

- [ ] **Step 4:** 编写 `hostkit/README.md`，只说明：

- 单 Service lifecycle 协调边界；
- `Service`、`Options`、`Result` 最小示例；
- code/exit 映射；
- 第一次/第二次 interrupt 与共享 cleanup budget；
- `hostkit` 不管理 workflow、HTTP、signal 常量、participant graph 或外部副作用。

- [ ] **Step 5:** 更新文档：

- `docs/modules.md`：把 `hostkit` 列为只依赖标准库的可选 Host-side capability；
- 根 `README.md`：模块数更新为当前 13，并链接 `hostkit`；不得回写历史发布记录；
- `examples/host-api/README.md`：记录 `HOST_API_SHUTDOWN_TIMEOUT`、SIGINT/SIGTERM、默认 30s/固定 5s、重启恢复；
- `docs/host-api-contract.md`：记录 `host_draining`、exit code 表、单行 stderr JSON、force 后 operator 显式 requeue；
- 明确 graceful shutdown 不等于任意崩溃点 exactly-once，外部系统应按稳定 ToolCallID 幂等。

- [ ] **Step 6:** 执行局部门禁：

```bash
(cd hostkit && go test ./... -count=1)
(cd hostkit && go test -race ./... -count=1)
(cd examples/host-api && go test ./... -count=1)
(cd examples/host-api && go test -race ./... -count=1)
(cd examples/host-api && go vet ./...)
bash ./scripts/verify-release-layout.sh
git diff --check
```

预期全部退出 0。

- [ ] **Step 7:** 执行 workspace 与真实进程最终门禁：

```bash
bash ./scripts/verify-all.sh
(cd examples/host-api && go test -tags=hostapisystemsmoke -run 'TestHostAPILifecycleProcess|TestHostSideEffectBoundary' -count=1)
```

预期全部 PASS，真实进程门禁不允许 SKIP。若环境前置条件导致 SKIP，修复前置条件后重跑，不能把 SKIP 记录为成功。

- [ ] **Step 8:** 做敏感输出和生成标记检查：

```bash
rg -n 'panic\\(|log\\.Fatal|TO[D]O|TB[D]|host_shutdown_timeout|host_draining' \
  hostkit examples/host-api docs README.md
git diff --check
git status --short
```

逐项确认：

- production `main` 不再 panic；
- 未完成生成标记不存在于本次新增内容；
- 稳定 code 只出现在设计允许的位置；
- 测试专属 side-effect helper 没进入 production 文件；
- worktree 只包含本计划范围内变更。

- [ ] **Step 9:** 提交：

```bash
git add hostkit/README.md go.work scripts/verify-all.sh scripts/verify-release-layout.sh \
  docs/modules.md README.md examples/host-api/README.md docs/host-api-contract.md
git commit -m "docs(hostkit): 纳入模块布局与Host运维契约"
```

- [ ] **Step 10:** 最终复核当前分支：

```bash
git status --short --branch
git log --oneline --decorate -12
git diff origin/main...HEAD --stat
git diff origin/main...HEAD --check
```

成功标准：

- Host 启动/监听错误无 panic 且退出码稳定；
- 两阶段 signal shutdown 与共享 cleanup budget 有 unit、race、真实进程证据；
- queued restart、lease 清理、approval/AgentRun/checkpoint 收口一致；
- 外部副作用测试证明不自动重放，并把 exactly-once 责任准确留在外部幂等；
- `hostkit` 成为第 13 个独立发布模块且无 workspace 内部依赖；
- 全部静态、单元、race、真实进程与发布布局门禁 PASS，无 SKIP。
