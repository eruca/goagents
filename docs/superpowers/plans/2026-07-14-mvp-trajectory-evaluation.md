# MVP Standard Trajectory and Outcome Evaluation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将真实 SQLite workflow/run/event 安全映射成可重复评分的 `evalkit` Outcome 与 Trace，不扩大 MVP 产品功能。

**Architecture:** `evalkit` 只增加独立的 Outcome DTO；`examples/host-api` 作为组合层读取现有 `workflowkit.Store` 和 `runkit.Store`。适配器使用固定字段白名单和显式指纹类型，结果、策略、效率仍由现有 `GraderFunc` 分开评分。

**Tech Stack:** Go 1.26.1、`evalkit`、`workflowkit`、`runkit`、host-api SQLite stores、Go testing。

## Global Constraints

- 不新增 HTTP 接口、数据库迁移、外部依赖、在线监控或模型调用。
- 不保存输入/输出正文、原始错误、未知 metadata、凭证或本机绝对信任路径。
- `evalkit` 不导入 `goagent`、`workflowkit`、`llmkit`、`runkit` 或 `artifactkit`。
- 所有生产代码必须先有按预期失败的测试；实现只满足当前测试和已确认设计。
- 提交信息使用简体中文，并按 `evalkit` 契约、host 适配器、文档三个语义边界提交。

---

### Task 1: 为 evalkit 增加 Outcome 契约

**Files:**
- Modify: `evalkit/evalkit.go`
- Modify: `evalkit/evalkit_test.go`

**Interfaces:**
- Produces: `evalkit.Outcome{Status, OutputRef, ErrorCode, Metadata}`
- Produces: `RunResult.Outcome` and `Trial.Outcome`
- Preserves: 现有 Harness、Grader、Trace 和 Summary 行为

- [ ] **Step 1: 写 Outcome 传递与复制失败测试**

在 `evalkit/evalkit_test.go` 增加 `TestRunnerCarriesAndClonesOutcome`。Harness 返回：

```go
sourceMetadata := map[string]any{"dataset_split": "held_out"}
return &RunResult{
    Outcome: Outcome{
        Status:    "succeeded",
        OutputRef: "artifact:wf-1:final",
        Metadata:  sourceMetadata,
    },
}, nil
```

grader 断言 `req.Trial.Outcome` 三个字段；Runner 返回后修改 `sourceMetadata`，再断言
`result.Trials[0].Trial.Outcome.Metadata["dataset_split"]` 仍为 `held_out`。

- [ ] **Step 2: 运行测试确认 RED**

Run: `cd evalkit && go test ./... -run TestRunnerCarriesAndClonesOutcome -count=1`

Expected: 编译失败，提示 `RunResult`/`Trial` 没有 `Outcome` 或 `Outcome` 未定义。

- [ ] **Step 3: 写最小实现**

在 `evalkit/evalkit.go` 增加：

```go
type Outcome struct {
    Status    string
    OutputRef string
    ErrorCode string
    Metadata  map[string]any
}
```

给 `RunResult`、`Trial` 增加 `Outcome Outcome`；`runTrial` 使用 `cloneOutcome` 赋值：

```go
func cloneOutcome(outcome Outcome) Outcome {
    outcome.Metadata = cloneMetadata(outcome.Metadata)
    return outcome
}
```

- [ ] **Step 4: 运行 Outcome 与 evalkit 全量测试确认 GREEN**

Run: `cd evalkit && go test ./... -run TestRunnerCarriesAndClonesOutcome -count=1`

Expected: PASS。

Run: `cd evalkit && go test ./... -count=1`

Expected: PASS。

- [ ] **Step 5: 提交 evalkit 契约**

```bash
git add evalkit/evalkit.go evalkit/evalkit_test.go
git commit -m "feat(evalkit): 增加可评分结果契约"
```

### Task 2: 增加 host SQLite 轨迹适配器和 held-out 评测

**Files:**
- Create: `examples/host-api/eval_trace.go`
- Create: `examples/host-api/eval_trace_test.go`

**Interfaces:**
- Consumes: `workflowkit.Store.Get`, `runkit.Store.FindByWorkflowID/Get/Events`
- Consumes: `evalkit.Outcome`, `evalkit.Trace`, `evalkit.TraceStep`, `evalkit.Usage`
- Produces: `hostEvalFingerprint`
- Produces: `buildHostEvalResult(context.Context, string, workflowkit.Store, runkit.Store, hostEvalFingerprint) (*evalkit.RunResult, error)`

- [ ] **Step 1: 写真实 SQLite 评测失败测试**

在 `eval_trace_test.go` 增加 `TestHostEvalSuiteUsesPersistedTrajectoryAndOutcome`：

1. 用 `NewServer(Config{RuntimeHome: t.TempDir(), ApprovalAuthenticator: testApprovalAuthenticator{...}})` 创建真实 SQLite stores；
2. 用 `writeHostAPISkill` 创建不申请工具的 `trajectory-review`，通过 `skillkit.Discover`
   注入 catalog；将 `&skillEvalProvider{}` 赋给 `server.providers["local-free"]`，再用
   `doJSON[workflowResponse]` 创建带 `trajectory-review` 引用的 workflow；
