# Host API MVP Unified Black-box Acceptance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新增一个可重复的真实 Host API 进程验收 smoke，覆盖审批与 Skill 重启、Provider 失败后 requeue，以及未注册工具失败关闭。

**Architecture:** 测试用 loopback OpenAI-compatible stub 提供 `ready`、`unavailable` 和 `unregistered_tool` 三种互斥状态；真实 Host 子进程通过临时 `LLMKIT_HOME/config.yaml` 连接该 stub。所有产品行为断言只通过公开 HTTP API完成，SQLite 和 Keychain CLI 仅用于环境生命周期管理。

**Tech Stack:** Go、`httptest.Server`、OpenAI Chat Completions JSON、macOS Keychain、OIDC test provider、SQLite-backed Host runtime、Go build tags。

## Global Constraints

- 只新增 `darwin && cgo && hostapisystemsmoke` 测试代码，不修改生产 HTTP API、数据库结构、路由或重试策略。
- 不直接读取或修改 SQLite 作为验收断言。
- Keychain service 必须使用唯一 `.smoke.` 前缀并精确清理。
- Provider key 只能是测试合成值；不得加载或保存 todo 的真实 Qwen key。
- Artifact 只验证公开 workflow/run 响应中的稳定 `input_ref/output_ref`。
- 每个子场景使用独立 runtime home；不共享 workflow、审计或 Keychain 状态。
- 失败时只输出公开状态和脱敏日志，不输出请求 Authorization 或本机 Skill 物理路径。

---

### Task 1: 可控 OpenAI-compatible Stub 与审批/Skill 重启黑盒场景

**Files:**
- Create: `examples/host-api/host_mvp_provider_smoke_test.go`
- Create: `examples/host-api/host_mvp_blackbox_smoke_test.go`
- Reuse: `examples/host-api/host_process_smoke_test.go`

**Interfaces:**
- Produces: `mvpProviderStub`、`newMVPProviderStub`、`SetMode`、`Requests`、`writeMVPLLMKitConfig`。
- Produces: `runMVPApprovalSkillRestart(t, binary)` 和 `waitForProcessWorkflowStatus`。
- Consumes: `buildHostBinary`、`startHostProcessWithEnv`、`processJSON`、OIDC 与 Keychain smoke helpers。

- [ ] **Step 1: 写入顶层失败测试**

创建 `host_mvp_blackbox_smoke_test.go`：

```go
//go:build darwin && cgo && hostapisystemsmoke

package main

import "testing"

func TestHostAPIProcessMVPBlackBoxClosure(t *testing.T) {
	binary := buildHostBinary(t)
	t.Run("approval skill and restart", func(t *testing.T) {
		runMVPApprovalSkillRestart(t, binary)
	})
}
```

- [ ] **Step 2: 验证 RED**

Run:

```bash
cd examples/host-api
go test -tags hostapisystemsmoke -run '^TestHostAPIProcessMVPBlackBoxClosure$' -count=1
```

Expected: FAIL，`runMVPApprovalSkillRestart` 未定义。

- [ ] **Step 3: 实现线程安全 Provider stub**

在 `host_mvp_provider_smoke_test.go` 定义：

```go
type mvpProviderMode string

const (
	mvpProviderReady            mvpProviderMode = "ready"
	mvpProviderUnavailable      mvpProviderMode = "unavailable"
	mvpProviderUnregisteredTool mvpProviderMode = "unregistered_tool"
	mvpProviderAPIKey                           = "mvp-smoke-provider-key"
	mvpProviderAPIKeyEnv                        = "HOST_API_MVP_SMOKE_PROVIDER_KEY"
)

type mvpProviderRequest struct {
	Authorization      string
	ToolNames          []string
	HasToolObservation bool
}

type mvpProviderStub struct {
	server   *httptest.Server
	mu       sync.Mutex
	mode     mvpProviderMode
	requests []mvpProviderRequest
}
```

Handler 必须解析 `model/messages/tools`，记录工具名和 tool observation，并按模式返回：

```go
switch mode {
case mvpProviderUnavailable:
	http.Error(w, `{"error":{"message":"mvp smoke unavailable"}}`, http.StatusServiceUnavailable)
case mvpProviderUnregisteredTool:
	writeMVPChatResponse(w, "", "call-unregistered", "unregistered_tool")
default:
	if len(toolNames) > 0 && !hasToolObservation {
		writeMVPChatResponse(w, "", "call-record-review", recordReviewToolName)
		return
	}
	writeMVPChatResponse(w, "mvp smoke response", "", "")
}
```

