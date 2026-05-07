# llmkit host contract follow-ups design

## 结论

这 4 项都应该作为 host-facing contract 继续推进，而不是直接塞进
`goagent` core，也不应把 `llmkit` 变成生产级 gateway。

当前策略：

- `llmkit` 保持小核心：定义可复用类型、路由策略、审计 schema 和 adapter hook。
- `examples/host-api` 继续作为 canonical host composition，展示一种接法。
- 生产部署需要的共享状态、预算扣减、错误恢复和 HTTP patch 语义由 host 拥有。

本文把 4 个后续方向固定为可执行设计，后续实现时按独立小提交推进。

## 1. HealthStore contract 文档化

实现状态：已拆出正式 contract 文档 `docs/llmkit-healthstore-contract.md`。

### 当前状态

`llmkit.HealthStore` 当前接口很小：

```go
type HealthStore interface {
    Begin(context.Context, Candidate) error
    RecordOutcome(context.Context, TaskOutcome) error
    Snapshot() ProviderHealthSnapshot
}
```

`MemoryHealthStore` 已覆盖单进程内的：

- in-flight 计数
- provider availability
- quota exhausted
- failure streak
- cooldown

它足够支撑 host-api 示例，但多副本服务不会共享状态。

### 边界判断

不应在 `llmkit` 中直接引入 Redis、SQLite、Postgres 或租户概念。
原因是 health 的一致性级别取决于 host：

- 单机 CLI 或桌面 app 只需要内存态。
- 单服务进程可以继续使用 `MemoryHealthStore`。
- 多副本 HTTP 服务需要共享 health store。
- 有强配额要求的生产系统还需要 provider/account 级 lease 或 token bucket。

### Contract 要求

后续文档应明确 `HealthStore` 实现必须满足：

- `Begin` 在 provider call 前调用，用于增加 in-flight 或拒绝不可用资源。
- `RecordOutcome` 在 provider call 后调用，用于减少 in-flight、更新失败 streak 和 cooldown。
- `Snapshot` 返回路由用的只读快照；路由策略只依赖 snapshot，不直接依赖 store 类型。
- store 不保存 prompt、response、API key 或 header。
- key 使用 `account_alias|model_alias|provider`，与 `ProviderHealthKey` 一致。

### 可替换实现建议

本节作为设计背景保留；正式 contract 以 `docs/llmkit-healthstore-contract.md`
为准，不新增 core 依赖。

当前 contract 包含：

- `MemoryHealthStore` 适用场景
- shared store 必须实现的语义
- Redis/DB 实现伪代码
- 多副本部署注意事项
- host-api 如何注入自定义 `HealthStore`

### 验收标准

- 文档能让 host 作者实现一个自定义 shared health store，而不需要读 `MemoryHealthStore` 源码。
- 不修改 `HealthStore` 接口。
- 不引入任何外部 store 依赖。

## 2. 错误类型化 fallback

实现状态：已完成第一步 contract 落地。`llmkit` 已有 `ErrorClass`，
`TaskOutcome` 已记录 `error_class`，goagent adapter 已支持可选 `ErrorClassifier`。
完整 fallback rule engine 尚未实现。

### 当前状态

goagent adapter 现在有显式 attempt 数量控制：

```go
FallbackPolicy{MaxAttempts: 2}
```

provider 失败后会：

1. 记录失败 outcome，当前 `error_code` 是通用 `provider_error`。
2. 移除当前候选。
3. 重新让 `RoutePolicy` 选择下一候选。
4. 直到成功、无候选或达到 `MaxAttempts`。

这已经比隐式 fallback 更清楚。当前新增的 `error_class` 能表达错误类别，但还没有
根据不同错误类别执行“换账号、换模型、升级模型或停止”的完整 rule engine。

### 边界判断

错误分类应该进入 `llmkit` contract，但具体从 provider error 转换成错误类别的逻辑应由
adapter 或 host 提供。原因是不同 provider 的错误形态不同，不能靠 core 猜测。

### 已落地 contract

已新增一层可选分类，不破坏现有 `ErrorCode`：

```go
type ErrorClass string

const (
    ErrorClassTransient          ErrorClass = "transient_error"
    ErrorClassRateLimited        ErrorClass = "rate_limited"
    ErrorClassTimeout            ErrorClass = "timeout"
    ErrorClassInvalidJSON        ErrorClass = "invalid_json"
    ErrorClassToolCallInvalid    ErrorClass = "tool_call_invalid"
    ErrorClassCapabilityMismatch ErrorClass = "capability_mismatch"
    ErrorClassPolicyBlocked      ErrorClass = "policy_blocked"
    ErrorClassAuth               ErrorClass = "auth_error"
    ErrorClassUnknown            ErrorClass = "unknown"
)
```

