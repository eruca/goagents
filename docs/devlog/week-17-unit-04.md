# W17-U04：typed pre-dispatch hook 与安全 route metadata

日期：2026-08-06

状态：实现与 Unit 自动验证已完成；用户已于 2026-08-06 明确“批准”实际 Diff，并授权进入
W17-U05。本文只记录 W17-U04，不授权或实现 Week 18、tag、Release、OCI 或 push。

> 最终状态注记（2026-08-07）：G08 hook、digest 和 host-owned index 语义保持有效；本文中
> 对 U03 首次 token/字节 API 的引用已由 U05 patch-compatible additive API 与显式通用 limits
> 取代。当前公开 API 与最终门禁以 `week-17-unit-05.md` 和 `week-17-summary.md` 为准。

## 前置审核与范围

- 用户于 2026-08-06 以“继续”审核通过 W17-U03 实际 Diff，并授权进入 W17-U04。
- 本 Unit 只关闭 G08：为 `llmkit` 增加通用 typed pre-dispatch hook，并让 hook 所需的安全
  route metadata 不再依赖可选 JSONL Recorder。
- `provider_call_index` 的生成及 AI Todo 范围 `1..4` 属于未来 host/Gateway policy；GoAgents
  逐 Provider attempt 接收 host 给出的正数，并只校验严格递增，没有推导连续值或硬编码上限。
- G04 module/release 卫生、仓库外冻结 Diff consumer、独立审核和 Week 总结仍属于 W17-U05。
- 未实现 `hostruntime`、UDS/mTLS、Control/Tool Bridge、Host binary/OCI，也没有把
  `examples/host-api` 或 `hostkit` 当作生产 Host runtime。
- AI Todo 仅作为只读合同来源；没有修改其依赖、代码或文档，没有启动数据库、Server、容器
  或浏览器，也没有连接、探测或操作 `127.0.0.1:54321`。
- 所有 Provider 验证均使用 `httptest` 或内存 fake；没有读取真实凭据、发送业务/用户数据或
  产生 Provider 费用。

## 实时起点、最终边界与 module 事实

- 实施路径：`/Users/nick/.codex/worktrees/4149/goagents`，detached
  `0a24e951b92e46fe4d39ca59ffda93f42952aedc`；`main = origin/main`，ahead/behind `0/0`。
- U04 启动时未暂存任何文件；已有未提交内容仅为用户审核通过的 U01～U03 Diff。
- `goagent` 与 `llmkit` 是独立 module。`llmkit/go.mod` 继续精确依赖已发布的
  `goagent v0.1.0`，没有 `replace`；因此 `llmkit` 的 `GOWORK=off` 测试使用公开结构接口与
  fake 验证新 hook，不假装能直接 import 尚未发布的本地 `goagent` 新符号。根 workspace 另行
  验证本地 OpenAI-compatible client 与 adapter 的组合；双 tag 仓库外 consumer 留给 U05。
- AI Todo `HEAD = main = origin/main =
  713246d09e92ac004cba768b26a7d98897235cb6`，ahead/behind `0/0`，worktree clean。
- GoAgents 主目录 `HEAD = main = origin/main =
  0a24e951b92e46fe4d39ca59ffda93f42952aedc`；用户 tracked diff SHA-256 仍为
  `beafd422a0bbf63212c4616eab2615659b3f58bfd210b36df8bed24815966a5f`，staged diff 仍为空
  文件的 `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`，六个 untracked
  文档内容清单 SHA-256 仍为
  `bf8c8baed2b7dfeccd6f9fd3053e17f23734b31fa5251c4b41c13c1ed6a10a6a`。
- 本 Unit 没有在 GoAgents 主目录写文件、stage、提交、回退或清理用户的 memorykit 文档。
- 隔离 worktree 与 GoAgents 主目录都没有 `.codegraph/`，未初始化或使用 CodeGraph。

## 合同与根因

正式合同要求每次 LLM 调用严格按以下顺序执行：