3. 调用最终审批使 workflow 进入 `succeeded`；
4. 向真实 run store 追加一条含 allowlisted `tool`/`ref` 和禁止字段 `input`/`secret`/`Message` 的事件；
5. Harness 调用 `buildHostEvalResult`；Task metadata 写入 `dataset_split=held_out`；
6. 分别注册 `outcome-contract`、`trajectory-policy`、`efficiency-budget` 三个 grader；
7. 断言 trial 通过、Outcome 引用正确、存在 workflow/agent 多步骤轨迹、usage 来自 run summary、指纹键完整、Skill refs 和 visible tools 排序去重；
8. JSON 序列化 Trace 后断言输入正文、event message、`input`、`secret` 和本机 Skill root 均不存在。

另加 `TestBuildHostEvalResultRequiresStores`，断言 nil workflow/run store 返回明确错误而不 panic。

- [ ] **Step 2: 运行测试确认 RED**

Run: `cd examples/host-api && go test ./... -run '^(TestHostEvalSuiteUsesPersistedTrajectoryAndOutcome|TestBuildHostEvalResultRequiresStores)$' -count=1`

Expected: 编译失败，提示 `buildHostEvalResult` 或 `hostEvalFingerprint` 未定义。

- [ ] **Step 3: 实现只读适配器**

`eval_trace.go` 实现以下最小结构：

```go
type hostEvalFingerprint struct {
    GitCommit           string
    Provider            string
    ModelAlias          string
    AgentDefinitionHash string
    PromptVersion       string
    VisibleToolIDs      []string
}
```

`buildHostEvalResult` 必须：

- 校验两个 store 非 nil；
- 读取 workflow、相关 runs 和每个 run 的 events；
- workflow step 只映射 attempt/output_ref/agent_run_id/audit_ref/approval_ref；
- agent event 只映射 sequence/stage/iteration/tool/ref，不复制 Message；
- 按非零时间优先、开始时间升序做稳定排序；
- 汇总所有 terminal summary 的 token、LLM/tool calls；
- 根据 workflow status/output ref 构造 Outcome，failed/cancelled 使用稳定通用 error code；
- 只产生设计规定的固定 labels；Skill refs 和 visible tools 排序，visible tools 去重。

事件状态归一规则：`.started -> running`，`.completed`/`finalized`/`output.validated -> succeeded`，
`.failed`/`.denied`/`.rejected -> failed`，`.pending`/`.requested -> waiting_approval`，其他为空。

- [ ] **Step 4: 运行专项测试确认 GREEN**

Run: `cd examples/host-api && go test ./... -run '^(TestHostEvalSuiteUsesPersistedTrajectoryAndOutcome|TestBuildHostEvalResultRequiresStores)$' -count=1`

Expected: PASS。

Run: `cd examples/host-api && go test ./... -count=1`

Expected: PASS，现有 Skill 安全评测也保持通过。

- [ ] **Step 5: 提交 host 评测适配器**

```bash
git add examples/host-api/eval_trace.go examples/host-api/eval_trace_test.go
git commit -m "test(host-api): 接入真实轨迹结果评测"
```

### Task 3: 同步文档并完成全量验证

**Files:**
- Modify: `evalkit/README.md`
- Modify: `docs/superpowers/specs/2026-07-14-mvp-validation-design.md`
- Modify: `docs/superpowers/specs/2026-07-14-mvp-trajectory-evaluation-design.md`

**Interfaces:**
- Documents: Outcome 与 host adapter 边界
- Documents: MVP 测试证据必须同时包含 outcome、trajectory、efficiency

- [ ] **Step 1: 更新文档**

在 `evalkit/README.md` 增加 Outcome 示例和脱敏规则；在 MVP 验收设计的“测试证据”中加入
Outcome、轨迹策略、usage 与固定指纹；将轨迹评测设计状态改成 `已实现`，并记录真实代码路径。

- [ ] **Step 2: 格式与静态检查**

Run: `gofmt -w evalkit/evalkit.go evalkit/evalkit_test.go examples/host-api/eval_trace.go examples/host-api/eval_trace_test.go`

Run: `git diff --check`

Expected: 无输出，退出码 0。

Run: `cd evalkit && go vet ./...`

Expected: PASS。

Run: `cd examples/host-api && go vet ./...`

Expected: PASS。

- [ ] **Step 3: 运行完整 workspace 验证**

Run: `bash ./scripts/verify-all.sh`

Expected: 退出码 0，最后输出 `goagents workspace verification passed`。

- [ ] **Step 4: 核对范围并提交文档**

Run: `git status --short && git diff --stat HEAD~2 && git log -4 --oneline`

Expected: 只有计划内代码和文档；没有 SQLite、Keychain、日志或临时报告文件。

```bash
git add evalkit/README.md \
  docs/superpowers/specs/2026-07-14-mvp-validation-design.md \
  docs/superpowers/specs/2026-07-14-mvp-trajectory-evaluation-design.md
git commit -m "docs(mvp): 记录真实轨迹评测基线"
```

- [ ] **Step 5: 提交后重新验证工作区状态**

Run: `git status --short --branch && git log -5 --oneline`

Expected: 功能分支干净，最近三个提交分别对应 evalkit、host 适配器和文档。
