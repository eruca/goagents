# MVP README 干净环境复现设计

**日期：** 2026-07-14

**状态：** 已确认

## 1. 目标

让首次进入仓库的开发者不依赖既有 runtime、数据库、模型配置或真实云端凭证，仅按 README
即可找到项目入口，并在本机完成 Host API MVP 的真实进程验收。

本阶段解决的是文档可发现性和可复现性，不新增认证后门，不降低 OIDC、Keychain、Skill 或
Provider 的安全边界。

## 2. 已确认问题

在 `main@76c52a2` 创建全新 worktree 后得到以下证据：

- 仓库根目录没有 `README.md`，首次使用者无法从根目录找到模块和 MVP 入口；
- `examples/host-api/README.md` 末尾直接给出 `go run .`，但服务启动必须连接可发现 JWKS 的
  OIDC issuer，README 没有提供本地 issuer；
- 清空 OIDC 和 runtime 环境后执行 `go run .`，程序因 `invalid OIDC approval configuration`
  退出；
- README 已有的真实进程 Keychain smoke 在同一干净 worktree 中 PASS，证明程序闭环本身可运行。

## 3. 方案选择

采用“自动化黑盒作为零配置快速开始，真实 OIDC 作为长期服务前置条件”的方案。

未选择以下方案：

- 不新增独立开发 OIDC 服务，避免为文档复现引入新的生产相邻组件和认证维护面；
- 不只补一句“请配置 OIDC”，因为这仍不能给首次使用者提供本机可完成的 MVP 闭环。

## 4. 根 README

新增根目录 `README.md`，只承担仓库入口职责：

- 简述该仓库是多个 Go agent runtime kit 与可运行示例组成的 workspace；
- 列出核心模块及其 README 链接；
- 给出全仓验证命令 `bash ./scripts/verify-all.sh`；
- 把 Host API MVP 指向 `examples/host-api/README.md`；
- 给出 macOS 本机进程级 MVP 黑盒命令，并说明三个子场景必须全部 PASS、SKIP 不算通过。

根 README 不复制 Host API 的完整配置、端点或安全说明，避免两份文档漂移。

## 5. Host API README

重排 `examples/host-api/README.md` 的运行入口，形成两条明确路径。

### 5.1 本机 MVP 快速验收

在文档前部增加快速开始：

```bash
cd examples/host-api
go test ./... -count=1
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPBlackBoxClosure$' \
  -count=1 ./...
```

说明：

- 第二条命令仅支持带 CGO 的 macOS 交互式登录会话；
- 测试自带 loopback OIDC 和合成 OpenAI-compatible Provider，不使用真实 Qwen/API key；
- 使用唯一 `.smoke.` Keychain item，并在结束时精确清理；
- 三个子场景必须全部 PASS；任何 SKIP 都表示环境阻塞，而不是验收成功。

### 5.2 连接真实 OIDC 后运行服务

保留 `go run .`，但明确它不是零配置命令。运行前必须设置：

```bash
export HOST_API_OIDC_ISSUER=https://id.example.com
export HOST_API_OIDC_AUDIENCE=goagents-host-api
go run .
```

同时说明 issuer 必须可通过 OIDC discovery/JWKS 访问；如需持久状态，再设置
`HOST_RUNTIME_HOME`。不提供关闭认证或伪造 approver 的开发开关。

## 6. 范围边界

本阶段只修改：

- 根目录 `README.md`；
- `examples/host-api/README.md`；
- 本设计与后续实施计划/验收记录。

本阶段不修改：

- Go 生产代码、HTTP API 或 OpenAPI；
- OIDC、Keychain、SQLite、Skill 或 Provider 实现；
- 默认测试标签或 `scripts/verify-all.sh`；
- 真实 Qwen 配置和凭证。

`go run .` 缺少配置时以 panic 退出属于独立 CLI 错误呈现问题，不影响本阶段 README 复现目标，
留到最终 P0-P3 汇总评估是否需要后续处理。

## 7. 验收标准

在新的、无未提交变更的 worktree 中执行：

```bash
bash ./scripts/verify-all.sh
cd examples/host-api
go test ./... -count=1
go test -v -tags hostapisystemsmoke \
  -run '^TestHostAPIProcessMVPBlackBoxClosure$' \
  -count=1 ./...
cd ../..
git diff --check
```

验收要求：

- 所有命令退出码为 0；
- 三个 MVP 黑盒子场景均 PASS，没有 SKIP；
- 根 README 中的相对链接指向现有文件；
- README 不包含真实 key、机器专属路径或可绕过 OIDC 的说明；
- 文档明确区分“自动化本机验收”和“连接真实 OIDC 后长期运行”。
