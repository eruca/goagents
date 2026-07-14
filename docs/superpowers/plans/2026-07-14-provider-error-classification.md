# Provider 错误分类 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 OpenAI-compatible Provider 的认证、限流、超时和服务端错误在未显式配置分类器时也产生稳定的 `llmkit.ErrorClass`。

**Architecture:** `goagent/openaiapi` 用类型化 `ResponseError` 保留 HTTP 状态码；`llmkit/adapters/goagent` 通过 `errors.As` 和标准库错误类型完成默认分类。`NewClient` 仅在调用者未提供分类器时使用默认值，现有覆盖扩展点保持不变。

**Tech Stack:** Go、`net/http`、`errors.As`、`context`、`net.Error`、Go table-driven tests、真实 Qwen OpenAI-compatible endpoint。

## Global Constraints

- 不解析 Provider 错误字符串，不根据 Qwen 文案硬编码。
- 不修改路由评分、fallback 次数、模型配置、审计结构或重试策略。
- outcome 只记录稳定错误分类，不记录响应正文、请求头或密钥。
- 严格执行 RED → GREEN；每项生产代码之前必须先看到对应测试按预期失败。
- 真实配置只通过环境变量读取，临时配置和审计在验收后删除。

---

### Task 1: 暴露 OpenAI-compatible 类型化响应错误

**Files:**
- Create: `goagent/extensions/providers/openaiapi/errors.go`
- Modify: `goagent/extensions/providers/openaiapi/client.go:79-82`
- Test: `goagent/extensions/providers/openaiapi/client_test.go`

**Interfaces:**
- Produces: `openaiapi.ResponseError`，包含 `StatusCode int` 和 `Body string`，并实现 `error`。
- Preserves: 原有错误文本仍包含 `openai-compatible request failed`、状态码和响应摘要。

- [ ] **Step 1: 写入失败测试**

在 `TestClientReturnsErrorForNon2xxResponse` 中增加类型断言：

```go
var responseErr *ResponseError
if !errors.As(err, &responseErr) {
	t.Fatalf("err type = %T, want *ResponseError", err)
}
if responseErr.StatusCode != http.StatusBadRequest || !strings.Contains(responseErr.Body, "bad request") {
	t.Fatalf("ResponseError = %+v", responseErr)
}
```

- [ ] **Step 2: 验证 RED**

Run: `cd goagent && go test ./extensions/providers/openaiapi -run TestClientReturnsErrorForNon2xxResponse -count=1`

Expected: FAIL，原因是 `ResponseError` 尚未定义。

- [ ] **Step 3: 实现最小类型化错误**

创建 `errors.go`：

```go
package openaiapi

import "fmt"

// ResponseError preserves the HTTP status needed by host-side error policy.
type ResponseError struct {
	StatusCode int
	Body       string
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("openai-compatible request failed: status %d: %s", e.StatusCode, e.Body)
}
```

并让 `Client.Chat` 的非 2xx 分支返回：

```go
return nil, &ResponseError{StatusCode: resp.StatusCode, Body: string(data)}
```

- [ ] **Step 4: 验证 GREEN**

Run: `cd goagent && go test ./extensions/providers/openaiapi -count=1`

Expected: PASS。

- [ ] **Step 5: 提交独立契约改动**

```bash
git add goagent/extensions/providers/openaiapi/errors.go \
  goagent/extensions/providers/openaiapi/client.go \
  goagent/extensions/providers/openaiapi/client_test.go
git commit -m "feat(goagent): 暴露 Provider 响应错误类型"
```

### Task 2: 为 llmkit adapter 增加默认分类

**Files:**
- Create: `llmkit/adapters/goagent/errors.go`
- Create: `llmkit/adapters/goagent/errors_test.go`
- Modify: `llmkit/adapters/goagent/client.go:82-103`
- Test: `llmkit/adapters/goagent/client_test.go`

**Interfaces:**
- Consumes: `*openaiapi.ResponseError`。
- Produces: `DefaultErrorClassifier(error) llmkit.ErrorClass`。
- Preserves: `Config.ErrorClassifier` 的显式覆盖优先级。

- [ ] **Step 1: 写入表驱动失败测试**

在 `errors_test.go` 覆盖 deadline、network timeout、401、403、408、429、500、503、400、普通错误和 `context.Canceled`：

