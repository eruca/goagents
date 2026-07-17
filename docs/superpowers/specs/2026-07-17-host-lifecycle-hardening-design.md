# v0.1.1 Host 生命周期硬化设计

**日期：** 2026-07-17

**状态：** 已批准，待实施

## 1. 决策摘要

本阶段关闭 v0.1.0 最终验收保留的两个 P2：

1. Host 启动配置、初始化和监听失败不再 `panic`，改为单行结构化错误与稳定退出码；
2. Host 支持 `SIGINT/SIGTERM`，先停止接单并等待活动 workflow，超时或收到第二次信号后
   取消执行、做有界状态收口并退出。

生命周期协调抽成新的独立模块：

```text
github.com/eruca/goagents/hostkit
```

`hostkit` 首次发布版本为 `v0.1.0`；本次产品里程碑仍称为 GoAgents Host 生命周期
`v0.1.1`。`hostkit` 只负责通用生命周期状态机，不依赖 `workflowkit`、HTTP、OIDC、
SQLite 或 Host API 业务类型。`examples/host-api` 通过一个薄 adapter 把现有组件接入。

## 2. 当前基线与问题

当前 `examples/host-api/main.go` 有三处直接 `panic`：

- Host 配置加载失败；
- `NewServer` 初始化失败；
- `http.ListenAndServe` 返回错误。

这会把 Go stack 写入进程输出，也无法让 supervisor 按稳定退出码区分配置、监听和运行期
故障。

当前 queued worker 的同一个 context 同时控制“是否继续 claim”和“当前 workflow 是否
继续执行”。直接取消该 context 会立即取消当前 workflow，不能表达“停止接单，但允许当前
执行完成”的 drain 语义。approval janitor 也只有启动入口，没有可等待的停止边界。

现有真实进程测试可以发送 `os.Interrupt`，但 stop helper 接受进程正常退出或五秒后被 kill，
没有断言 signal-aware drain、退出码或持久化收口。当前重启恢复已经覆盖安全持久化边界，
但没有覆盖停机时仍有活动 workflow 的场景。

现有 SQLite lease 能避免同时 claim，并能恢复 pending 或已过期的 pending lease；它不能
让本地 SQLite 更新与外部系统副作用成为原子事务。若外部副作用已经提交而本地 checkpoint
尚未完成，自动重放可能重复不可逆操作。

## 3. 目标

- 配置、初始化、监听和运行期错误有稳定分类、单行 JSON 输出与固定退出码；
- 第一次 `SIGINT/SIGTERM` 立即停止 HTTP 接单、queued claim 和 janitor 新一轮扫描；
- 已经接收的 HTTP workflow/审批操作和当前 queued workflow 最多获得 30 秒完成时间；
- drain 成功后正常关闭资源并退出 `0`；
- drain 超时或第二次信号跳过剩余等待，进入最多 5 秒的强制 cleanup；
- 强制 cleanup 将尚未进入稳定状态的 workflow 收口为
  `failed` / `host_shutdown_timeout`，并清理其 lease；
- 强制退出后的 workflow 不自动重放，由 operator 核对外部副作用后决定是否 requeue；
- 用真实 Host 二进制证明停止接单、优雅退出、强制退出和同 runtime home 重启恢复；
- 用测试专属子进程和 loopback 服务证明外部副作用边界，不扩大生产 Host 工具面。

## 4. 非目标

- 不修改 `goagent` 核心；
- 不实现多 worker、跨进程调度、worker crash supervision 或 stuck recovery；
- 不承诺任意崩溃点 exactly-once；
- 不实现 Skill 动态激活；
- 不新增健康检查、dashboard、长期 metrics、生产网关或部署 supervisor；
- 不顺带升级 GitHub Actions；
- 不顺带给 Apache-2.0 门禁增加固定 SHA-256；
- 不创建 tag、GitHub Release，不推送远端。

GitHub Actions 与 LICENSE SHA-256 是独立维护项，应在本阶段之外按语义单独提交。