`TaskOutcome` 已扩展为：

```go
ErrorCode  string     `json:"error_code,omitempty"`
ErrorClass ErrorClass `json:"error_class,omitempty"`
```

adapter 已增加可选 classifier：

```go
type ErrorClassifier func(error) llmkit.ErrorClass
```

`FallbackPolicy` 后续仍可扩展为：

```go
type FallbackPolicy struct {
    MaxAttempts int
    Rules []FallbackRule
}

type FallbackRule struct {
    ErrorClass ErrorClass
    Action FallbackAction
    MaxAttempts int
}
```

第一版 action 只定义 contract，不急着复杂实现：

```text
retry_same_model_next_account
try_next_candidate
prefer_lower_latency
prefer_more_capable
stop
```

### 当前兼容策略

保持兼容：

- 没有 classifier 时，错误继续记为 `provider_error`，fallback 行为不变。
- `MaxAttempts <= 0` 继续表示尝试所有剩余 eligible candidates。

后续 rule engine 可采用：

- `policy_blocked` 默认 stop。
- `auth_error` 默认 stop，并建议 host 标记账号不可用。
- `rate_limited` 默认 try_next_candidate，同时 health store 进入 cooldown。
- `timeout` 在 urgent task 下 prefer_lower_latency，否则 try_next_candidate。
- `invalid_json` 和 `tool_call_invalid` 默认 prefer_more_capable。

### 审计要求

当前 outcome audit 已能回答：

- 失败属于什么 `error_class`

后续 rule engine 还应回答：

- 哪条 fallback rule 命中
- 为什么继续 fallback 或停止
- fallback 后选择了哪个候选

### 验收标准

- 没有配置 classifier 时，现有测试和行为不变。
- 配置 classifier 后，`outcomes.jsonl` 能记录 `error_class`。
- route audit 展示 fallback action 或停止原因留给后续 rule engine。

## 3. 项目/账号级预算

### 当前状态

当前已有单任务硬约束：

```go
TaskProfile.MaxEstimatedCents
```

候选模型如果有 `EstimatedCents`，或者历史 stats 提供了 `AvgEstimatedCents`，超过任务预算会在评分前被过滤。

还没有：

- project monthly budget
- account budget
- model family budget
- 预算扣减
- reserved budget
- 余额不足时的错误 contract

### 边界判断

项目/账号级预算不应进入 `llmkit` core 的路由算法内部。它属于 host 成本治理，通常需要：

- project id / tenant id
- billing period
- usage ledger
- refund / correction
- admin override
- alerting
- provider invoice reconciliation

这些都不是 `llmkit` small core 的职责。

### 建议 contract

`llmkit` 只定义一个可选预算 gate，让 host 注入：

```go
type BudgetDecision struct {
    Allowed bool
    Reason string
    MaxEstimatedCents int
    RemainingCents int
}

type BudgetGate interface {
    CheckRouteBudget(context.Context, TaskProfile, Candidate) (BudgetDecision, error)
    ReserveRouteBudget(context.Context, RouteTrace, int) (BudgetReservation, error)
    CommitRouteBudget(context.Context, BudgetReservation, TaskOutcome) error
    ReleaseRouteBudget(context.Context, BudgetReservation, string) error
}
```

但第一步不建议马上实现完整接口。更稳妥的落地顺序：

1. 文档化 host budget gate 设计。
2. 在 host-api 示例里保留当前 per-task budget。
3. 真实 host 项目需要预算时，在 host 层先做 pre-route profile 降级或拒绝。
4. 等至少一个真实 host 需要 reserve/commit，再把最小接口抽到 `llmkit`。

### 推荐数据流

```text
request
  -> host identifies project/account/user
  -> host checks project budget
  -> host sets TaskProfile.MaxEstimatedCents
  -> llmkit filters candidates by per-task budget
  -> route selected
  -> host records estimated usage reservation
  -> provider outcome recorded
  -> host commits actual or estimated cost
```

### 预算失败语义

预算失败不是 provider failure，不应该进入普通 fallback。

建议错误语义：

```text
budget_exceeded
budget_unknown
budget_reservation_failed
budget_commit_failed
```

其中：

- `budget_exceeded`：host 可直接拒绝或降低 profile。
- `budget_unknown`：host 可选择保守拒绝或使用本地免费候选。
- `budget_reservation_failed`：不调用 provider。
- `budget_commit_failed`：provider 已调用，必须进入 host ledger 修复队列。

### 验收标准

- 文档明确 per-task budget 与 project/account budget 的边界。
- 不把 project id、tenant id、billing period 加入 `TaskProfile`。
- host 可以在不改 `llmkit` core 的情况下先实现预算治理。

