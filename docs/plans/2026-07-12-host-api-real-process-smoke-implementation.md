# Host API 本机真进程验收 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在真实 `main` 进程中证明 Host API 的 OIDC 认证、SQLite 持久化、Keychain 加密审批和重启恢复能形成一个安全的工具审批闭环。

**Architecture:** 新增一个带 `hostapisystemsmoke` 构建标签的 Darwin 测试。父测试进程运行现有本地 OIDC discovery/JWKS issuer，编译并启动真实 Host API 二进制，再通过 TCP HTTP 请求创建工具暂停、验证无效令牌、停止并重启同一 runtime、用已签名令牌批准工具和最终工作流。子进程停止后，测试只通过现有 SQLite store 读取安全状态和密文，不读取 Keychain 或解密 checkpoint。

**Tech Stack:** Go 1.26、`net/http`、`os/exec`、现有 `coreos/go-oidc` 测试 issuer、runkit/workflowkit SQLite stores、macOS Keychain。

## Global Constraints

- 测试文件必须使用 `//go:build darwin && cgo && hostapisystemsmoke`；不得加入 `scripts/verify-all.sh` 或默认 `go test ./...`。
- 启动真实编译后的 `main` 二进制，禁止向 `Config` 注入测试 cipher 或测试 authenticator。
- 使用 `t.TempDir()` 作为 `HOST_RUNTIME_HOME`，用同一目录跨进程重启；不得写入仓库或用户业务 runtime。
- 本地 OIDC issuer 仅供测试进程使用，测试令牌的 audience 固定为 `host-api`、subject 固定为 `operator-process`。
- 触发工具暂停会按既有产品路径在 Keychain 服务 `goagents.host-api.approvals` 创建或复用 `local-v1` 数据密钥；测试绝不导出、打印或删除该密钥。
- SQLite 断言只能检查 checkpoint 存在、状态、审批者和 ciphertext 不含测试输入；禁止把密文、提示词或 bearer token 输出到测试日志。
- 不新增 HTTP endpoint、数据库 schema、生产环境变量、分布式 worker 或 multi-agent 抽象。

---

### Task 1: 增加带构建标签的真实进程回归测试

**Files:**

- Create: `examples/host-api/host_process_smoke_test.go`

**Consumes:** `newOIDCTestProvider`, `oidcTestProvider.mintToken`, `workflowResponse`, `agentApprovalResponse`, `runkit/sqlitestore.Open`, `workflowkit/sqlitestore.Open`，以及既有 HTTP contract。

**Produces:** `TestHostAPIProcessToolApprovalSurvivesRestart`，可通过下列命令独立运行：

```bash
cd examples/host-api
go test -tags hostapisystemsmoke -run TestHostAPIProcessToolApprovalSurvivesRestart -count=1 -v
```

- [x] **Step 1: 写出失败的真进程测试和 HTTP/process helpers。**

  在文件首行加入：

  ```go
  //go:build darwin && cgo && hostapisystemsmoke
  ```

  增加 `hostProcess`、`buildHostBinary`、`startHostProcess`、`stopHostProcess`、`waitForHostReady`、`freeLoopbackAddress` 和 `processJSON` helpers。`buildHostBinary` 必须运行 `go build -o <t.TempDir()/host-api> .`；`startHostProcess` 必须设置 `HOST_API_ADDR`、`HOST_RUNTIME_HOME`、`HOST_API_OIDC_ISSUER` 和 `HOST_API_OIDC_AUDIENCE=host-api`，并通过 `GET /workers/queued` 等待 HTTP readiness。

  在测试中：

  ```go
  provider := newOIDCTestProvider(t)
  runtimeHome := t.TempDir()
  token := provider.mintToken(t, "operator-process", "host-api", time.Now().Add(time.Hour))
  first := startHostProcess(t, binary, runtimeHome, provider.issuer)
  created, status := processJSON[workflowResponse](t, first.client, http.MethodPost, "/workflows", map[string]any{
      "id": "wf-process-tool-approval",
      "input": "process-only approval checkpoint plaintext",
      "task_profile": map[string]any{"needs_tools": true},
  }, "")
  if status != http.StatusAccepted || created.AgentApproval == nil {
	      t.Fatalf("create status=%d agent_approval=%#v, want 202 and pending tool", status, created.AgentApproval)
  }
  ```

  以创建的精确 tool identity 构造 `POST /workflows/{id}/agent-approve` 请求；先用 `Bearer invalid` 断言 `401`，再停止第一个进程。此时运行测试，预期因 helper 尚未实现而编译失败。

