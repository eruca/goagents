# Week 17 总结：GoAgents runtime reliability 冻结候选

日期：2026-08-07

状态：W17-U01～W17-U05 已按顺序完成实现、本地门禁与独立审核；独立代码审核结论为
`Critical 0 / Important 0`。当前停在用户最终实际 Diff 审核，不代表已经提交、发布或获准进入
Week 18。

## 本周边界

- 只关闭 GoAgents G01～G04、G08 和 `goagent`/`llmkit` 独立 module 卫生，为进程内与未来
  跨进程模式提供共同可靠性基础。
- 没有修改 `/Users/nick/aicoder/aitodo`，也没有为 AI Todo 增加 GoAgents 依赖或业务特例。
- 没有实现 Week 18 的生产 `hostruntime`、wire/client/server、UDS/mTLS、Control/Tool Bridge、
  Host binary 或 OCI；`examples/host-api`、`examples/host-runtime` 与 `hostkit` 未被冒充为生产
  hostruntime。
- 没有使用真实 Provider 凭据或业务/用户数据，没有产生 Provider 费用；没有启动 AI Todo
  数据库、Server、容器或浏览器，也没有连接、探测或操作 `127.0.0.1:54321`。
- 没有 stage、commit、merge、push、tag、Release 或 OCI 操作。

## 实时基线与隔离

- GoAgents 隔离 worktree：detached
  `HEAD = main = origin/main = 0a24e951b92e46fe4d39ca59ffda93f42952aedc`，ahead/behind `0/0`。
- AI Todo：`HEAD = main = origin/main =
  713246d09e92ac004cba768b26a7d98897235cb6`，ahead/behind `0/0`，worktree clean。
- GoAgents 最新远端 CI run `30140268232` 为 success，但只覆盖 clean `0a24e951`；它不覆盖
  当前未提交 Week 17 Diff。
- GoAgents 主目录用户 memorykit 文档始终保持隔离。最终复核的 tracked/staged/untracked 指纹为：
  - `beafd422a0bbf63212c4616eab2615659b3f58bfd210b36df8bed24815966a5f`
  - `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`
  - `bf8c8baed2b7dfeccd6f9fd3053e17f23734b31fa5251c4b41c13c1ed6a10a6a`
- 隔离 worktree 与 GoAgents 主目录均没有 `.codegraph/`，没有自行初始化或使用 CodeGraph。

## Unit 交付

| Unit | 关闭内容 | 最终交付 |
|---|---|---|
| W17-U01 | G04 起点与失败矩阵 | 在 `GOWORK=off` 下复现 `llmkit` 全包 setup failure；只向 `llmkit/go.sum` 增加公开 `goagent v0.1.0` 的两条校验 |
| W17-U02 | G01 | Provider 返回后用独立 2 秒 cleanup context 精确收口 health/outcome；cancel/timeout 不泄漏 `in_flight`，post-call failure 不 fallback |
| W17-U03 | G02/G03 首次实现 | 稳定错误阶段/类别、取消优先、Provider token/字节/响应/redirect/脱敏边界；U05 后续把公共 API 收敛为 patch-compatible additive 形态 |
| W17-U04 | G08 | typed pre-dispatch hook、安全 route metadata、精确 Provider request digest、host-owned 严格递增 call index；hook 未明确成功前不开始 health/Provider/fallback |
| W17-U05 | G04 冻结门禁 | 修正 v0.1.1 兼容与通用限额，扩展仓库外 consumer，加入 CI step，完成 module/layout/全仓门禁和独立复审 |

## 最终 runtime 合同

### G01：取消与收口

- Provider `Begin` 成功后，无论请求 context 是否取消，health/outcome 都在
  `context.WithTimeout(context.WithoutCancel(requestCtx), 2*time.Second)` 中有界收口。
- Provider 忽略取消并晚到成功时，完整响应被丢弃，调用返回请求取消错误；`in_flight` 回到 0。
- health 或 audit 的 post-call failure 是已 dispatch 错误，立即返回且禁止第二 Provider。

### G02：类型化错误与 fallback

- `RuntimeError` 暴露稳定 `Stage`、`Class`、`ProviderDispatched`，底层 cause 只供
  `errors.Is/errors.As` 使用；公开 `Error()` 不拼接 Provider 原文。
- `NewClient` 保留 v0.1.0 的既有 Provider failure fallback 行为，但取消始终终止。
- 新的 `NewRuntimeClient` 为 fail-closed 路径：只有调用方显式 allowlist 的稳定类别，且错误明确
  证明 `ProviderDispatched=false`，才允许 fallback；已 dispatch 或未知状态不重试。

### G03：Provider 安全边界

- `ChatRequest`、`ThinkStage` 与 `ReActConfig` 保持 v0.1.0 公开形状；生成上限通过
  `WithMaxOutputTokens`、`ChatWithMaxOutputTokens` 和 additive constructor 下传。
- `openaiapi.New` 保留旧 consumer 的无全局限额行为；`NewWithLimits(Config, Limits)` 显式启用
  output token、request bytes、response bytes 边界，零值关闭对应限制。
- GoAgents 不硬编码 AI Todo 的 `4096/128 KiB/128 KiB` 产品预算。
- request 超限在出站前失败；response 使用 `Content-Length` 与 `LimitReader(limit+1)` 有界读取；
  跨 origin redirect 被阻断，同 origin redirect 保留。
- usage/choice/tool-call/JSON 错误均有稳定类型；非 2xx 原始 body 不进入公开错误、日志或 audit。

### G08：pre-dispatch claim 边界

```text
route select
  -> 精确 Provider request SHA-256
  -> host metadata / provider_call_index
  -> typed pre-dispatch hook
  -> health Begin
  -> Provider call
  -> bounded outcome cleanup
```