## 5. `hostkit` 模块边界

### 5.1 公开生命周期协议

`hostkit` 只使用 Go 标准库，公开一个最小 `Service` 协议：

```go
type Service interface {
    Start(context.Context) error
    Done() <-chan error
    Drain(context.Context) error
    ForceStop(context.Context) error
    Close(context.Context) error
}

type Options struct {
    DrainTimeout   time.Duration
    CleanupTimeout time.Duration
}

type Result struct {
    ExitCode int
    Code     string
    Err      error
}

func Run(
    ctx context.Context,
    service Service,
    interrupts <-chan struct{},
    options Options,
) Result

func WriteError(w io.Writer, result Result) error
```

语义固定为：

- `Start` 完成同步启动并在服务可以进入运行态后返回；传入 context 只约束启动过程，
  `Service` 不得把它直接当作执行期 root context；
- `Done` 返回稳定的只读 channel。服务从运行态退出时必须向该 channel 发送且只发送一次
  terminal result；预期 drain 可发送 `nil`，运行期异常发送具体错误；
- `Drain` 停止 intake 并等待活动操作完成；
- `ForceStop` 取消活动操作并执行宿主拥有的业务收口；
- `Close` 在没有可继续安全访问资源的活动操作后释放资源，并且必须可安全地最多执行一次。

`hostkit.Run(ctx, service, interrupts, options)` 负责调用这些方法。`interrupts` 是只读的通用
事件 channel；Host CLI 把 `SIGINT/SIGTERM` 转换成事件。`hostkit` 不直接依赖 Unix signal
常量，因此单元测试可以确定性驱动第一次和第二次中断。

`hostkit` 还提供受限的 `Code` 常量和带 cause 的 `Failure` 构造函数。退出码由 `Code`
唯一映射，调用方不能为同一个 code 指定另一个退出码。`Run` 使用 `errors.As` 读取
`Failure`；未分类错误统一降级为 `internal_error`，禁止解析 `err.Error()` 文本决定分类。
`WriteError` 只负责把非零 `Result` 编码为固定 JSON schema。

不增加 participant 注册表、依赖图、插件系统、自动启动排序或任意 callback 列表。一个
`hostkit.Run` 只协调一个 `Service`，具体 Host 在 adapter 内自行组合其组件。

### 5.2 生命周期状态机

```text
starting
  -> running
  -> draining
  -> stopped

draining --drain timeout--> forcing
draining --second interrupt--> forcing
running  --unexpected Done error--> forcing
forcing  --cleanup completed--> stopped
forcing  --cleanup timeout--> stopped
```

关键规则：

- 第一次中断只触发一次 drain；
- `hostkit` 在 goroutine 中执行 `Drain`，同时继续监听第二次中断和 drain deadline；
- drain 成功后使用 cleanup timeout 调用 `Close`；
- drain 超时或第二次中断取消 drain context，再调用 `ForceStop`；
- `ForceStop` 与 `Close` 共用 5 秒 cleanup 总预算，不各自重新获得 5 秒；
- `Start` 已经被调用后，即使启动失败，`hostkit` 也要用 cleanup budget 调用一次 `Close`，
  让部分完成的初始化有明确回收机会；
- 所有阶段转换与 close 都必须并发安全且最多发生一次；
- cleanup timeout 时不得并发关闭仍可能被活动 goroutine 使用的 store。此时记录
  `shutdown_cleanup_timeout` 并退出，由进程边界终止剩余 goroutine。

正常强制路径必须完成 workflow 失败收口；`shutdown_cleanup_timeout` 表示连该收口都未能
在预算内完成，不能宣称持久化状态已经安全收敛。

## 6. Host API adapter

### 6.1 独立控制面

`examples/host-api` 增加 `hostAPIService`，内部至少维护：

- `intakeCtx`：queued worker 是否继续 claim、janitor 是否继续开始新扫描；
- `executionCtx`：已接收的 HTTP workflow、审批操作和当前 queued workflow；
- HTTP listener 与 `http.Server`；
- queued worker 和 janitor 的 done 信号；
- Host execution registry；
- workflow store 与 run store 的一次性关闭状态。

