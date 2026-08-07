# W17-U05：module、layout 与外部 consumer 冻结门禁

日期：2026-08-06

状态：首轮独立审核结论为 `Critical 0 / Important 3 / Minor 1`。用户于 2026-08-07 批准保持
`v0.1.1` patch-compatible 的 additive 修复路线；兼容修正、外部 consumer 和全部本地门禁已通过。
第二次独立审核绑定最终内容指纹，结论为代码 `Critical 0 / Important 0`，仅发现一个历史文档
措辞 Minor；已增加 superseded 注记并形成 Week 17 总结，等待用户审核最终实际 Diff。

## 前置审核与范围

- 用户于 2026-08-06 明确“批准” W17-U04 实际 Diff，并授权进入最后一个 W17-U05。
- 本 Unit 只关闭 G04 和 Week 17 冻结门禁：`goagent`/`llmkit` 独立 module 卫生、race/vet、
  release layout、仓库外 frozen Diff consumer、完整仓库门禁和独立审核。
- 不实现 Week 18 的 `hostruntime`、wire/client/server、UDS/mTLS、Control/Tool Bridge、Host
  binary/OCI，也不创建、移动或发布 tag。
- AI Todo 保持只读；没有启动 AI Todo 数据库、Server、容器或浏览器，没有连接、探测或操作
  `127.0.0.1:54321`。
- 所有 Provider 行为继续由公开 API、fake transport 或已有本地示例验证；主动清空
  `OPENAI_COMPAT_*`，没有真实凭据、业务/用户数据或 Provider 费用。

## 实时基线与远端事实

- `git fetch origin --prune` 后，隔离 worktree 仍为 detached
  `0a24e951b92e46fe4d39ca59ffda93f42952aedc`，`main = origin/main`，ahead/behind `0/0`。
- GoAgents 最新远端 CI 仍为 run `30140268232`，精确覆盖 clean commit `0a24e951` 且为
  success；它不覆盖当前 Week 17 dirty Diff。
- 远端 `goagent/v0.1.0` 与 `llmkit/v0.1.0` annotated tag 均 peel 到原发布提交
  `8bb91d8889318f98993644c380011cb707d19e45`；本 Unit 没有移动或新建 tag。
- AI Todo `HEAD = main = origin/main =
  713246d09e92ac004cba768b26a7d98897235cb6`，ahead/behind `0/0`，worktree clean。
- GoAgents 主目录用户 memorykit 文档边界保持不变：tracked/staged/untracked 三个指纹分别为
  `beafd422a0bbf63212c4616eab2615659b3f58bfd210b36df8bed24815966a5f`、
  `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`、
  `bf8c8baed2b7dfeccd6f9fd3053e17f23734b31fa5251c4b41c13c1ed6a10a6a`；没有写入、stage、
  提交、回退或清理。
- 隔离 worktree 与主目录都没有 `.codegraph/`，未初始化或使用 CodeGraph。

## G04 正确 RED

现有 `scripts/verify-release-consumer.sh` 只验证已提交/已 tag 的 `hostkit`、`workflowkit` 和
`runkit`，不能验证尚未获准提交的 `goagent + llmkit` Week 17 Diff。先执行期望的新门禁：

```bash
GOWORK=off bash ./scripts/verify-week17-consumer.sh
```

RED 为 exit `127`，精确错误：

```text
bash: ./scripts/verify-week17-consumer.sh: No such file or directory
```

CI wiring 另有直接 RED：在 `.github/workflows/ci.yml` 搜索该脚本返回 exit `1`、无匹配，证明
即使手工门禁存在也不会被未来精确 commit CI 自动执行。

### patch 兼容修正的第二次 RED

首轮独立审核发现四个已发布 struct 被新增字段破坏后，先把 v0.1.0 公开 unkeyed literal 加入仓库外
consumer，再执行同一命令。编译以 exit `1` 失败，精确错误为：

```text
./consumer_test.go:57:32: too few values in struct literal of type ports.ChatRequest
./consumer_test.go:58:35: too few values in struct literal of type agentcore.ThinkStage
./consumer_test.go:62:2: too few values in struct literal of type agentcore.ReActConfig
./consumer_test.go:63:37: too few values in struct literal of type goagent.FallbackPolicy
FAIL example.invalid/goagents-week17-consumer [build failed]
```

