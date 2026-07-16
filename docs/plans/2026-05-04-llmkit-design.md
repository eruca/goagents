# llmkit 设计文档

## 背景

`goagents` 当前采用多 Go module 的 workspace 结构。`goagent` 是 small-core runtime，只通过 `ports.LLMClient` 调用模型，不应该直接拥有多账号、价格、并发、失败历史、项目级策略或持久化审计。

`llmkit` 作为新的 sibling module，用于解决 host 项目中“一个任务应该选择哪个 LLM 和哪个账号，才能在可接受成本与延迟下提高成功概率”的问题。

## 目标

- 为 host 项目提供可配置、可审计的 LLM 路由能力。
- 支持本地免费模型、云端高级模型、多账号 API key、并发限制、失败降权、价格和延迟权衡。
- 让简单低风险任务优先使用本地或低成本模型，让复杂任务和高失败成本任务直接使用成功率更高的高级模型。
- 保持 `goagent` core 不依赖 `llmkit`，由 host 项目选择是否引入。

## 非目标

- 不在第一版实现复杂强化学习、自动多模型辩论或投票。
- 不让 LLM 自己决定路由。
- 不在审计文件中保存明文 API key。
- 不把 durable store 固定为某一种数据库。第一版优先使用 host workspace 下的文件。

## 模块边界

建议新增模块：

```text
llmkit/
  go.mod              # module github.com/eruca/goagents/llmkit
  llmkit/             # 核心路由、配置、审计 contract
  adapters/goagent/   # 可选：把 Router 暴露为 goagent ports.LLMClient
```

依赖方向：

```text
goagent     -> 不依赖 llmkit
llmkit      -> 可在 adapter 子包中依赖 goagent/ports
host app    -> 组合 goagent + llmkit
```

核心包保持尽量独立；如果为了直接接入 `goagent`，可以在 `adapters/goagent` 中实现 `ports.LLMClient`，避免核心路由包被 `goagent` 类型绑定。

## LLMKIT_HOME

`llmkit` 使用环境变量确定 host 项目的工作目录：

```bash
LLMKIT_HOME=/path/to/project/.llmkit
```

该目录保存配置、审计、统计缓存和并发状态：

```text
$LLMKIT_HOME/
  config.yaml
  route-events.jsonl
  outcomes.jsonl
  model-stats.json
  locks/
  cache/
```

加载规则：

1. 如果设置 `LLMKIT_HOME`，使用该目录。
2. 如果没有设置，但当前目录存在 `.llmkit/`，开发模式下可使用当前目录的 `.llmkit/`。
3. 生产环境建议必须显式设置 `LLMKIT_HOME`。

API key 不写入审计目录。配置中只保存环境变量名或账号别名：

```yaml
accounts:
  local_default:
    provider: local
    base_url: http://127.0.0.1:1234/v1

  cloud_advanced_a:
    provider: openai_compatible
    base_url: https://api.example.com/v1
    api_key_env: CLOUD_ADVANCED_API_KEY_A
```

审计记录只保存 `account_alias`、`model_alias` 和 provider 类型，不保存明文 key。

## 任务画像

host 项目调用 `llmkit` 时，应提供或推导 `TaskProfile`：

```text
task_type: summarize / extract_json / chat_reply / tool_agent / code_review / planning
complexity: simple / medium / hard
latency_requirement: none / normal / urgent
failure_cost: low / medium / high
needs_reasoning: true / false
needs_tools: true / false
needs_json: true / false
needs_long_context: true / false
privacy_level: local_preferred / cloud_allowed / local_only
```

如果 host 不提供 `TaskProfile`，第一版可以使用默认画像，但应在 `RouteTrace` 中标记 `profile_source=default`。

## 模型与账号属性

模型配置包含静态属性：

```text
price_class: free / low / medium / high
is_local: true / false
capability_level: simple / balanced / advanced
supports_tools: true / false
supports_json: true / false
context_window_class: short / medium / long
max_concurrency
latency_class: slow / normal / fast
```

运行时统计包含动态属性：

```text
recent_failure_count
recent_failure_rate
recent_latency_ms
current_concurrency
rate_limited_until
task_success_rate_by_profile
json_validation_success_rate
tool_call_success_rate
```

## 路由原则

第一版采用“先过滤，再排序，再 fallback”的确定性策略。

过滤：

- 不满足 `local_only` 的云模型直接排除。
- 不支持 JSON 的模型不能承接 `needs_json` 任务。
- 不支持 tool calling 的模型不能承接 `needs_tools` 任务。
- 超过并发上限或处于 rate limit 冷却期的账号暂时排除。

排序：

- 简单任务、无延迟要求：本地免费模型优先。
- 简单任务、有延迟要求：低延迟模型优先，同时考虑本地当前并发。
- 中等任务：本地强模型可用则优先，否则使用普通云模型。
- 复杂任务或高失败成本任务：高级 LLM 优先，不做便宜模型试探。
- 结构化 JSON 和工具调用任务：优先历史成功率高的模型。
- 最近失败次数高、延迟异常或当前并发高的候选降权。

可表达为评分，但第一版应避免过度数学化：

```text
score =
  capability_match
  + historical_success
  + local_preference
  - cost_penalty
  - latency_penalty
  - failure_penalty
  - concurrency_penalty
```

## Fallback

失败分类决定 fallback 行为：

```text
transient_error       -> 同模型换账号重试
rate_limited          -> 账号冷却，换账号或换模型
timeout               -> 根据任务延迟要求换低延迟模型或高级模型
invalid_json          -> 选择 JSON 历史成功率更高的模型
tool_call_invalid     -> 选择 tool calling 更稳定的模型
capability_mismatch   -> 升级模型
policy_blocked        -> 不自动 fallback，交给 host
```

高失败成本任务应减少试探次数，直接选择更可靠模型。

## 审计与历史学习

`llmkit` 应定义审计 contract，但 durable store 由 host workspace 拥有。

`RouteTrace` 由 `llmkit` 自动写入 `route-events.jsonl`：

```text
route_id
timestamp
task_profile
candidate_models
selected_model
selected_account_alias
route_reason
score_breakdown
fallback_chain
latency_ms
usage_tokens
cost_estimate
error_class
validation_result
```

`TaskOutcome` 由 host 写入 `outcomes.jsonl`：

```text
route_id
task_id
outcome: success / failure / partial / unknown
success_signal: schema_passed / tests_passed / human_accepted / tool_completed
failure_reason
```

后续选择依据来自 `RouteTrace + TaskOutcome` 聚合后的 `model-stats.json`。没有 host outcome，只能说明 provider 调用成功，不能说明任务真正成功。

## 接入 goagent

host 项目可以这样接入：

```text
router := llmkit.NewRouter(...)
agent := agentcore.NewAgent(
  agentcore.WithLLM(goagentadapter.NewClient(router, taskProfileProvider)),
)
```

`goagent` 仍然只看到一个 `ports.LLMClient`。`llmkit` 负责在内部选择本地模型、云模型、账号和 fallback。

## 第一版验收标准

- host 能通过 `LLMKIT_HOME` 加载配置。
- 简单任务在无延迟要求时优先选择本地模型。
- 复杂任务或高失败成本任务优先选择高级 LLM。
- 并发已满、rate limit 或最近失败过多的账号会被降权或跳过。
- 每次路由都生成可审计的 `RouteTrace`。
- host 可以回写 `TaskOutcome`，并生成基础模型统计。
- `goagent` 不引入对 `llmkit` 的依赖。