`intakeCtx` 在首次 drain 时取消；`executionCtx` 只在强制收口时取消。两者禁止复用。

### 6.2 启动顺序

1. CLI 解析 Host 配置及 `HOST_API_SHUTDOWN_TIMEOUT`；
2. `NewServer` 打开并组合所需 store、Provider、Skill 和 approval 组件；
3. `hostAPIService.Start` 使用 `net.Listen` 显式绑定 `HOST_API_ADDR`；
4. 创建 `http.Server`，用 `executionCtx` 作为 `BaseContext`；
5. 启动单槽 queued worker、approval janitor 和 HTTP serve；
6. listener 绑定成功后才输出既有的 `host_api_addr=<addr>`；
7. serve 后续的非预期错误通过 `Done` 上报。

监听失败发生在后台组件接单之前。`Start` 的部分失败必须能由后续 `Close` 回收已经打开的
store。预期 shutdown 后的 `http.ErrServerClosed` 归一为正常 terminal result；运行态中
非预期的 serve 退出归为 `serve_failed`。

### 6.3 Drain 顺序

首次中断后：

1. 标记 Host 正在 drain；
2. 取消 `intakeCtx`，阻止 worker claim 下一项任务并停止 janitor 新扫描；
3. 调用 `http.Server.Shutdown`，关闭 listener、idle connection，并等待已接收 handler；
4. 并行等待 queued worker 当前 workflow、活动 HTTP execution 和正在收尾的 janitor；
5. 当前 workflow 可以自然到达 `succeeded`、`failed` 或 `waiting_approval`；
6. 所有活动完成后按打开顺序的逆序关闭 workflow store、run store。

queued worker 收到 intake 取消后不得 claim 下一项 pending workflow。若取消发生时已有一项
正在执行，该项继续使用 `executionCtx`；完成后 worker 退出，尚未 claim 的 workflow 保持
pending，供下一进程恢复。

### 6.4 强制收口

drain 超时或收到第二次信号后：

1. 取消 `executionCtx`；
2. 强制关闭剩余 HTTP 连接；
3. 等待当前内置 Provider、step、approval 和工具执行响应 context 取消；
4. 对进入强制阶段时的 active execution 快照逐项执行操作类型对应的 cleanup；
5. cleanup 完成后关闭 store；
6. 返回 `shutdown_timeout` 和退出码 `5`。

当前内置 Host composition 的 Provider、step、store 和审批路径必须响应 context。若未来接入
忽略 context 的第三方组件，最多等待 cleanup budget；超时后不能与未停止 goroutine 并发
关闭 store。

## 7. Host execution registry 与业务收口

### 7.1 登记范围

execution registry 只属于 `examples/host-api`，不进入 `hostkit`。以下活动操作需要登记：

- sync workflow 执行；
- 当前 queued workflow 执行；
- final workflow approval continuation；
- agent tool approval/resume。

纯查询不登记。queued create、requeue 等短事务仍由 HTTP shutdown 等待；它们必须继续使用
request context 和 store 的原子更新，不能在后台脱离 handler。

每个登记项至少包含：

- workflow ID；
- 操作种类；
- 完成信号；
- 操作类型对应的条件式 cleanup。

ForceStop 先取得 active 快照，再取消执行 root，等待每项返回，最后对快照调用 cleanup。
即使某项刚刚完成，cleanup 也只能通过持久化状态做条件式判断，不能依赖内存中的旧状态。

### 7.2 条件式 workflow 收口

通用 workflow cleanup 使用独立 cleanup context，并遵守：

- `succeeded`、`failed`、`cancelled` 不覆盖；
- 已经完整持久化的稳定 `waiting_approval` 不覆盖；
- shutdown 中断且仍为 `pending` 或 `running` 的活动 workflow 更新为 `failed`；
- `WorkflowRun.Error` 固定写入 `host_shutdown_timeout`；
- queued workflow 同时清空 `LeaseOwner` 和 `LeaseUntil`；
- 不自动 requeue；
- 不删除已持久化的 step history、AgentRunID、AuditRef 或 output ref。