## 4. task_profile bool override 语义

实现状态：已在 `examples/host-api` 落地。请求侧使用 pointer patch 语义；响应侧继续返回最终
生效的普通 `TaskProfile`。

### 原问题

host-api 之前的 `task_profile` override 使用 Go bool 字段：

```go
NeedsReasoning bool `json:"needs_reasoning,omitempty"`
NeedsTools bool `json:"needs_tools,omitempty"`
NeedsJSON bool `json:"needs_json,omitempty"`
NeedsLongContext bool `json:"needs_long_context,omitempty"`
```

问题是 HTTP JSON 里：

```json
{}
```

和：

```json
{"needs_reasoning": false}
```

在旧 Go struct 中都会变成 `false`。因此当 preset 默认 `needs_reasoning=true` 时，传一个只想改
`task_type` 的 `task_profile` 会把 `needs_reasoning` 意外改成 `false`。

这个限制已经通过 request-only pointer patch 修正。

### 边界判断

这个问题属于 host-api HTTP patch contract，不属于 `llmkit.TaskProfile` core。
`llmkit.TaskProfile` 作为最终生效画像，继续使用普通 bool 是合理的。

### 已采用方案

host-api 使用 request-only patch 类型：

```go
type taskProfilePatchRequest struct {
    TaskType *string `json:"task_type,omitempty"`
    Complexity *string `json:"complexity,omitempty"`
    Latency *string `json:"latency,omitempty"`
    FailureCost *string `json:"failure_cost,omitempty"`
    Privacy *string `json:"privacy,omitempty"`
    MaxEstimatedCents *int `json:"max_estimated_cents,omitempty"`
    NeedsReasoning *bool `json:"needs_reasoning,omitempty"`
    NeedsTools *bool `json:"needs_tools,omitempty"`
    NeedsJSON *bool `json:"needs_json,omitempty"`
    NeedsLongContext *bool `json:"needs_long_context,omitempty"`
}
```

应用语义：

- 字段缺失：继承 preset 或默认 profile。
- 字段存在且为 `false`：显式覆盖为 false。
- 字段存在且为 `true`：显式覆盖为 true。
- string 字段存在但为空：视为 invalid request，而不是静默忽略。
- `max_estimated_cents` 存在且小于 0：invalid request。
- `max_estimated_cents` 存在且为 0：invalid request，避免“清除预算”和“未传”语义混淆。

### 兼容路径

当前实现保留了这些兼容边界：

1. 保留响应里的 `task_profile` shape 不变。
2. 请求解析改为 pointer patch。
3. 对旧客户端来说，传 true/false 的 JSON 仍然兼容。
4. 行为变化只影响“字段缺失”的情况：缺失将继承 preset，而不是变成 false。
5. 更新 OpenAPI，说明 `task_profile` 是 patch semantics。

### 测试覆盖

已补 3 个 host-api 测试：

- `high_success` preset + `task_profile: {"task_type":"x"}` 仍保留 `needs_reasoning=true`。
- `high_success` preset + `task_profile: {"needs_reasoning":false}` 显式关掉 reasoning。
- `task_profile: {"complexity":""}` 返回 `invalid_task_profile` 或 `invalid_request`，不要静默忽略。

### 验收标准

- HTTP patch 语义清晰。
- response 继续返回最终生效 `TaskProfile`。
- `llmkit.TaskProfile` 不需要引入 pointer bool。

## 后续实现顺序

本文件已经完成 4 项设计收口。后续进入实现时，建议按这个顺序做，且每项独立提交：

1. `docs(llmkit): 固化 HealthStore 接入合同`
   - 已完成：`docs/llmkit-healthstore-contract.md`

2. `feat(llmkit): 增加错误类型化 fallback contract`
   - 已完成第一步：新增 `ErrorClass`、可选 classifier 和 audit 字段
   - 后续可继续实现 fallback rule engine

3. `docs(llmkit): 明确 host 预算治理边界`
   - 若没有真实 host 预算实现，先不抽 `BudgetGate`
   - 继续保留当前 per-task budget

4. `feat(host-api): 修正 task profile patch 语义`
   - 已完成

剩余实现重点是前 3 项是否需要继续从 contract 进入代码；第 4 项不再重复排期。

## 当前不做的事

- 不把 Redis/DB health store 放进 `llmkit`。
- 不把 typed fallback 的全部执行引擎一次做完。
- 不在 `TaskProfile` 加 project id、tenant id 或 billing period。
- 不把 budget ledger 放进 `llmkit`。
- 不把 host-api 升级成生产 gateway。