`writeMVPLLMKitConfig` 创建 `runtimeHome/.llmkit/config.yaml`，内容固定为一个
OpenAI-compatible advanced model，`base_url` 为 `stub.URL()+"/v1"`，
`api_key_env` 为 `HOST_API_MVP_SMOKE_PROVIDER_KEY`，文件权限 `0600`。

- [ ] **Step 4: 实现审批、Skill 与重启场景**

`runMVPApprovalSkillRestart` 必须完成：

```go
requireInteractiveLoginKeychain(t)
provider := newMVPProviderStub(t, mvpProviderReady)
runtimeHome := t.TempDir()
writeMVPLLMKitConfig(t, runtimeHome, provider.URL())
oidc := newOIDCTestProvider(t)
token := oidc.mintToken(t, "operator-mvp", "host-api", time.Now().Add(time.Hour))
skillRoot, err := filepath.Abs("skills")
if err != nil { t.Fatalf("resolve Skill root: %v", err) }
```

然后使用唯一 Keychain service 和真实 Host 进程：

1. `GET /skills` 读取 `workflow-review` digest；
2. `POST /workflows` 创建 `needs_tools=true` workflow 并取得 agent approval；
3. 无效 bearer 调用返回 401；
4. 重启后 `GET /workflows/{id}` 仍显示同一 approval；
5. 有效 token 完成 agent approval 和最终 approval；
6. 创建带 `workflow-review` 的 workflow，确认完整 digest，再完成最终 approval；
7. 再次重启后读取两个 workflow、`GET /agent-runs/{id}`、routes、events 和 skills；
8. 断言 succeeded、tool call 数、route success、digest 和 ref 稳定；
9. Stub 请求必须包含 `Bearer mvp-smoke-provider-key`，且工具 observation 已进入第二次调用；
10. 精确清理 Keychain item。

增加进程轮询 helper：

```go
func waitForProcessWorkflowStatus(t *testing.T, process *hostProcess, id string, want workflowkit.Status) workflowResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last workflowResponse
	for time.Now().Before(deadline) {
		last, _ = processJSON[workflowResponse](t, process, http.MethodGet, "/workflows/"+id, nil, "")
		if last.Status == string(want) { return last }
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("workflow %s status=%q, want=%q", id, last.Status, want)
	return workflowResponse{}
}
```

- [ ] **Step 5: 验证 GREEN**

Run:

```bash
cd examples/host-api
go test -v -tags hostapisystemsmoke -run '^TestHostAPIProcessMVPBlackBoxClosure$/approval_skill_and_restart$' -count=1
```

Expected: PASS 且没有 SKIP。

- [ ] **Step 6: 提交第一个场景**

```bash
git add examples/host-api/host_mvp_provider_smoke_test.go \
  examples/host-api/host_mvp_blackbox_smoke_test.go
git commit -m "test(host-api): 覆盖 MVP 审批与 Skill 重启黑盒闭环"
```

### Task 2: Provider 失败与 requeue 黑盒场景

**Files:**
- Modify: `examples/host-api/host_mvp_blackbox_smoke_test.go`

**Interfaces:**
- Consumes: `mvpProviderStub.SetMode`、临时 llmkit config、OIDC provider、进程 HTTP helpers。
- Produces: `runMVPProviderRequeue(t, binary)`。

- [ ] **Step 1: 增加失败测试入口**

在顶层测试增加：

```go
t.Run("provider failure and requeue", func(t *testing.T) {
	runMVPProviderRequeue(t, binary)
})
```

- [ ] **Step 2: 验证 RED**

Run:

```bash
cd examples/host-api
go test -tags hostapisystemsmoke -run '^TestHostAPIProcessMVPBlackBoxClosure$/provider_failure_and_requeue$' -count=1
```

Expected: FAIL，`runMVPProviderRequeue` 未定义。

- [ ] **Step 3: 实现 HTTP-only requeue 场景**

实现固定流程：

```go
provider := newMVPProviderStub(t, mvpProviderUnavailable)
// 写配置并启动真实 Host，创建 run_mode=queued workflow。
failed := waitForProcessWorkflowStatus(t, process, workflowID, workflowkit.StatusFailed)
routes, _ := processJSON[llmRoutesResponse](t, process, http.MethodGet, "/workflows/"+workflowID+"/llm-routes", nil, "")
// 断言失败 route 的 outcome 为 provider_error/transient_error。
provider.SetMode(mvpProviderReady)
requeued, status := processJSON[workflowResponse](t, process, http.MethodPost, "/workflows/"+workflowID+"/requeue", nil, "")
// 断言 202、同一 ID；轮询 waiting approval，OIDC approve 后 succeeded。
```