随后先写增量 API 的编译期测试，确认旧实现缺少
`NewWithLimits`、`ChatWithMaxOutputTokens`、`ProviderRequestSHA256WithMaxOutputTokens`、
`NewRuntimeClient` 与 `WithRetryableErrorClasses`。这两组 RED 分别证明 public struct 兼容缺陷与所需
增量能力，未通过 `go.work`、`replace`、缓存或跳过包掩盖。

## 最小实现

- 新增 `scripts/verify-week17-consumer.sh`，只冻结当前 `goagent` 与 `llmkit` 目录。
- 脚本把两个目录打包为临时 file GOPROXY 中的 synthetic `v0.1.1-week17.0` module zip；该版本
  只存在于脚本自己创建的临时目录，不是 Git tag、commit、pseudo-version 或 Release。
- consumer 位于仓库外，使用空 `GOMODCACHE/GOCACHE` 和 `GOWORK=off`；`go.mod` 与整个 module
  graph 都必须没有 `replace`，最终必须精确解析两个 synthetic candidate version。
- consumer 通过真实公开 API 验证：OpenAI-compatible 最大输出 token `+1` 在出站前返回稳定
  typed budget error；Provider 实现公开 structural digester；无 Recorder 的 typed hook 失败后
  `RuntimeError` 为 pre-call/not-dispatched，health 与出站 HTTP 次数均为零，hook DTO 不含输入
  sentinel 且 Provider request digest 非空。
- `ChatRequest`、`ThinkStage`、`ReActConfig`、`FallbackPolicy` 恢复 v0.1.0 公开形状；
  `New`/`NewClient` 继续保留 v0.1.0 默认行为。生成上限、Provider 请求摘要、严格 runtime fallback
  和 pre-dispatch claim 均改为 additive constructor、option 或 structural interface。
- OpenAI-compatible 全局硬编码上限被移除；`NewWithLimits(Config, Limits)` 让 consumer 显式选择
  `MaxOutputTokens`、`MaxRequestBytes`、`MaxResponseBytes`，零值关闭对应边界。GoAgents 不内置
  AI Todo 的 4096/128 KiB 产品预算。
- `NewRuntimeClient` 只允许调用方显式列出的错误类别在 Provider 明确证明
  `ProviderDispatched=false` 时 fallback；已 dispatch 或未知错误 fail closed。旧 `NewClient` 仍保留
  既有 Provider failure fallback，但 context cancellation 总是终止，避免取消后继续换 Provider。
- `llmkit` 的 `GOWORK=off` 独立 module 只能依赖已发布 `goagent v0.1.0`，因此没有引用尚未发布的
  新请求类型；新增能力使用双方可独立编译的 structural method signature。需要显式 Provider 限额的
  consumer 直接用 `openaiapi.NewWithLimits` 构造 alias map，再交给 llmkit adapter。
- CI `verify` job 增加独立 `Verify Week 17 external consumer` step；当前尚未提交，故不能冒充
  已有远端 CI 覆盖。

GREEN 使用空缓存完成，并输出本次 module snapshot：

```text
goagent=9aa534bd45b1dcd20986ac43801ba687891872e23772433bb3ac184ad492e446
llmkit=6f62b743d0006aa211ecca41532c51c745142825ea10de0512b83ba235c09def
week 17 external consumer verification passed
```

最终 consumer 还覆盖：四个 v0.1.0 unkeyed literal、取消优先且 `in_flight=0`、显式
pre-dispatch transient fallback、已 dispatch transient 禁止 fallback、request limit 无出站、response
limit typed error、跨 origin redirect 阻断且目标服务零请求，以及非 2xx body sentinel 不进入错误文本。
脚本只对两个 synthetic module 设置 `GONOSUMDB`，第三方依赖继续使用 checksum database。

## Module 与 release layout 验证

### 两个变更 module 的空缓存独立门禁

`goagent` 与 `llmkit` 分别从新建的空 module/build cache 执行：