```text
route select
  -> 安全 route metadata + 精确 Provider request bytes SHA-256
  -> 单调 provider_call_index
  -> typed pre-dispatch claim
  -> claim 明确成功
  -> health Begin
  -> Provider.Chat
  -> 有界 cleanup 收口
```

原 adapter 只有配置 JSONL Recorder 时才构造 route trace，随后直接进入 health `Begin` 和
`Provider.Chat`。这既无法让进程内与未来 Host 模式共享同一通用 claim 边界，也会在 claim
失败时把“未获准调用”误记为 Provider health 失败。原 Provider client 还没有公开“最终实际
出站 JSON bytes”的 digest 能力；由 adapter 重复序列化会产生双实现漂移，无法证明 digest
对应实际发送正文。

## 正确 RED

先写直接测试，再补生产接口与实现。主要定向命令为：

```bash
cd goagent
GOWORK=off go test -count=1 ./extensions/providers/openaiapi \
  -run TestClientProviderRequestSHA256MatchesDispatchedBody -v

cd ../llmkit
GOWORK=off go test -count=1 ./adapters/goagent \
  -run 'TestClient(PreDispatch|RunsPreDispatch)' -v
```

RED 逐项证明：

- OpenAI-compatible client 最初没有 `ProviderRequestSHA256`，测试在编译期失败；
- typed hook 与安全 DTO 最初不存在，测试在编译期失败；
- 暂不处理 hook error 时，claim 失败仍返回本地 Provider 成功响应；
- Provider 未实现 request digester 时，原路径仍调用 Provider；
- host metadata 缺少正数 call index 或 Run digest 时，原路径仍调用 Provider；
- Provider 返回零 digest 时，原路径仍调用 Provider；
- 初始 fallback 实现未增加 attempt offset 时，实际 indexes 为 `[7 7]`，期望 `[7 8]`；
- 后续合同自审发现由 GoAgents 计算连续 offset 仍侵犯 host 索引所有权：host 提供 `[7,11]`
  时实际 hook 得到 `[7,8]`，host 重复提供 `[7,7]` 时原实现仍调用第二 Provider；
- digest 构造返回 request-too-large 时，忽略该错误会错误落为 `configuration_error`；
- route metadata provider 在一次 fallback 链中被调用两次，测试期望只分配一次；
- 兼容性自审移除向既有公开 struct 增字段的初稿后，目标新增构造器与类型尚不存在，测试再次
  在编译期失败，证明最终实现来自明确的 additive API RED。

所有行为 RED 都通过恢复最小生产逻辑转绿，没有用 `go.work`、`replace`、缓存命中、跳包或
固定业务样例掩盖问题。

## 最小实现

### 精确 Provider request digest

- OpenAI-compatible client 抽出单一私有 `providerRequestPayload`；`Chat` 与公开
  `ProviderRequestSHA256` 共用同一序列化、token 和 `128 KiB` 请求门禁。
- digest 是实际 dispatch JSON bytes 的 SHA-256，不含 API key/header；直接测试捕获
  `httptest` 收到的正文并逐字节验证 hash。
- `llmkit` 只依赖结构接口 `ProviderRequestDigester`。启用 hook 而 Provider 未声明该能力时，
  在 config/pre-dispatch 阶段 fail closed，Provider 调用为零。

### additive typed hook

- 保持既有 `Config`、`RouteMetadata` 和 `NewClient(Config)` 的公开形状与行为不变，避免 patch
  版本出现 unkeyed struct literal 源码破坏。
- 新增 `NewClientWithPreDispatch(Config, PreDispatchConfig)`；hook 输入仅公开 allowlist 字段：
  `RouteTrace`、`ProviderCallIndex`、`Attempt`、`ProviderClass`、Run request SHA-256 与 Provider
  request SHA-256，不公开 `ChatRequest`、Prompt、messages 或 credential 字段。
- route metadata provider 每个 `Chat` 只调用一次；dispatch metadata provider 在精确 Provider
  digest 构造成功后逐 attempt 调用。GoAgents 直接使用 host 给出的 `[7,11]` 并要求后值严格
  大于前值；重复/倒退会在 hook、health 和第二 Provider 前 fail closed。这同时避免整数 offset
  溢出，并证明通用库未硬编码 AI Todo 的 `1..4` 或连续分配策略。
