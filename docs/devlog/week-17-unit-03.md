# W17-U03：类型化错误、fail-closed fallback 与 Provider 边界

日期：2026-08-06

状态：实现与 Unit 自动验证已完成；用户已于 2026-08-06 以“继续”审核通过实际 Diff，并
授权进入 W17-U04。本文只记录 W17-U03，不授权或实现 W17-U05、Week 18、tag、Release、
OCI 或 push。

> 最终状态注记（2026-08-07）：本文的“最小实现”和“冻结验证”记录 U03 当时通过审核的
> 历史快照。U05 独立审核随后发现公开 struct、默认 fallback 和全局硬限额不满足 v0.1.1
> patch 兼容；最终实现已改为保留 v0.1.0 形状/默认行为的 additive API 与显式通用 limits。
> 当前事实与最终门禁以 `week-17-unit-05.md` 和 `week-17-summary.md` 为准。

## 前置审核与范围

- 用户于 2026-08-06 以“继续”审核通过 W17-U02 实际 Diff，并授权进入 W17-U03。
- 本 Unit 只关闭 G02/G03：稳定错误类别与阶段、fail-closed fallback、最大生成 token、
  Provider request/response 字节上限、redirect 与响应错误脱敏。
- G08 typed pre-dispatch hook 和无 Recorder 时的安全 route metadata 属于 W17-U04，本 Unit
  没有提前实现。
- module 发布卫生、release layout、仓库外冻结 Diff consumer 和独立审核仍属于 W17-U05。
- AI Todo 仅作为只读合同来源；没有修改其依赖、代码或文档，没有启动数据库、Server、容器
  或浏览器，也没有连接、探测或操作 `127.0.0.1:54321`。
- 所有 Provider 验证均使用 `httptest` 或内存 fake；没有读取真实凭据、发送业务/用户数据或
  产生 Provider 费用。

## 实时起点与 module 边界

- 实施路径：`/Users/nick/.codex/worktrees/4149/goagents`，detached
  `0a24e951b92e46fe4d39ca59ffda93f42952aedc`。
- U03 启动时未暂存任何文件；已有未提交内容仅为用户审核通过的 U01/U02 Diff。
- `goagent` 与 `llmkit` 是独立 module。`llmkit/go.mod` 继续精确依赖已发布的
  `goagent v0.1.0`，没有 `replace`；因此 `llmkit` 的 `GOWORK=off` 测试不能直接 import 本次
  尚未发布的 `goagent` 新符号。
- 为保持该边界，`llmkit` 通过公开结构能力
  `interface { ProviderErrorClass() string }` 和
  `interface { ProviderDispatched() bool }` 读取 Provider 错误元数据，并对 class 做 allowlist；
  没有解析错误字符串或依赖本机 workspace。
- 隔离 worktree 与 GoAgents 主目录都没有 `.codegraph/`，未初始化或使用 CodeGraph。

最终边界复核仍为：

- AI Todo `HEAD = main = origin/main =
  713246d09e92ac004cba768b26a7d98897235cb6`，ahead/behind `0/0`，worktree clean；
- GoAgents 主目录 `HEAD = main = origin/main =
  0a24e951b92e46fe4d39ca59ffda93f42952aedc`；用户 tracked diff SHA-256 仍为
  `beafd422a0bbf63212c4616eab2615659b3f58bfd210b36df8bed24815966a5f`，staged diff 仍为空
  文件的 `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`，六个 untracked
  文档内容清单 SHA-256 仍为
  `bf8c8baed2b7dfeccd6f9fd3053e17f23734b31fa5251c4b41c13c1ed6a10a6a`；
- 本 Unit 没有在 GoAgents 主目录写文件、stage、提交、回退或清理这些用户文档。

## G02：根因、RED 与最小实现

### 根因

原 adapter 的 Provider 错误会无条件删除当前候选并进入下一候选；`FallbackPolicy` 只有次数，
没有稳定类别门禁，也无法判断出站请求是否已经 dispatch。`context.Canceled` 被归为
`unknown`，route/config/pre-call/provider/post-call 失败也没有可供 host 使用的阶段类型。
这会把取消、认证、配置、预算、结果未知和 post-call 失败混成同一种可 fallback 错误，可能
制造重复出站和费用。