```bash
GOWORK=off go mod tidy -diff
GOWORK=off go test -count=1 ./...
GOWORK=off go test -race -count=1 ./...
GOWORK=off go vet ./...
GOWORK=off go list -m -f '{{.Path}}|{{.Version}}|{{if .Replace}}{{.Replace.Path}}{{end}}' all
```

两者全部 exit `0`。`llmkit` 独立 graph 精确解析公开 `goagent v0.1.0` 且无 replace；当前
`goagent + llmkit` Diff 的组合另由 synthetic snapshot consumer 和根 workspace 门禁证明。

兼容修正后的针对性命令也全部 exit `0`：

```bash
GOWORK=off go test -count=1 ./agentcore ./extensions/providers/openaiapi  # goagent
GOWORK=off go test -count=1 ./adapters/goagent                           # llmkit
GOWORK=off go mod tidy -diff && GOWORK=off go test -count=1 ./...         # 两个 module 分别执行
```

### layout 与脚本负面门禁

以下命令全部 exit `0`：

```bash
bash -n scripts/verify-week17-consumer.sh
bash scripts/verify-release-layout.sh
bash scripts/verify-release-layout-test.sh
bash scripts/verify-release-consumer-test.sh
```

release layout 继续描述当前已发布版本；Week 18 才能在同一 reviewed commit 上加入
`goagent/llmkit v0.1.1` 与新 `hostruntime` 的统一 release delta。W17 不提前把 synthetic
candidate 写入 tag manifest。

### 仓库完整门禁

正式命令显式移除真实 Provider/PostgreSQL 环境变量，并禁止依赖下载：

```bash
env -u OPENAI_COMPAT_BASE_URL -u OPENAI_COMPAT_MODEL -u OPENAI_COMPAT_API_KEY \
  -u MEMORYKIT_POSTGRES_TEST_DSN -u MEMORYKIT_REQUIRE_POSTGRES -u LLMKIT_HOME \
  GOPROXY=off bash ./scripts/verify-all.sh
```

结果：exit `0`，最终输出 `goagents workspace verification passed`。本地 stdio/HTTP MCP、
workflow/Host 示例和 goagent test/race/smoke 全部完成；OpenAI-compatible 示例明确打印
缺少配置而跳过，没有发起真实 Provider 调用。PostgreSQL 专项未运行，也没有启动或连接
任何 AI Todo 服务。

## 计划外事件

### U05-E01 独立审核发现 patch 兼容停止线

- 独立审核绑定 runtime/release 内容指纹
  `f1e4be451fbaba6dbfe587f0ed7e3a393edc87c109c20dcbab1eb075cec34c22`，没有修改文件。
- 结论为 `Critical 0 / Important 3 / Minor 1`，当前不能交付 Week 17 最终 Diff：
  1. `ChatRequest`、`ThinkStage`、`ReActConfig`、`FallbackPolicy` 均为 `v0.1.0` 已导出 struct；
     新增字段会让既有外部 unkeyed literal 编译失败，且 `RetryableClasses=nil` 把公开文档描述的
     默认 fallback 行为改成 fail closed。这违反正式 roadmap“破坏 public API 时停止并改走新的
     minor 版本”的门禁。
  2. OpenAI-compatible Provider 把 AI Todo 场景合同的 `4096 / 128 KiB / 128 KiB` 固定成所有
     consumer 不可配置的全局上限；旧 consumer 升级 patch 后可能被新限制拒绝，不符合通用
     GoAgents-first 边界。
  3. 仓库外 consumer 虽证明空缓存、`GOWORK=off`、无 `replace` 和精确 snapshot 解析，但只覆盖
     token `+1` 与 hook failure；尚未按正式合同组合验证取消清零、typed fallback、response limit，
     也未覆盖 request limit、redirect/body 脱敏等高风险公开边界。
- Minor：snapshot 为 synthetic module 设置 `GOSUMDB=off` 时也关闭了第三方依赖 checksum；可改成
  只对两个本地候选 module 使用 `GONOSUMDB`。