- [x] **Step 2: 实现最小安全 helpers，并验证 pause 的持久化状态。**

  `stopHostProcess` 先发送 `os.Interrupt` 并在五秒内等待；超时才 `Kill`，失败消息只能包含捕获的子进程输出，不能包含 token。进程停止后，用：

  ```go
  runs, err := runsql.Open(filepath.Join(runtimeHome, "agent-runs.db"))
  checkpoint, err := runs.GetCheckpoint(ctx, created.AgentApproval.CheckpointID, localApprovalTenant)
  if checkpoint.Status != runkit.CheckpointPending || len(checkpoint.Ciphertext) == 0 || bytes.Contains(checkpoint.Ciphertext, []byte("process-only approval checkpoint plaintext")) {
      t.Fatalf("checkpoint did not remain opaque and pending")
  }
  ```

  关闭 store 后继续测试。此步骤通过时，真实 CLI 已在没有依赖注入的情况下完成 Keychain-backed 加密 pause。

- [x] **Step 3: 实现重启、精确批准和最终批准断言。**

  用同一 `runtimeHome` 和新的 loopback 地址启动第二个进程。使用先前 `created.AgentApproval.Tools[0]` 的 `index`、`tool_call_id`、`tool` 和 `allowed:true` 调用 agent approval endpoint，并携带已签名 bearer token。断言返回 `200`、状态仍为 `waiting_approval`、`agent_approval` 已清空。随后携带同一令牌调用：

  ```json
  {"note":"process smoke accepted"}
  ```

  到 `POST /workflows/wf-process-tool-approval/approve`，断言返回 `200` 和 `succeeded`。停止第二个进程，再分别打开 `agent-runs.db` 和 `workflow.db`，断言：

  ```go
  run.Summary.Status == runkit.StatusSucceeded && run.Summary.ToolCalls == 1
  checkpoint.Status == runkit.CheckpointConsumed && checkpoint.Approval.ApproverID == "operator-process"
  workflow.Status == workflowkit.StatusSucceeded && workflow.Metadata["approved_by"] == "operator-process"
  ```

- [x] **Step 4: 运行 tagged smoke。**

  Run:

  ```bash
  cd examples/host-api
  go test -tags hostapisystemsmoke -run TestHostAPIProcessToolApprovalSurvivesRestart -count=1 -v
  ```

  Expected: 真实二进制在两次启动间恢复同一 encrypted checkpoint；无效令牌返回 `401`，且唯一一次工具执行由 OIDC subject `operator-process` 审批。

### Task 2: 记录本机副作用与常规回归边界

**Files:**

- Modify: `examples/host-api/README.md`
- Modify: `docs/plans/2026-07-12-host-api-real-process-smoke-implementation.md`

**Consumes:** Task 1 的 tagged command 和既有 Keychain/SQLite contract。

**Produces:** 可手工触发的本机验收说明，不改变默认 CI 或 workspace verification。

- [x] **Step 1: 更新 README。**

  在 Keychain/审批段落后加入 tagged command，并明确：它会启动 loopback OIDC issuer 与真实 host binary；使用临时 runtime home；首次工具暂停会创建或复用本机 `goagents.host-api.approvals/local-v1` Keychain item；不会打印、导出或删除密钥；默认 `go test ./...` 与 `bash ./scripts/verify-all.sh` 不执行此 smoke。

- [x] **Step 2: 运行默认回归与格式检查。**

  Run:

  ```bash
  bash ./scripts/verify-all.sh
  (cd examples/host-api && go test -race ./...)
  git diff --check
  ```

  Expected: 默认回归不触发 Keychain smoke，所有模块和 Host API race tests 通过。

- [x] **Step 3: 仅在每个命令成功后更新 checkbox。**
