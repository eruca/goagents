# Week 18：进程内 GoAgents 双 Tag 本地发布候选

日期：2026-08-08

状态：本地 release candidate 实现与自动验证已完成；CI、独立审核和用户审核尚未完成。
本文不授权或声称已 commit、merge、push、创建 tag 或 Release。

## 范围与起点

- clean 起点、`main` 与 `origin/main` 均为
  `5bbcf73a02d05276ca6e231b0beedb53d8869646`。
- 本次 release delta 精确为 `goagent/v0.1.1` 与 `llmkit/v0.1.1`；两者必须来自同一
  reviewed commit，且 `llmkit/go.mod` 精确依赖 `goagent v0.1.1`。
- 不修改 Go 公共 API 或 runtime 行为；Week 17 已完成的 patch-compatible additive API、
  typed pre-dispatch、严格 runtime fallback 和 Provider limits 保持不变。
- 不包含 `hostruntime`、`goagents-host`、UDS/mTLS、Host OCI、SBOM、容器许可证/漏洞门禁或
  双模式 conformance。
- 既有 dirty Host worktree
  `/Users/nick/.codex/worktrees/2d15/goagents` 只把 W18-U05 spec/plan 标记为已中止；其中
  Tasks 1～7 与 Host/binary/OCI Diff 仍是未发布的未来线索，Tasks 8～10 不再执行，也不属于
  本 release candidate。
- 仓库未初始化 `.codegraph/`，本次未初始化或使用 CodeGraph。

## 实际实现

- `llmkit/go.mod` 与 `go.sum` 改为精确消费 candidate proxy 中的 `goagent v0.1.1`；根
  `go.work` 仅为本地 workspace 映射同版本目录。
- release manifest 只把 `goagent/v0.1.1`、`llmkit/v0.1.1` 标为 `release-delta`；
  `hostkit/v0.1.0`、`workflowkit/v0.1.1`、`runkit/v0.1.1` 均为既有发布。
- 新增仓库外 deterministic file proxy：只打包两个 candidate module，补入并校验根
  Apache-2.0 `LICENSE`，固定 archive 时间与排序，并覆盖调用方 Go cache。
- 新增 candidate proxy 负面门禁：相对路径、符号链接、错误许可证、调用方 cache 复用和
  不存在版本均 fail closed；consumer 精确解析两个 `v0.1.1` 且整个 module graph 无 Replace。
- 既有 Week 17 仓外 consumer 升格为精确 `v0.1.1` candidate gate；CI 只改步骤名称与未来
  入口，不增加 write permission、secret、Host 或 registry 行为。
- README 与 module 文档明确当前候选只推进进程内 `goagent/llmkit`，并继续声明本地 layout
  不构成远端发布证据。

## RED 与 GREEN

- candidate wrapper 尚不存在时，`test -x scripts/with-inprocess-candidate-proxy.sh` 以 exit `1`
  RED；实现后正向与全部负面 candidate proxy 场景通过。
- 先只把 `llmkit` 内部依赖改为 `goagent v0.1.1`，旧 manifest 以
  `llmkit internal requirements differs from the release manifest`、exit `1` RED；同步
  manifest 与 `go.work` 后正向 layout 和额外 delta/unreleased delta 负面门禁均通过。
- 仓外 consumer 从空 cache 下载两个 synthetic `v0.1.1`，测试通过并输出：
  `goagent=9aa534bd45b1dcd20986ac43801ba687891872e23772433bb3ac184ad492e446`、
  `llmkit=5a3b74c7a76899c1074d025d0aada2b3da2a2d6a176b5e4fd2235a217840372c`。
  该 synthetic proxy 是同一源码快照验证，不是远端 tag、CI 或 Release。

## 冻结验证

两个变更 module 均通过独立 candidate proxy fresh gate：

```bash
GOWORK=off go mod tidy -diff
GOWORK=off go test -count=1 ./...
GOWORK=off go test -race -count=1 ./...
GOWORK=off go vet ./...
```

冻结后以下门禁均 exit `0`：

```bash
bash scripts/verify-inprocess-candidate-proxy-test.sh
env -u OPENAI_COMPAT_BASE_URL -u OPENAI_COMPAT_MODEL -u OPENAI_COMPAT_API_KEY \
  GOWORK=off bash scripts/verify-week17-consumer.sh
bash scripts/verify-release-layout.sh
bash scripts/verify-release-layout-test.sh
env -u OPENAI_COMPAT_BASE_URL -u OPENAI_COMPAT_MODEL -u OPENAI_COMPAT_API_KEY \
  -u MEMORYKIT_POSTGRES_TEST_DSN -u MEMORYKIT_REQUIRE_POSTGRES -u LLMKIT_HOME \
  bash scripts/verify-all.sh
git diff --check
```

`verify-all` 最终输出 `goagents workspace verification passed`；它按仓库既有定义回归了
`hostkit` 单测与 `examples/host-runtime` 的进程内示例，但没有启动新的 Host 进程、UDS/mTLS、
OCI 或 Docker，也不把这些既有能力加入本次 release delta。OpenAI-compatible 示例因真实
配置不存在而明确跳过；PostgreSQL 专项与真实 Provider 调用均未执行。

## 六层状态

1. 实现：已完成本地 module metadata、candidate proxy、release layout、consumer、CI 文案和
   发布文档；未实现新的 Go runtime 或 AI Todo 集成。
2. 自动验证：两模块 tidy/test/race/vet、candidate proxy、仓外 consumer、layout 正负门禁、
   全仓 `verify-all` 与 `git diff --check` 均通过。
3. 真实运行：仓库外空 cache/no-Replace consumer、loopback/fake Provider、MCP 与仓库既有示例
   已运行；真实 Provider、PostgreSQL、Docker、OCI 与独立 Host 进程未运行。
4. CI：未执行；本地修改尚未形成远端 exact commit，不能冒充远端 CI。
5. 独立审核：未执行。
6. 用户审核：待完成；当前停在实际 Diff 审核门前。

## 远端与停止点

- 2026-08-08 只读查询确认：候选相关远端 tag 仍只有 `goagent/v0.1.0` 与
  `llmkit/v0.1.0`；远端不存在两个 `v0.1.1` tag。
- 本地 synthetic `v0.1.1` 不得写成正式 tag、Release、远端 CI 或正式 consumer。
- stage、commit、merge、push、annotated tag 与 Release 均未执行。
- 用户审核实际 Diff 并另行授权 Git/远端动作前，停止在本地 release candidate。

## 计划外事件

- 第一次 RED 记录命令在 zsh 中使用只读变量名 `status`，命令自身失败，未把它误记为功能
  RED；改用普通变量名后得到预期的 wrapper 缺失 exit `1`。
- 第一组合并冻结门禁没有输出最终完成标记，未据此宣称通过；随后逐项增加显式 PASS 标记并
  重新执行，所有门禁才作为最终证据记录。
