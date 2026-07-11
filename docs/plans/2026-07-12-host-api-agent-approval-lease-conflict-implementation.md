# Host API 工具批准 Lease 冲突 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让同时到达的第二个工具批准请求返回明确的冲突结果，且绝不把已被另一请求租用的 approval workflow 或 agent run 标记为失败。

**Architecture:** `runkit.CheckpointStore` 已经原子地让一个请求获得 lease，并以 `ErrCheckpointNotClaimable` 拒绝竞争者。Host API 必须把该错误作为“本请求未拥有 lease”的 `409 approval_conflict`，在调用 `failAgentApprovalWorkflow` 前直接返回。一个测试专用 forwarding store 在首个成功 lease 后阻塞，以确定性地让第二个 HTTP 请求命中冲突，再释放首个请求验证正常完成。

**Tech Stack:** Go 1.26、`net/http/httptest`、现有 runkit checkpoint store、workflowkit/runkit memory stores。

## Global Constraints

- 不改动 `runkit.CheckpointStore`、加密、lease 或 agentcore 的公开 API。
- 只处理来自 `ApproveAndResume` 的 `runkit.ErrCheckpointNotClaimable`；格式错误 resolution 仍按现有 `400 invalid_request` 和 fail-closed 语义处理。
- 冲突请求不得执行工具、不得调用 `failAgentApprovalWorkflow`、不得清除 approval metadata、不得写终态 run summary。
- 获得 lease 的请求继续复用现有 resume/persist 流程，最终只执行一次 `record_review` 工具。
- OpenAPI 中已有 agent-approve `409` response；只补错误码语义，不扩展 endpoint 或 response schema。

---

### Task 1: 用确定性 lease 竞争测试锁定状态机

**Files:**

- Modify: `examples/host-api/agent_approval_test.go`

**Consumes:** `Server.agentApprovals.checkpoints`, `runkit.CheckpointStore`, `createToolApprovalWorkflow`, `agentApprovalRequestForTest`。

**Produces:** `TestHostAPIAgentToolApprovalLeaseConflictDoesNotFailWinner` 和仅限测试的 `blockingFirstLeaseCheckpointStore`。

- [x] **Step 1: 写失败测试。**

  创建工具暂停 workflow 后，用 wrapper 替换 `server.agentApprovals.checkpoints`。wrapper 的 `ApproveAndLease` 先委托给嵌入的 store；第一次成功 lease 后关闭 `acquired` channel 并阻塞在 `release` channel，失败 lease 直接返回。完整 wrapper 只覆写该方法：

  ```go
  type blockingFirstLeaseCheckpointStore struct {
      runkit.CheckpointStore
      acquired chan struct{}
      release  chan struct{}
      once     sync.Once
  }

  func (s *blockingFirstLeaseCheckpointStore) ApproveAndLease(ctx context.Context, request runkit.ApprovalLeaseRequest) (runkit.ApprovalCheckpoint, error) {
      checkpoint, err := s.CheckpointStore.ApproveAndLease(ctx, request)
      if err != nil {
          return runkit.ApprovalCheckpoint{}, err
      }
      s.once.Do(func() {
          close(s.acquired)
          <-s.release
      })
      return checkpoint, nil
  }
  ```

  以 goroutine 发起第一个 allowed request，等待 `acquired`；然后同步发起相同 allowed request。期望第二个响应为 `409`，workflow 仍为 `waiting_approval` 并仍带原 checkpoint metadata。关闭 `release` 后断言第一个请求 `200`、workflow 保持最终批准等待状态、agent run 成功且 `ToolCalls == 1`。此时运行：

  ```bash
  cd examples/host-api
  go test ./... -run TestHostAPIAgentToolApprovalLeaseConflictDoesNotFailWinner -count=1
  ```

  Expected: FAIL，当前第二个请求走通用失败分支，返回 `500` 并破坏 winner 的 workflow 状态。

- [x] **Step 2: 实现 Host API 冲突映射。**

  在 `handleApproveAgentTool` 中，紧接 `ApproveAndResume` 调用后、所有 `ErrApprovalPending` 和通用失败分支之前加入：

  ```go
  if errors.Is(resumeErr, runkit.ErrCheckpointNotClaimable) {
      writeError(w, http.StatusConflict, "approval_conflict", "agent tool approval is already being processed")
      return
  }
  ```

  此路径不接触 run/workflow store。不要把此错误合并进 `ErrInvalidApprovalResolution` 或通用 `500` 分支。

- [x] **Step 3: 验证竞争测试与 race detector。**

  Run:

  ```bash
  cd examples/host-api
  go test ./... -run TestHostAPIAgentToolApprovalLeaseConflictDoesNotFailWinner -count=1
  go test -race ./... -run TestHostAPIAgentToolApprovalLeaseConflictDoesNotFailWinner -count=1
  ```

  Expected: 首个请求单独执行工具并进入最终批准等待；竞争请求为 `409 approval_conflict`，没有失败状态或第二次工具执行。

### Task 2: 同步运行契约并回归

**Files:**

- Modify: `examples/host-api/openapi.yaml`
- Modify: `docs/host-api-contract.md`
- Modify: `docs/plans/2026-07-12-host-api-agent-approval-lease-conflict-implementation.md`

**Consumes:** Task 1 的 `409 approval_conflict` HTTP contract。

**Produces:** 文档化的并发批准行为，默认 API schema 不变。

- [x] **Step 1: 记录 `409 approval_conflict`。**

  在 agent-approve prose 中说明：如果另一个请求已经租用相同 checkpoint，当前请求返回 `409 approval_conflict`；它不会执行工具、不会改变 workflow 或 agent run，lease owner 继续处理。将 `approval_conflict` 加到 `ErrorResponse.error` 的枚举；保留既有 `409` response。

- [x] **Step 2: 跑完整验证。**

  Run:

  ```bash
  bash ./scripts/verify-all.sh
  (cd examples/host-api && go test -race ./...)
  git diff --check
  ```

  Expected: 全仓通过，且默认 Host API tests 覆盖 lease winner/loser 状态隔离。

- [x] **Step 3: 仅在命令成功后更新 checkbox。**