“稳定 waiting approval”要求其对应 approval/checkpoint 已经完整持久化。若 agent tool
approval/resume 仍处在外部副作用与 checkpoint 提交之间，必须走该操作专属 cleanup，而
不能只看 workflow status。

### 7.3 AgentRun 与 approval cleanup

agent tool approval/resume 的 cleanup 复用现有 runkit/checkpoint 原语：

- 未获得 approval lease：不修改 checkpoint 或 AgentRun；
- 已获得 lease 但工具未执行完成：将 lease 对应 checkpoint 和 AgentRun 收口为失败；
- 工具可能已经产生副作用但本地完成状态未持久化：checkpoint、AgentRun 和 workflow
  收口为失败，原因使用稳定 shutdown code；
- 已经完整持久化为 consumed/succeeded 且 workflow 已进入 final approval wait：保持现状；
- cleanup 不重试工具，不创建新 checkpoint，不自动 requeue。

所有更新必须使用当前 store contract 的条件式操作，禁止直接拼接测试专用 SQL 或根据
workflow ID 硬编码状态。

## 8. 配置、错误和退出契约

### 8.1 Shutdown 配置

新增：

```text
HOST_API_SHUTDOWN_TIMEOUT
```

语义：

- 缺省值为 `30s`；
- 接受 Go duration，例如 `10s`、`2m`；
- 解析失败或值不大于零属于 `config_failed`；
- 强制 cleanup timeout 固定为 `5s`，不增加第二个环境变量。

Host 必须在调用边界显式包装错误，不按错误字符串分类：

- 缺失、格式错误或互相冲突的环境配置属于 `config_failed`；
- OIDC discovery、Skill catalog 读取、Provider composition 和 store 打开失败属于
  `initialization_failed`；
- `net.Listen` 失败属于 `listen_failed`；
- 已进入运行态后的 HTTP serve 错误属于 `serve_failed`。

为保持这个边界清晰，当前同时承担环境校验和 OIDC discovery 的启动代码应拆成“纯配置
解析/校验”和“有外部 I/O 的初始化”两步。已有 fail-closed 校验顺序保持不变：明显无效的
Keychain、Skill 或 shutdown 配置必须在 OIDC discovery 前被拒绝。

第二次 `SIGINT/SIGTERM` 不立即跳过 cleanup，而是跳过剩余 drain，直接进入最多 5 秒的
ForceStop/Close 总预算。

### 8.2 稳定退出码

| 场景 | error code | exit |
|---|---|---:|
| 正常启动后 drain 成功 | 无错误记录 | 0 |
| 环境配置错误 | `config_failed` | 2 |
| store、OIDC、Skill 等初始化失败 | `initialization_failed` | 2 |
| 地址绑定失败 | `listen_failed` | 3 |
| 非预期 HTTP serve 退出 | `serve_failed` | 4 |
| drain 超时或第二次信号 | `shutdown_timeout` | 5 |
| ForceStop/Close 未在 cleanup budget 内完成 | `shutdown_cleanup_timeout` | 5 |
| 未分类内部错误 | `internal_error` | 1 |

如果运行期 serve 异常后 cleanup 也超时，以更高风险的
`shutdown_cleanup_timeout` / `5` 为最终结果；否则保留 `serve_failed` / `4`。

正常 signal drain 不输出错误 JSON。所有非零退出只向 `stderr` 写一条 JSON：

```json
{"level":"error","event":"host_exit","code":"listen_failed","message":"listen tcp 127.0.0.1:8080: bind: address already in use"}
```

固定字段只有 `level`、`event`、`code`、`message`。不输出 panic stack、嵌套错误对象、
环境变量值、token、密钥、checkpoint 内容、Prompt、模型响应或原始 Provider payload。
Host 在把错误交给 `hostkit` 前负责产生安全 message；现有敏感输出扫描继续作为真实进程
门禁。