- 处理：遵守停止线，没有静默改变发布版本或公共 API。用户于 2026-08-07 选择保持
  `v0.1.1`：保留四个旧 struct 与 `NewClient` 既有语义，通过 additive API 提供新能力，并补齐
  consumer Gate。首轮指纹和审核已明确失效；本 Unit 将为最终 runtime/release 内容计算新指纹并
  重新发起独立审核。

### U05-E02 全仓错误施加 `GOWORK=off`

- 类型：验证路线偏差与既有技术债发现；不改变当前 Unit 目标。
- 事实：第一次完整门禁错误执行为 `GOWORK=off GOPROXY=off bash scripts/verify-all.sh`，在
  `runkit` setup 阶段因缺少公开 `goagent v0.1.0` 两条 `go.sum` 校验而 exit `1`。
- 根因：仓库定义的 `verify-all.sh` 对历史 module 使用根 workspace 组合；正式 Week 17 Exit
  Gate 只要求本周变更的 `goagent/llmkit` 独立 `GOWORK=off`。直接复现确认 `runkit` 默认
  workspace test 通过，独立 `tidy -diff` 只要求增加同类两条 sum。
- 处理：没有修改范围外 `runkit/go.sum`，也没有把失败隐藏为通过；将其登记为后续仓库级
  module 卫生债务，并按脚本正式 workspace 语义完整重跑至通过。
- 影响：不使 `goagent/llmkit` 空缓存、snapshot consumer 或正式完整门禁证据失效；若未来声称
  所有历史 module 均可独立 checkout 测试，该结论必须先修正并重验 runkit 等 module。

### U05-E03 临时目录 cleanup 命令被安全策略拒绝

- 第一次空缓存命令在创建进程前因 `rm -rf` cleanup 被工具策略拒绝，没有执行测试或修改仓库。
- 改用系统废纸篓处理脚本自己创建的精确 `mktemp` 目录后完整重跑，两 module 均通过。

### U05-E04 zsh 审计变量覆盖 PATH

- 2026-08-07 今日基线审计的首条命令把循环变量命名为 zsh 特殊变量 `path`，导致该进程内
  `PATH` 被联动覆盖，随后出现 `shasum: command not found`。
- 只读排障确认 `/usr/bin/shasum` 存在，consumer 脚本和机器依赖均未漂移；改用普通变量名后
  AGENTS 与内容指纹审计成功。没有为该命令错误修改产品代码或放宽哈希门禁。

### U05-E05 独立 module 边界否决共享新类型

- 第一版增量 API 草案使用新的 `ports.ChatOptions`，但 `llmkit` 在 `GOWORK=off` 下必须只依赖
  已发布 `goagent v0.1.0`，因此会在新 tag 产生前编译失败。
- 处理：没有用 workspace 或 replace 掩盖；收窄为已有 `ChatRequest` 加 `int` 参数的 structural
  method。两个 module 继续能独立 tidy/test，组合行为由 frozen snapshot consumer 验证。

### U05-E06 consumer 测试夹具纠正

- 扩展 consumer 时先后发现测试草案用了不存在的枚举、默认 profile 会过滤 local candidate，且
  默认 classifier 不会猜测任意字符串为 transient。
- 处理：只纠正测试夹具，使用真实公开枚举、满足策略的 profile，并显式注入 transient classifier；
  没有修改产品实现去迎合错误夹具。最终 consumer 从空缓存完整重跑通过。

### U05-E07 zsh 未拆分换行文件列表

- 最终格式检查第一次把 `git diff --name-only` 的换行结果保存到普通变量后直接传给 `gofmt`；
  zsh 没有按换行拆词，`gofmt` 报整串路径不存在，而命令末尾的 YAML 检查又让组合命令返回 `0`。
- 处理：没有把该 exit `0` 当成格式通过；改用 NUL 分隔的
  `git diff --name-only -z -- '*.go' | xargs -0 gofmt -d` 单独重跑，输出为空且 exit `0`，
  `git diff --check` 也为 exit `0`。

### U05-E08 指纹脚本的嵌套 shell 转义

- 最终指纹第一次重算时，多余的 `bash -c` 嵌套让 `awk` 的 `$1` 被外层提前展开；在读取文件
  内容前因 `unbound variable` 以 exit `1` 停止。