### 正确 RED

先增加类型与行为测试，再修改生产实现。执行目录为 `llmkit/`：

```bash
GOWORK=off go test -count=1 ./adapters/goagent \
  -run 'Test(ClientDiscardsLateResponseAfterRequestCancellation|ClientDoesNotFallbackForProviderErrorsByDefault|ClientReturnsTyped|ClientFallsBackOnlyForExplicitPreDispatchClass|ClientDoesNotFallbackForDispatchedErrorEvenWhenClassAllowed|DefaultErrorClassifier)' \
  -v
```

RED 证明：

- Provider 忽略取消并晚到成功时，adapter 会把完整成功响应返回；
- cancel、timeout、401、429、503 和 unknown 默认都会调用第二 Provider；
- canceled classifier 实际为 `unknown`；
- 新的稳定阶段、类别、dispatch 状态和 host retry class API 尚不存在，测试先在编译期失败。

### 最小实现

- 增加稳定 `canceled`、configuration、budget、post-call、request/response too large、usage
  missing、redirect blocked、invalid response 等 `ErrorClass`。
- 增加公开 `RuntimeError`，只暴露 `Stage`、`Class`、`ProviderDispatched`；`Error()` 只输出稳定
  stage/class，底层 cause 仅通过 `errors.Is/errors.As` 保留，不把原始正文拼入公开字符串。
- route/config/pre-call/provider/post-call 五个阶段均返回可类型判断的 `RuntimeError`。
- 请求 context 在 Provider 返回时已取消或超时，则丢弃晚到响应，以请求取消结果收口。
- fallback 只有同时满足以下条件才允许：host 在 `RetryableClasses` 明确允许该稳定类别，且
  Provider 类型明确证明 `ProviderDispatched()==false`。未知错误和已 dispatch 错误一律按已
  dispatch 处理；默认 policy 不 fallback。
- post-call health/audit 失败继续立即返回 typed post-call error，不进入第二 Provider。

定向 GREEN 通过：late success 被丢弃；取消后的 health `InFlight=0`；cancel、timeout、auth、
429、5xx、unknown 默认第二 Provider 调用数均为 0；只有显式允许的 pre-dispatch transient
错误会进入下一候选。

## G03：根因、RED 与最小实现

### 根因

- `ports.ChatRequest` 没有最大生成 token，Agent budget 只在 Provider 返回后检查累计 usage；
- OpenAI-compatible client 未发送 `max_tokens`，请求序列化后没有字节门禁；
- 响应使用无界 `io.ReadAll`，缺失 `usage` 会静默得到零值；
- client 默认跟随跨 origin redirect；
- 非 2xx 原始 body 被保存在 `ResponseError.Body` 并拼入 `Error()`。

### 正确 RED

执行目录为 `goagent/`：

```bash
GOWORK=off go test -count=1 ./agentcore \
  -run TestAgentPassesPerCallMaxOutputTokensToLLM -v
GOWORK=off go test -count=1 ./extensions/providers/openaiapi \
  -run 'TestClient(DoesNotExposeNon2xxResponseBody|RejectsResponseWithoutUsage|RejectsCrossOriginRedirect|AllowsSameOriginRedirect|EnforcesMaxOutputTokens|EnforcesRequestByteLimit|EnforcesResponseByteLimit|ReturnsErrorForMalformedJSON|RejectsResponseWithoutChoices|ReturnsErrorForToolCallWithoutID)' \
  -v
```

RED 证明：

- token 字段、独立的单次生成 option 与稳定 Provider 错误 API 尚不存在，相关测试先在编译期
  失败；
- 原实现的非 2xx `Error()` 包含敏感 body，usage 缺失仍成功，跨 origin redirect 被跟随；
- request/response `128 KiB + 1` 均被接受，`MaxOutputTokens=4097` 仍发生出站；
- 第一次 redirect 修正过严并拒绝同 origin redirect；保留同 origin、只拒绝跨 origin后转绿。

`llmkit/` 另有一个配置边界 RED：

```bash
GOWORK=off go test -count=1 ./adapters/goagent \
  -run TestOpenAICompatibleProvidersFromConfigRejectsMissingConfiguredAPIKey -v
```

实际失败为 `error = nil, want missing API key error`，证明显式配置的 secret 环境变量解析为空时
仍会构造可调用 Provider。