`main` 采用：

```go
func main() {
    os.Exit(runHost())
}
```

`runHost` 在返回前完成可完成的 cleanup；`main` 不包含业务分支或 `panic`。

## 9. 外部副作用边界

本阶段改善受控 signal shutdown，但不改变跨系统事务事实：

```text
external side effect committed
  -> local checkpoint / AgentRun / workflow commit
```

这两个动作不在同一事务中。进程在二者之间被 `SIGKILL`、cleanup timeout 或机器故障中断
时，Host 无法仅靠 SQLite 判断外部副作用是否已经发生。

因此：

- 强制收口后 workflow 失败关闭，不自动重放；
- operator 必须先核对外部系统，再决定是否调用 requeue；
- 不可逆写工具必须调用幂等外部 API，或用稳定 ToolCallID 作为去重键；
- requeue 允许重新发出请求，但 exactly-once 效果来自外部幂等/去重，不来自本地
  workflow store；
- 文档不得把 graceful shutdown 描述为任意崩溃点 exactly-once。

## 10. 测试设计

### 10.1 `hostkit` 默认单元测试

使用 fake `Service`、可控 interrupt channel 和短 deadline 覆盖：

- `Start` 成功与分类失败；
- 运行期 `Done` 异常；
- 第一次中断触发一次 Drain；
- Drain 成功后 Close 并退出 `0`；
- Drain 超时进入 ForceStop；
- 第二次中断跳过剩余 Drain；
- ForceStop 与 Close 共享 cleanup budget；
- cleanup timeout 返回 `shutdown_cleanup_timeout` / `5`；
- `Drain -> ForceStop -> Close` 调用顺序和最多一次语义；
- Start 部分失败后仍调用一次 Close；
- 稳定退出码与精确单行 JSON schema。

测试通过 channel/barrier 驱动顺序，不以任意 sleep 让测试碰巧通过。

### 10.2 Host API 进程内回归

使用可控阻塞 execution 和 store 测试替身覆盖：

- intake 取消后 worker 不 claim 第二个 queued workflow；
- 当前 queued workflow 在 drain 中可以自然完成；
- ForceStop 取消 sync、queued、final approval 和 agent approval 活动；
- 未终态 workflow 收口为 `failed/host_shutdown_timeout`；
- queued lease 清空；
- terminal 和稳定 waiting approval 不被覆盖；
- checkpoint、AgentRun 与 workflow 状态一致；
- worker、janitor、execution registry 和 stores 只停止/关闭一次；
- goroutine 回落；
- `go test -race` 下无 Add/Wait、channel close、map 或 store 并发问题。

### 10.3 真实 Host 二进制黑盒

继续使用 `hostapisystemsmoke` 真实进程框架，增加独立生命周期测试：

1. **配置失败**
   - 启动真实 binary；
   - 断言退出 `2`；
   - stderr 只有一条 `config_failed` JSON；
   - 不含 `panic`、`goroutine` stack 或敏感配置值。
2. **监听失败**
   - 先占用 loopback 地址；
   - 用有效 Host 配置启动 binary；
   - 断言退出 `3` 和 `listen_failed`。
3. **优雅 drain 与重启恢复**
   - 提交两个 queued workflow；
   - 单槽 worker 在第一个 workflow 的 loopback Provider 调用处阻塞；
   - 发送第一次 signal；
   - 断言 listener 已停止接单且第二个 workflow 未被 claim；
   - 释放第一个 workflow，让它到达稳定状态；
   - 断言进程退出 `0`；
   - 使用同一 runtime home 启动新进程；
   - 断言第一个 workflow 状态保持，第二个 pending workflow 被恢复执行。
4. **Drain 超时**
   - 测试进程使用短 `HOST_API_SHUTDOWN_TIMEOUT`；
   - Provider 在收到 context cancellation 前保持阻塞；
   - 断言退出 `5` 和 `shutdown_timeout`；
   - 只读检查 SQLite 中 workflow 为 `failed/host_shutdown_timeout` 且 lease 已清空；
   - 重启后断言 Provider 未被自动再次调用。