- 处理：移除多余嵌套并用普通变量名重跑成功。该失败没有修改仓库，也没有被计为有效指纹证据。

## 最终候选指纹

- runtime/release 内容范围：`.github`、`goagent`、`llmkit`、`scripts` 中相对基线的 tracked 与
  untracked 文件；清单逐项记录当前可执行位、SHA-256 和相对路径。
- 最终候选内容指纹：
  `5b70e890039505f300fd0753937ffe6f23a466e4dff73807102682263ee29a05`。
- 整体 tracked Diff 指纹：
  `c8d2ab49fda05e2c00234fa5a8c26ddc584d36bc41aa5e4983fcce1c0a46cc1`；staged Diff 仍为空。
- 冻结后再次核对 GoAgents 主目录用户修改，tracked/staged/untracked 指纹仍精确为
  `beafd422a0bbf63212c4616eab2615659b3f58bfd210b36df8bed24815966a5f`、
  `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`、
  `bf8c8baed2b7dfeccd6f9fd3053e17f23734b31fa5251c4b41c13c1ed6a10a6a`，边界未漂移。
- 本指纹只冻结第二次独立审核的输入，不代表 commit、tag、CI、Release 或用户验收。

## 第二次独立审核

- 审核员先独立重算 runtime/release 内容指纹，精确匹配
  `5b70e890039505f300fd0753937ffe6f23a466e4dff73807102682263ee29a05`；审查过程只读，
  没有修改、stage、提交或 push 文件。
- 审核员完整复核适用 AGENTS、Week 16/17/18、ADR-0004～0007、两份 runtime 合同、U01～U05、
  v0.1.0 发布 API、最终代码/测试/consumer/CI，并独立重跑两 module 的 tidy/test/race/vet、
  外部 consumer、layout/负面脚本与 Diff 检查。
- 代码结论：`Critical 0 / Important 0`。首轮三个 Important 与一个 Minor 均已关闭：四个旧
  struct/默认 fallback 兼容、显式通用 limits、外部 consumer 矩阵、第三方 checksum database。
- 唯一新 finding 为 Minor：U03 仍以现时语气描述已被 U05 替代的首次 struct/硬限额实现，可能
  误导读者。已在 U03 顶部增加最终状态注记，并在 U04 增加同类边界说明；没有改动 runtime。
- Gate verdict：W17-U05 与 Week 17 代码门禁 `PASS`，允许形成 `week-17-summary.md` 并交用户
  审核最终 Diff；不代表已获 commit、push、tag、Release 或进入 Week 18 的授权。

## 六层状态

| 层级 | 状态 | 本次证据 |
|---|---|---|
| 实现 | 已完成 | patch-compatible additive API、显式通用限额与 consumer 矩阵已实现 |
| 自动验证 | 已通过 | 两 module 空缓存 tidy/test/race/vet、layout、脚本负面门禁与完整 verify-all 均通过 |
| 真实运行 | 已通过本地门禁 | 仓库外空缓存 module proxy consumer 已组合运行全部高风险公开边界 |
| CI | 未执行 | 远端 success 只覆盖旧 `0a24e951`；新 step 等待未来提交 |
| 独立审核 | 已通过 | 最终指纹匹配，代码 `Critical 0 / Important 0`；唯一文档 Minor 已修正 |
| 用户审核 | 待审核 | 提交 W17-U05 与 Week 17 最终实际 Diff 给用户审核 |

## Diff 与停止点

- U05 实现：兼容修正后的 `goagent`/`llmkit` runtime、`scripts/verify-week17-consumer.sh`、
  `.github/workflows/ci.yml`；
- U05 证据：`docs/devlog/week-17-unit-05.md`；仅在二次独立审核关闭全部 Critical/Important 后
  形成 Week 17 summary；
- 当前 worktree 仍包含已逐 Unit 审核的 U01～U04 Diff；
- `actionlint` 本机不可用；shell syntax、真实脚本执行和 workflow 文本 wiring 已验证，远端 CI
  仍待未来精确 commit；
- stage/commit/merge/push/tag/Release/OCI：均未执行；
- 当前停止在 W17-U05/Week 17 最终实际 Diff 用户审核；不进入 Week 18。