- route trace 在 Recorder 或 hook 任一启用时构造；因此无 Recorder 时 hook 仍能取得安全 route
  metadata。可选 Recorder 先写，typed claim 后执行，避免非严格 JSONL 失败消耗一次 claim。
- hook 失败、digester 缺失、digest 失败/为零、host metadata 无效都返回未 dispatch 的 typed
  error，并在 health `Begin`、Provider 与 fallback 之前立即停止。
- hook 明确成功后才允许 health `Begin` 与 Provider；后续 outcome cleanup 继续复用 U02 的
  有界收口，不在本 Unit 复制生命周期逻辑。

## 直接回归覆盖

- hook 无 Recorder 时仍按 `digest -> hook -> health_begin -> provider -> health_outcome` 执行；
- hook DTO 的 JSON 不含测试 prompt sentinel，route、attempt、class 与两种 digest 精确匹配；
- hook 失败时即使 fallback policy 允许 transient，两个 Provider 调用总数、health 事件均为零；
- digester 缺失、host metadata 无效、Provider digest 为零均 fail closed；
- digest 构造错误保留稳定 `request_too_large` 分类和底层 cause，且不触发 hook/health/Provider；
- fallback 使用 host 逐 attempt 提供的非连续 `[7,11]`；重复 index 被拒绝，route metadata
  provider 每个 `Chat` 仅调用一次，dispatch metadata provider 每 attempt 调用一次；
- 真实 `MemoryHealthStore` 参与顺序/计数测试，仅外部 Provider 与 hook 边界使用 fake。

## 冻结验证

实现冻结后的新鲜验证结果：

- U04 两组定向测试：通过；
- `goagent/: GOWORK=off go test -count=1 ./...`：通过；
- `llmkit/: GOWORK=off go test -count=1 ./...`：通过；
- 根 workspace：`go test -count=1 ./goagent/... ./llmkit/...`：通过；
- 两个 module 分别执行 `GOWORK=off go test -race -count=1 ./...`：通过；
- 两个 module 分别执行 `GOWORK=off go vet ./...`：通过；
- 两个 module 分别执行 `GOWORK=off go mod tidy -diff`：通过，无输出、无 module 漂移；
- `git diff --check`：通过；
- 真实 Provider/Server/数据库/浏览器：不适用且未获授权；
- CI、release layout、仓库外双 tag consumer 和独立审核：本 Unit 不执行，留给 W17-U05。

## 计划事件与兼容性结论

实现过程中发生两次目标不变的兼容性/合同返工：

- 初稿把 hook 字段加进既有公开 `Config` 与 `RouteMetadata`。自审发现这会破坏外部 unkeyed
  struct literal 后，先把测试改为期望 additive constructor 并得到编译 RED，再删除新增字段、
  改用独立 `PreDispatchConfig` 与 `NewClientWithPreDispatch`；
- 初稿只取一次 dispatch metadata，再由 GoAgents 用 fallback offset 生成后续 index。回到正式
  合同后确认索引生成属于 host/Gateway；增加非连续与重复 index RED 后，改为逐 attempt 获取
  host index、只校验正数与严格递增，并把 metadata 分配放到 Provider digest 成功之后。

产品终点、Unit 范围和验证标准均未改变；没有发现需要停止并重算版本的 Critical/Important
架构冲突。

## Diff 与审核停止点

- U04 生产代码：`goagent/extensions/providers/openaiapi/client.go`、
  `llmkit/adapters/goagent/client.go`；
- U04 直接测试：对应两个 module 的 `client_test.go`；
- 证据：`docs/devlog/week-17-unit-04.md`；W17-U03 devlog 只更新用户审核状态；
- 当前 worktree 还包含已逐 Unit 审核的 U01～U03 Diff，未把它们误报为 U04 新改动；
- stage/commit/merge/push/tag/Release/OCI：均未执行；
- 用户已审核通过 W17-U04 实际 Diff，后续只按正式顺序进入 W17-U05。