- hook DTO 只含白名单 route/attempt/provider class 与两种 SHA-256，不含 ChatRequest、Prompt、
  message 或 credential。
- Provider digester、host metadata、正数且严格递增的 call index、非零 digest 或 hook 任一失败，
  都在 health 与 Provider 前 fail closed。
- call index 由 host 提供；GoAgents 不硬编码 AI Todo 的 `1..4`、连续编号或业务 claim 规则。

## patch 兼容与 module 边界

- 首轮独立审核发现 `ChatRequest`、`ThinkStage`、`ReActConfig`、`FallbackPolicy` 新增字段会破坏
  v0.1.0 unkeyed literal，且旧 `NewClient` 默认 fallback 被反转。用户选择继续以 v0.1.1 为目标，
  保留旧 struct 形状与默认行为，通过 additive API 承载严格路径。
- 仓库外 consumer 编译四个 v0.1.0 unkeyed literal，直接证明源码兼容。
- `goagent` core 没有依赖 `llmkit`；`llmkit` 在 `GOWORK=off` 下仍只依赖正式
  `goagent v0.1.0`，不引用尚未发布的新 Go 类型。新增组合能力通过 structural method
  signature 保持两个 module 可独立编译。
- `llmkit/go.mod` 没有 `replace`；`go.sum` 只增加 `goagent v0.1.0` module/content 两条校验。

## 最终验证

### 独立 module

`goagent` 与 `llmkit` 分别使用新建的空 module/build cache 执行以下命令，全部 exit `0`：

```bash
GOWORK=off go mod tidy -diff
GOWORK=off go test -count=1 ./...
GOWORK=off go test -race -count=1 ./...
GOWORK=off go vet ./...
GOWORK=off go list -m -f '{{.Path}}|{{.Version}}|{{if .Replace}}{{.Replace.Path}}{{end}}' all
```

`llmkit` 独立 graph 精确解析 `github.com/eruca/goagents/goagent v0.1.0`，无 replace。

### 仓库外 frozen Diff consumer

```bash
GOWORK=off bash ./scripts/verify-week17-consumer.sh
```

最终从仓库外、空 `GOMODCACHE/GOCACHE`、无 `replace` 的 synthetic file proxy 运行通过；精确解析
两个 `v0.1.1-week17.0` candidate。该版本只用于未提交 Diff 验证，不是 tag、Release 或正式版本。

```text
goagent=9aa534bd45b1dcd20986ac43801ba687891872e23772433bb3ac184ad492e446
llmkit=6f62b743d0006aa211ecca41532c51c745142825ea10de0512b83ba235c09def
week 17 external consumer verification passed
```

consumer 覆盖四个旧 unkeyed literal、token `+1`、取消清零、严格 typed fallback、request/response
limit、跨 origin redirect、非 2xx body 脱敏和 hook fail-closed。只对两个 synthetic module 使用
`GONOSUMDB`，第三方依赖继续使用 checksum database。

### layout、完整仓库与静态检查

以下门禁全部 exit `0`：

```bash
bash -n scripts/verify-week17-consumer.sh
bash scripts/verify-release-layout.sh
bash scripts/verify-release-layout-test.sh
bash scripts/verify-release-consumer-test.sh
git diff --check
```

`.github/workflows/ci.yml` 可由 YAML parser 读取；本机没有 `actionlint`，不能把未执行项冒充通过。

完整仓库门禁显式移除 Provider/PostgreSQL 环境变量并禁止下载：

```bash
env -u OPENAI_COMPAT_BASE_URL -u OPENAI_COMPAT_MODEL -u OPENAI_COMPAT_API_KEY \
  -u MEMORYKIT_POSTGRES_TEST_DSN -u MEMORYKIT_REQUIRE_POSTGRES -u LLMKIT_HOME \
  GOPROXY=off bash ./scripts/verify-all.sh
```

结果 exit `0`，最终输出 `goagents workspace verification passed`。OpenAI-compatible 真实示例因
未配置环境而明确跳过；没有运行 PostgreSQL 专项或真实 Provider。

## 冻结指纹与独立审核

- 最终 runtime/release 内容指纹：
  `5b70e890039505f300fd0753937ffe6f23a466e4dff73807102682263ee29a05`。
- 独立审核员先重算并精确匹配该指纹，再完整核对正式文档、v0.1.0 API、代码、测试、CI 和
  consumer；审查过程没有修改文件。
- 最终结论：`Critical 0 / Important 0 / Minor 1`。唯一 Minor 是 U03 历史快照未醒目标明已被
  U05 取代；U03/U04 顶部现已增加最终状态注记，不影响 runtime 指纹。
- Gate verdict：W17-U05 / Week 17 独立代码审核 `PASS`。

## 未完成与失效条件

- 用户尚未审核本次最终实际 Diff；没有 commit，因此还不存在可供 Week 18 固定的精确 clean
  commit。
- 远端 CI 尚未覆盖当前 Diff。只有未来获准提交并运行 exact commit CI 后，才能把 CI 层标为
  通过。
- `actionlint` 本机不可用；workflow 仅完成 YAML parse、脚本实际运行与完整本地门禁。
- 历史 `runkit` 独立 `GOWORK=off` 仍缺两条 `go.sum`，已登记为范围外 module 卫生债务；当前
  Week 17 正式 Gate 只要求本周变更的 `goagent`/`llmkit`，没有顺手修改 `runkit`。
- 任何 `.github`、`goagent`、`llmkit` 或 `scripts` 文件变化都会使上述 runtime/release 指纹、
  snapshot consumer、完整门禁和独立审核失效，必须重新执行。
- Week 18 仍未开始；必须先取得用户对本总结和最终 Diff 的明确审核结论，再决定 Git 与后续
  Unit 动作。
