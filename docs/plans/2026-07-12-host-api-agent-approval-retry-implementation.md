# Host API 工具批准安全重试 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让已成功完成工具批准、但客户端丢失响应后的完全相同 allow 请求可安全重试，并返回已有 workflow 状态而不重放工具。

**Architecture:** workflow 在工具批准完成后，已有由 host 产生的最终输出与普通最终批准等待态。持久化这次已完成批准的 checkpoint ID 和安全 tool identities（index、tool_call_id、tool），不保存任何 tool input。下一次 agent-approve 仍先完成 OIDC 身份验证与严格 JSON 解析；只有 workflow 正处于最终批准等待态且 incoming resolutions 全部 allow、与持久化 identities 精确一一匹配，才返回现有 response。任何 pending 新批准、拒绝、终态 workflow 或不匹配 resolution 都保持现有错误路径。

**Tech Stack:** Go 1.26、Host API metadata、workflowkit SQLite/memory stores、现有 HTTP contract。

## Global Constraints

- 不增加 HTTP request 字段、idempotency key、数据库表或 goagent/runkit 公开 API。
- metadata 只能存 checkpoint ID 和 safe pending tool identities；不得存 checkpoint plaintext、tool JSON input、prompt、bearer token 或自由文本原因。
- 仅对所有 resolution 为 `allowed:true` 且 index/tool_call_id/tool 精确匹配的 retry 返回 `200`。
- retry 必须仍要求有效 OIDC bearer token；不能在认证前读取或返回 workflow。
- 对仍有 pending approval 的 workflow，优先执行既有 pending 路径，绝不能把旧 retry 当作新工具批准。
- 重试不得写 artifact、agent run summary、checkpoint lease 或 workflow metadata。

---

### Task 1: 保存与验证安全的已完成批准 identity

**Files:**

- Modify: `examples/host-api/agent_approval.go`
- Modify: `examples/host-api/server.go`
- Modify: `examples/host-api/agent_approval_test.go`

**Consumes:** `agentApprovalResponse`, `agentApprovalFromMetadata`, `persistResumedAgentResult`, `agentApprovalRequest.coreResolutions`。

**Produces:** 安全 completed-approval metadata helpers 和精确 all-allowed retry 判断。

- [x] **Step 1: 写失败的 response-loss retry 测试。**

  创建工具暂停 workflow，以现有 exact allow request 调用一次 agent-approve 并断言 `200`。随后用相同 body 和有效 bearer 再调用一次，断言同样 `200`、返回 `waiting_approval` 且没有 `agent_approval`。直接读取 run 断言 `StatusSucceeded` 和 `ToolCalls == 1`；读取 workflow 断言 pending metadata 已清除而 completed safe approval metadata 存在。

  在同一测试中，以篡改 `tool_call_id` 的 all-allowed retry 再调用一次，断言 `400`，并再次断言 run 的 `ToolCalls` 仍为 `1`。运行：

  ```bash
  cd examples/host-api
  go test ./... -run TestHostAPIAgentToolApprovalExactRetryReturnsExistingWorkflow -count=1
  ```

  Expected: FAIL，当前成功后的重复请求返回 `400 invalid_request`，因为 pending metadata 已被清除。

- [x] **Step 2: 添加最小 completed metadata helpers。**

  在 `agent_approval.go` 增加两项私有 metadata keys，并实现：

  ```go
  func completedAgentApprovalFromMetadata(map[string]any) *agentApprovalResponse
  func rememberCompletedAgentApprovalMetadata(map[string]any, agentApprovalResponse)
  func resolutionsMatchCompletedApproval([]agentcore.ToolApprovalResolution, []agentApprovalPendingTool) bool
  ```

  前两个 helper 必须复用同一 safe tool JSON 校验；第三个 helper 必须要求长度相等、每个 index 只出现一次、`ToolCallID` 与 `Tool` 匹配且所有 `Allowed` 为 true。

- [x] **Step 3: 只在成功持久化路径记录与读取 retry 状态。**

  在 `persistResumedAgentResult` 的 workflow update 内，确认当前 pending checkpoint 后先记录 completed metadata，再清除 pending metadata。`handleApproveAgentTool` 在读取 workflow 后：若没有 pending approval，但 workflow 是最终批准等待态、completed metadata 存在且 resolutions 精确匹配，则直接 `writeJSON(... workflowToResponse(run, RunModeSync))`；否则保持现有 `400 invalid_request`。

  不在 `replacePendingAgentApproval`、拒绝、过期或失败路径记录 completed metadata。

- [x] **Step 4: 运行定向测试与 race detector。**

  Run:

  ```bash
  cd examples/host-api
  go test ./... -run TestHostAPIAgentToolApprovalExactRetryReturnsExistingWorkflow -count=1
  go test -race ./... -run TestHostAPIAgentToolApprovalExactRetryReturnsExistingWorkflow -count=1
  ```

  Expected: 精确 retry 返回已有 final-approval workflow；篡改 retry 为 `400`；工具只执行一次。

### Task 2: 同步 HTTP 契约并回归

**Files:**

- Modify: `examples/host-api/openapi.yaml`
- Modify: `docs/host-api-contract.md`
- Modify: `docs/plans/2026-07-12-host-api-agent-approval-retry-implementation.md`

**Consumes:** Task 1 的 exact retry behavior。

**Produces:** 无 request schema 改动的 response-loss retry contract。

- [x] **Step 1: 记录 retry 语义。**

  在 agent-approve OpenAPI description 与 prose contract 中说明：相同安全 identities 的已完成 allow retry 返回当前 workflow 的 `200` response，不执行工具；该行为仍需要 OIDC token，且不匹配 resolution 返回 `400 invalid_request`。不得把 completed metadata 暴露在 `WorkflowResponse`。

- [x] **Step 2: 跑完整验证。**

  Run:

  ```bash
  bash ./scripts/verify-all.sh
  (cd examples/host-api && go test -race ./...)
  git diff --check
  ```

  Expected: 全仓通过，默认 API response schema 不变。

- [x] **Step 3: 仅在命令成功后更新 checkbox。**