```go
for _, test := range tests {
	t.Run(test.name, func(t *testing.T) {
		if got := DefaultErrorClassifier(test.err); got != test.want {
			t.Fatalf("DefaultErrorClassifier() = %q, want %q", got, test.want)
		}
	})
}
```

在 `client_test.go` 增加未显式配置分类器时，401 Provider 错误被记录为 `auth_error` 的测试。

- [ ] **Step 2: 验证 RED**

Run: `cd llmkit && go test ./adapters/goagent -run 'TestDefaultErrorClassifier|TestClientUsesDefaultErrorClassifier' -count=1`

Expected: FAIL，原因是默认分类器尚未定义，且 `NewClient` 尚未提供默认值。

- [ ] **Step 3: 实现最小分类器**

创建 `errors.go`，规则固定为：

```go
func DefaultErrorClassifier(err error) llmkit.ErrorClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return llmkit.ErrorClassTimeout
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return llmkit.ErrorClassTimeout
	}
	var responseErr *openaiapi.ResponseError
	if errors.As(err, &responseErr) {
		switch responseErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return llmkit.ErrorClassAuth
		case http.StatusRequestTimeout:
			return llmkit.ErrorClassTimeout
		case http.StatusTooManyRequests:
			return llmkit.ErrorClassRateLimited
		}
		if responseErr.StatusCode >= 500 && responseErr.StatusCode <= 599 {
			return llmkit.ErrorClassTransient
		}
	}
	return llmkit.ErrorClassUnknown
}
```

在 `NewClient` 中先计算：

```go
errorClassifier := config.ErrorClassifier
if errorClassifier == nil {
	errorClassifier = DefaultErrorClassifier
}
```

并写入 `Client.errorClassifier`。

- [ ] **Step 4: 验证 GREEN 与覆盖语义**

Run: `cd llmkit && go test ./adapters/goagent -count=1`

Expected: PASS；已有 `TestClientRecordsClassifiedProviderErrors` 继续证明显式分类器可覆盖默认值。

- [ ] **Step 5: 提交 adapter 改动**

```bash
git add llmkit/adapters/goagent/errors.go \
  llmkit/adapters/goagent/errors_test.go \
  llmkit/adapters/goagent/client.go \
  llmkit/adapters/goagent/client_test.go
git commit -m "feat(llmkit): 默认分类 Provider 错误"
```

### Task 3: 回归与真实 Qwen 失败链验收

**Files:**
- Temporary only: `.tmp-qwen-error-classification/config.yaml`
- Verify: `scripts/verify-all.sh`

**Interfaces:**
- Consumes: todo `.env` 中的 `LLM_BASE_URL`、`LLM_MODEL`，以及故意无效的临时 API key。
- Produces: 临时 JSONL outcome，其中 `error_code=provider_error`、`error_class=auth_error`；验收后不保留文件。

- [ ] **Step 1: 运行模块回归**

```bash
cd goagent && go test ./... -count=1 && go vet ./...
cd ../llmkit && go test ./... -count=1 && go vet ./...
```

Expected: 全部退出码为 0。

- [ ] **Step 2: 运行全仓验证**

Run: `cd .. && bash ./scripts/verify-all.sh`

Expected: 所有 workspace 模块测试、vet 和示例检查退出码为 0。

- [ ] **Step 3: 运行真实认证失败验收**

通过 `apply_patch` 创建临时 `LLMKIT_HOME/config.yaml`：base URL 和模型名取自 todo 当前 `.env`，
配置只写 `api_key_env: OPENAI_COMPAT_API_KEY`。设置 `OPENAI_COMPAT_API_KEY=invalid-auth-smoke` 后运行：

```bash
cd llmkit
LLMKIT_HOME="$PWD/../.tmp-qwen-error-classification" \
OPENAI_COMPAT_API_KEY=invalid-auth-smoke \
go run ./examples/goagent-routing
```

Expected: 进程因 Provider 认证失败返回非 0；最后一条 outcome 满足：

```json
{"success":false,"error_code":"provider_error","error_class":"auth_error"}
```

- [ ] **Step 4: 检查秘密与清理**

从 todo `.env` 读取真实 key 后仅执行精确匹配检查，不打印 key；确认临时目录和仓库均不含真实 key。
使用 `apply_patch` 删除临时配置和审计文件，删除空目录，再运行：

```bash
git diff --check
git status --short --branch
```

Expected: 没有秘密命中；临时目录不存在；Git 工作区干净。
