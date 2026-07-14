# GoAgents MVP 标准轨迹与结果评测设计

**日期：** 2026-07-14

**状态：** 已实现

**实现路径：**

- `evalkit/evalkit.go`：`Outcome`、Runner 传递与防御性复制；
- `examples/host-api/eval_trace.go`：SQLite workflow/run/event 只读适配器；
- `examples/host-api/eval_trace_test.go`：held-out 场景与 outcome、policy、efficiency grader。

## 1. 决策摘要

MVP 继续保持功能冻结，只补测试基础设施：把宿主已经持久化的 workflow、agent run 和
run event 映射成统一、脱敏的 `evalkit` 轨迹，并把 workflow 最终状态建模为独立结果。

本设计不增加 HTTP 接口、数据库表、模型调用、在线监控、自动 Prompt 优化或 Skill
自修改能力。它只让现有真实执行证据能够进入重复评测和 grader，作为后续 MVP 验收、
GEPA/ACE 离线实验和失败回归集的共同基线。

## 2. 方案选择

采用“`evalkit` 增加通用结果 DTO，`examples/host-api` 提供组合适配器”的方案：

- `evalkit` 只新增不依赖其他模块的 `Outcome` 契约，并保证 Runner 对其做防御性复制；
- `examples/host-api` 读取 `workflowkit.Store` 与 `runkit.Store` 的现有记录，构造
  `evalkit.RunResult`；
- grader 继续使用现有 `Grader`/`GraderFunc`，通过名称区分结果、策略和效率，不新增
  grader 框架。

未采用的方案：

1. 让 `evalkit` 直接导入 `runkit`/`workflowkit`：会破坏当前独立模块边界；
2. 让 `runkit` 直接依赖 `evalkit`：会把评测语义塞进持久化核心；
3. 新增面向外部的 trace HTTP API：会解冻产品能力，并扩大敏感数据暴露面。

## 3. `evalkit` 结果契约

新增最小 DTO：

```go
type Outcome struct {
    Status    string
    OutputRef string
    ErrorCode string
    Metadata  map[string]any
}
```

`RunResult` 和 `Trial` 各增加一个 `Outcome` 字段。语义约束如下：

- `Status` 是被评测系统的领域状态，如 `succeeded`、`failed`、`waiting_approval`；
- `OutputRef` 只保存宿主持有的引用，不复制 artifact 正文；
- `ErrorCode` 只接受稳定分类，不存原始错误或 provider 响应；
- `Metadata` 只放小型、非秘密、可用于断言的结构化值；
- Harness 自身无法执行时仍使用现有 `Trial.Error`，不能伪装成正常领域失败。

Runner 必须像复制 Trace 和 Metadata 一样复制 Outcome，避免 grader 或调用方修改 Harness
返回值后污染其他 trial。

## 4. Host 适配器

在 `examples/host-api` 增加只读适配器：

```go
type hostEvalFingerprint struct {
    GitCommit          string
    Provider           string
    ModelAlias         string
    AgentDefinitionHash string
    PromptVersion      string
    VisibleToolIDs     []string
}

func buildHostEvalResult(
    ctx context.Context,
    workflowID string,
    workflows workflowkit.Store,
    runs runkit.Store,
    fingerprint hostEvalFingerprint,
) (*evalkit.RunResult, error)
```

执行过程：

1. 读取真实 `WorkflowRun`；
2. 通过 `FindByWorkflowID` 读取所有 agent run；
3. 按每个 run 的持久化 sequence 读取 `RunEvent`；
4. 将 workflow step span 与 agent event 合并为稳定排序的 `TraceStep`；
5. 汇总各 agent run 的 token、LLM call 和 tool call；
6. 从 workflow 生成 `Outcome`，从显式 fingerprint 和固化 Skill 引用生成标签。

适配器只读现有 store，不回写 workflow、run、artifact 或 eval 报告。

## 5. 规范化轨迹

