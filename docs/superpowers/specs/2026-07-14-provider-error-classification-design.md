# Provider 错误分类设计

**日期：** 2026-07-14

**状态：** 已确认

## 1. 问题

真实 Qwen Provider 的文本、工具调用、结构化输出和 `llmkit` 选路均已通过，
但认证失败的 outcome 只有 `error_code=provider_error`，`error_class` 为空。
根因是 OpenAI-compatible client 把非 2xx 响应格式化成普通字符串错误，
同时 `llmkit` goagent adapter 只有可选的 `ErrorClassifier`，宿主未注入时不会分类。

这使认证失败、限流、超时和服务端故障无法形成稳定的审计与 fallback 信号，
不满足 MVP 真实 Provider 验收要求。

## 2. 决策

采用类型化错误和 adapter 默认分类：

1. `goagent/extensions/providers/openaiapi` 为非 2xx 响应返回可通过 `errors.As`
   识别的错误类型，至少保留 HTTP 状态码；原有错误文本继续包含状态码和响应摘要，
   避免破坏诊断能力。
2. `llmkit/adapters/goagent` 提供默认 Provider 错误分类器。
3. `NewClient` 在调用者未提供 `ErrorClassifier` 时使用默认分类器；显式注入仍可覆盖默认行为。
4. 审计记录只保存稳定 `ErrorClass`，不保存原始 Provider 错误、响应正文、请求头或密钥。

不在每个宿主重复实现分类器，也不解析错误字符串。这样现有 host-api、host-runtime
和 llmkit 示例无需分别接线，未来新增宿主也不会因遗漏配置而失去分类。

## 3. 分类规则

按错误链和类型分类，顺序如下：

| 条件 | `ErrorClass` |
|---|---|
| `context.DeadlineExceeded` 或实现 `net.Error` 且 `Timeout()` 为真 | `timeout` |
| HTTP 401、403 | `auth_error` |
| HTTP 408 | `timeout` |
| HTTP 429 | `rate_limited` |
| HTTP 500-599 | `transient_error` |
| 其他错误，包括未识别的 4xx | `unknown` |

`context.Canceled` 不等同于超时，归为 `unknown`。本轮不扩展新的错误枚举，
也不猜测未携带状态码的网络错误是否可重试。

## 4. 边界

本次只修改：

- OpenAI-compatible 非 2xx 错误的类型表达；
- llmkit goagent adapter 的默认分类；
- 对应单元测试和真实 Qwen 认证失败验收。

不修改路由评分、fallback 次数、模型配置、审计结构、重试策略或其他 Provider。
不根据 Qwen 错误文案硬编码规则。

## 5. 测试与验收

严格按 TDD 推进：

1. 先增加 OpenAI-compatible 非 2xx 错误可被 `errors.As` 识别的失败测试；
2. 再增加 401/403、408、429、5xx、deadline、network timeout 和 unknown 的表驱动失败测试；
3. 增加 adapter 未显式配置分类器时仍记录稳定 `ErrorClass` 的失败测试；
4. 保留已有自定义分类器覆盖测试，防止默认值破坏扩展点；
5. 运行 `goagent`、`llmkit` 相关测试和仓库验证；
6. 使用 todo 中现有 Qwen endpoint 与故意无效的临时凭据发起一次真实请求，确认 outcome 为
   `provider_error/auth_error`，并检查日志、审计和仓库不含真实密钥。

真实测试配置仅通过环境变量注入，所有临时配置和审计文件在验收后删除。