### 最小实现

- `ports.ChatRequest` 增加 `MaxOutputTokens`；新增 `WithMaxOutputTokens` 经 `ThinkStage` 下传，
  OpenAI-compatible JSON 使用 `max_tokens`。
- 单次生成上限与既有 `Budget.MaxOutputTokens`（Run 累计 usage 上限）保持独立；测试使用累计
  `8000` 与单次 `4096`，证明不会把两种预算误当成同一字段。
- Provider 默认和硬上限均为 `4096`：`0` 规范化为 `4096`，`4096` 允许，`4097` 在 dispatch
  前以稳定 `budget_exceeded` 拒绝。
- 序列化后的 Provider request 上限为 `128 KiB`；边界值允许，`+1` 在构造出站请求前以
  `request_too_large` 拒绝。
- Provider HTTP response 上限为 `128 KiB`；先检查 `Content-Length`，再用
  `io.LimitReader(limit+1)` 有界读取，边界值允许，`+1` 以 `response_too_large` 拒绝。
- 非 2xx 不读取或保存 response body；`ResponseError.Error()` 只输出 status。保留 `Body`
  字段仅为 patch 版本源码兼容，生产路径永不填充或渲染。
- client 复制传入的 `http.Client`，禁止跨 origin redirect，同时保留同 origin redirect 和调用方
  自定义 redirect policy；未配置自定义 policy 时保持标准 10 跳上限。
- usage 缺失、JSON 解码失败、choices 缺失和非法 tool call 都返回稳定、无正文的 Provider
  错误；llmkit classifier 只接受 allowlist 中的结构化 class。
- 若账号显式配置 `APIKeyEnv` 但值为空，Provider 构造在 config/pre-dispatch 阶段 fail closed；
  未配置 key 的本地兼容端点保持可用。

## 冻结验证

实现冻结后的新鲜验证结果：

- `goagent/: GOWORK=off go test -count=1 ./...`：通过；
- `llmkit/: GOWORK=off go test -count=1 ./...`：通过；独立解析的仍是
  `goagent v0.1.0`，没有本地 `replace`；
- 根 workspace：`go test -count=1 ./goagent/... ./llmkit/...`：通过，当前两个本地 module 可
  组合；这不冒充 U05 的仓库外固定 tag consumer；
- 两个 module 分别执行 `GOWORK=off go test -race -count=1 ./...`：通过；
- 两个 module 分别执行 `GOWORK=off go vet ./...`：通过；
- 两个 module 分别执行 `GOWORK=off go mod tidy -diff`：通过，无输出、无 module 漂移；
- `git diff --check`：通过；
- 真实 Provider/Server/数据库/浏览器：不适用且未获授权；
- CI、release layout、仓库外冻结 Diff consumer 与独立审核：本 Unit 不执行，留给 W17-U05。

## 未关闭风险与后续边界

- 当前没有 typed pre-dispatch hook；无 Recorder 时也不会生成给 hook 使用的安全 route
  metadata。这是 W17-U04/G08 的唯一下一步，不在本 Unit 提前实现。
- 当前 Diff 尚未形成 tag，`llmkit` 独立测试只能验证对 `goagent v0.1.0` 的兼容；本地 workspace
  验证当前组合。发布后的双 tag clean consumer 必须等 W17-U05 冻结验证，不能在 U03 冒充。
- AI Todo 仍未引入任何 GoAgents 依赖；生产 hostruntime、UDS/mTLS、Control/Tool Bridge、
  Host binary/OCI 全部仍属于 Week 18。

## Diff 与审核停止点

- G02：`llmkit/llmkit/audit.go`、`llmkit/adapters/goagent/{client,errors,providers}.go` 及直接测试。
- G03：`goagent/ports/llm.go`、`goagent/agentcore/{agent,think_stage,finalize_stage}.go`、
  `goagent/extensions/providers/openaiapi/{client,errors}.go` 及直接测试。
- U03 证据：`docs/devlog/week-17-unit-03.md`；U01/U02 已审核 Diff 保持不变。
- stage/commit/merge/push/tag/Release/OCI：均未执行。
- 完成 Unit 验证并展示实际 Diff 后已停止；用户审核通过后才进入 W17-U04。