### 5.1 Workflow step

每个 `workflowkit.StepRecord` 映射成：

- `Type = "workflow_step"`；
- `Name`、`Status`、`StartedAt`、`EndedAt` 保持原值；
- metadata 只允许 `attempt`、`output_ref`、`agent_run_id`、`audit_ref`、
  `approval_ref`；
- 不复制 `Error`、`WaitingReason` 或任意 step metadata 正文。

### 5.2 Agent event

每个 `runkit.RunEvent` 映射成：

- `Type = "agent_event"`；
- `Name` 使用事件类型，`Status` 根据事件类型后缀归一为 `running`、`succeeded`、
  `failed` 或 `waiting_approval`，无法归类时留空；
- `StartedAt`、`EndedAt` 都使用 `RecordedAt`；
- metadata 只允许 sequence、stage、iteration，以及源 metadata 中的 `tool`、`ref`；
- 不复制 `Message`、模型正文、工具输入、工具输出、原始错误或未知 metadata。

所有步骤按非零开始时间稳定排序；同一时间保持原始采集顺序；无时间的记录排在有时间记录
之后并保持源顺序。该规则保证相同持久化记录得到相同轨迹。

## 6. 指纹与脱敏

Trace 标签使用固定键，空值省略：

- `git.commit`；
- `provider`；
- `model.alias`；
- `agent.definition_hash`；
- `prompt.version`；
- `skill.refs`，值为排序后的完整 `name@digest`；
- `tools.visible`，值为排序、去重后的工具 ID。

适配器不接受任意标签 map，避免调用方把 API key、token、OIDC claim、绝对 Skill 根路径或
Prompt 正文混入报告。workflow ID 放在 `Trace.RunID`，不重复进入 labels。

## 7. 评测方式

第一组集成评测通过真实 SQLite-backed `NewServer` 产生 workflow、run 和 run events，再由
适配器读取，不手工制造目标 Trace。suite 至少包含三个独立 grader：

1. `outcome-contract`：断言 workflow 终态和 output ref；
2. `trajectory-policy`：断言真实步骤、事件顺序和工具边界；
3. `efficiency-budget`：断言 LLM/tool call 与 token 使用未超过任务预算。

任务 metadata 标记 `dataset_split=held_out`。当前只建立最小 held-out 回归样例，不引入
数据集文件格式、数据库或自动采样系统。以后每个真实失败可作为新的稳定 Task 加入，但
不得修改旧 Task 的成功标准来迁就实现。

## 8. 错误处理

- workflow 不存在：返回 store 的原始可分类错误；
- agent run 不存在或 events 读取失败：整个适配失败，不返回不完整成功轨迹；
- workflow 没有 agent run：允许生成只有 workflow step 的轨迹；
- workflow 失败：返回正常 `RunResult`，由 `Outcome.Status` 表达失败；
- fingerprint 字段为空：省略对应标签，不猜测版本；
- 未识别 Skill 引用结构：省略 `skill.refs`，不输出半截引用。

## 9. 验收标准

1. `evalkit` 能把 Outcome 从 Harness 无损带入 grader，并防御性复制 metadata；
2. host 集成测试从真实 SQLite workflow/run/event 生成多步骤轨迹；
3. Trace 不含输入正文、输出正文、原始错误、未知 metadata、凭证或本机信任路径；
4. Outcome、策略和效率三个 grader 分开报告并共同决定 trial；
5. 现有 Skill 安全评测、host-api 测试和 `scripts/verify-all.sh` 全部通过；
6. 不增加生产 HTTP surface、数据库迁移或新的外部依赖。

## 10. 非目标

- 模型作为裁判；
- GEPA、ACE、强化学习或 test-time search；
- 在线自动生成或晋升 Prompt/Skill；
- trace dashboard、远程上传或生产遥测；
- MCP Tasks、A2A 或多 Agent 轨迹；
- 保存或导出 artifact 正文。