最后读取 events，必须同时存在 failed step 和 `workflow_requeued`；整个函数不得打开 SQLite 或
直接写 artifact store。

- [ ] **Step 4: 验证 GREEN**

Run:

```bash
cd examples/host-api
go test -v -tags hostapisystemsmoke -run '^TestHostAPIProcessMVPBlackBoxClosure$/provider_failure_and_requeue$' -count=1
```

Expected: PASS。

- [ ] **Step 5: 提交 requeue 场景**

```bash
git add examples/host-api/host_mvp_blackbox_smoke_test.go
git commit -m "test(host-api): 覆盖 Provider 失败后的黑盒重排"
```

### Task 3: 未注册工具失败关闭场景

**Files:**
- Modify: `examples/host-api/host_mvp_blackbox_smoke_test.go`

**Interfaces:**
- Consumes: `mvpProviderUnregisteredTool`、Agent run/events HTTP response。
- Produces: `runMVPUnregisteredTool(t, binary)`。

- [ ] **Step 1: 增加失败测试入口**

```go
t.Run("unregistered tool fails closed", func(t *testing.T) {
	runMVPUnregisteredTool(t, binary)
})
```

- [ ] **Step 2: 验证 RED**

Run:

```bash
cd examples/host-api
go test -tags hostapisystemsmoke -run '^TestHostAPIProcessMVPBlackBoxClosure$/unregistered_tool_fails_closed$' -count=1
```

Expected: FAIL，`runMVPUnregisteredTool` 未定义。

- [ ] **Step 3: 实现失败关闭断言**

以 `mvpProviderUnregisteredTool` 启动 stub，创建 `needs_tools=true` queued workflow 并轮询到
`failed`。通过 `GET /agent-runs/{id}` 检查：

```go
for _, event := range run.Events {
	if event.Type == "tool.started" || event.Type == "tool.completed" {
		t.Fatalf("unexpected tool execution event: %+v", event)
	}
}
```

同时断言 workflow 没有 `AgentApproval`；Stub 记录的 Host 授权工具只有 `record_review`，不包含
`unregistered_tool`；workflow/events/process log 不包含合成 API key 或 Skill root 绝对路径。

- [ ] **Step 4: 验证 GREEN**

Run:

```bash
cd examples/host-api
go test -v -tags hostapisystemsmoke -run '^TestHostAPIProcessMVPBlackBoxClosure$/unregistered_tool_fails_closed$' -count=1
```

Expected: PASS。

- [ ] **Step 5: 提交失败关闭场景**

```bash
git add examples/host-api/host_mvp_blackbox_smoke_test.go
git commit -m "test(host-api): 验证未注册工具失败关闭"
```

### Task 4: 全量验证与验收记录

**Files:**
- Modify: `docs/superpowers/specs/2026-07-14-host-api-mvp-blackbox-acceptance-design.md`

**Interfaces:**
- Consumes: 三个子场景的真实执行结果。
- Produces: 设计文档状态和验收证据摘要，不保存 secret、绝对路径或原始 Provider body。

- [ ] **Step 1: 运行三个子场景**

```bash
cd examples/host-api
go test -v -tags hostapisystemsmoke -run '^TestHostAPIProcessMVPBlackBoxClosure$' -count=1 ./...
```

Expected: 三个子场景均 PASS，无 SKIP。

- [ ] **Step 2: 运行 Host API 与 workspace 回归**

```bash
go test ./... -count=1
go vet ./...
go vet -tags hostapisystemsmoke ./...
cd ../..
bash ./scripts/verify-all.sh
git diff --check
```

Expected: 所有命令退出码为 0。

- [ ] **Step 3: 更新设计状态**

把设计文档状态改为 `已实现并验收`，增加脱敏证据：commit、三个子场景 PASS、无 SKIP、
Host/API/workspace 验证通过、Keychain 精确清理、无真实凭证使用。

- [ ] **Step 4: 提交验收记录**

```bash
git add docs/superpowers/specs/2026-07-14-host-api-mvp-blackbox-acceptance-design.md
git commit -m "docs(mvp): 记录 Host API 黑盒验收结果"
```

- [ ] **Step 5: 检查最终工作区**

```bash
git status --short --branch
git log --oneline --decorate -8
```

Expected: 功能分支工作区干净，提交按测试场景和验收记录分离。