5. **第二次 signal**
   - 第一次 signal 进入 drain；
   - 第二次 signal 触发强制收口；
   - 断言不再等待完整 drain timeout，最终退出 `5`。

现有 stop helper 必须拆成“请求优雅停止”和“断言指定退出结果”两层。测试不得把五秒后
`Process.Kill` 当作成功；kill 只用于测试清理，并必须使测试失败。

### 10.4 外部副作用边界子进程

测试专属子进程复用 `hostkit` 和 Host execution registry，但不向生产 binary 注册测试
工具，也不增加生产环境变量开关。

loopback 副作用服务记录：

- 同一稳定 ToolCallID 的请求次数；
- 去重后实际生效次数；
- 首次副作用已经提交的 barrier。

测试在首次副作用生效后、本地完成状态持久化前触发强制收口，并断言：

- 子进程退出 `5`；
- workflow、AgentRun/checkpoint 进入失败关闭状态；
- 使用同一 runtime home 重启不会自动请求副作用服务；
- operator 显式 requeue 后可以再次请求；
- 外部服务按 ToolCallID 去重，请求次数可以增加，但实际生效次数保持 `1`。

该测试只证明 Host 不自动重放以及外部幂等可以安全承接显式重试；它不证明 Host 自身具有
跨系统 exactly-once。

## 11. 模块、文档与发布布局

新增：

- `hostkit/go.mod`
- `hostkit/README.md`
- `hostkit` 生命周期实现与单元测试

更新：

- `examples/host-api/go.mod`：依赖 `hostkit v0.1.0` 并保留仓库内相对 replace；
- `examples/host-api`：main、lifecycle adapter、worker/janitor 控制、execution registry、
  默认测试和真实进程测试；
- `go.work`：增加 `./hostkit` use 和精确 `hostkit v0.1.0 => ./hostkit`；
- `scripts/verify-all.sh`：执行 `hostkit` 测试；
- `scripts/verify-release-layout.sh`：发布模块清单从 12 个变为 13 个，增加
  `hostkit/v0.1.0`；
- `docs/modules.md`：记录 `hostkit` 的独立边界和依赖规则；
- 根 `README.md`、`examples/host-api/README.md`、`docs/host-api-contract.md`：记录启动、
  signal、退出码、重启恢复和副作用边界。

历史 v0.1.0 设计、验收和发布记录中的“12 个发布模块”是当时事实，保持原样。

`hostkit` 的依赖规则是：

```text
hostkit must not import any other GoAgents workspace module
examples/host-api may import hostkit and existing composition modules
```

## 12. 验收标准

默认门禁：

```bash
(cd hostkit && go test ./... -count=1)
(cd hostkit && go test -race ./... -count=1)
(cd examples/host-api && go test ./... -count=1)
(cd examples/host-api && go test -race ./... -count=1)
(cd examples/host-api && go vet ./...)
bash ./scripts/verify-release-layout.sh
bash ./scripts/verify-all.sh
git diff --check
```

真实进程门禁使用独立测试名，并在实现计划中固定精确命令。所有生命周期子场景必须 PASS；
`SKIP` 只能作为环境阻塞记录，不能算完成证据。

完成判定：

- `main.go` 不再有启动/监听 `panic`；
- 配置、监听、serve、drain timeout 的 JSON 与退出码符合固定契约；
- 第一次 signal 停止接单并允许当前 workflow 完成；
- 第二次 signal 和 drain timeout 有界退出；
- 正常强制 cleanup 后 workflow/run/checkpoint/lease 状态一致；
- 同 runtime home 重启不会自动重放失败关闭 workflow；
- 外部副作用测试明确证明 Host 边界与幂等责任；
- 新 `hostkit` 模块进入工作区、验证脚本和发布布局；
- 不引入 Skill 动态激活、多 worker 或其他非目标能力。
