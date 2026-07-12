# Host API Keychain 进程烟测隔离设计

## 1. 目标

修复 `TestHostAPIProcessToolApprovalSurvivesRestart` 在重复运行时等待 Keychain 授权界面、最终 HTTP 超时的问题，同时保留该测试对真实 Host API 二进制、OIDC、SQLite、macOS Keychain 加密和进程重启恢复的端到端验证价值。

本设计只隔离烟测使用的 Keychain 命名空间。生产默认服务名、密钥 ID、AES-GCM 信封格式和失败关闭语义均保持不变。

## 2. 根因

当前真实进程烟测固定使用生产 Keychain 标识：

- service：`goagents.host-api.approvals`
- key ID：`local-v1`
- account：`approval-data-key:local-v1`

macOS 传统 Keychain 项的默认受限 ACL 只信任创建该项的调用应用。烟测每次都会在新的临时目录重新编译 Host API；旧 Keychain 项仍绑定此前的临时二进制，新二进制读取时会触发系统授权交互。

Keychain 查询默认允许认证 UI，而且 `SecItemCopyMatching` 会阻塞调用线程。无人值守测试无法处理授权窗口，于是首个创建 workflow 的 HTTP 请求超时。直接使用同一 Go Keychain provider 读取现有项也会复现阻塞，而 `security` CLI 的只读查询可立即返回，说明阻塞位于应用身份与 Keychain ACL 的交互，不在 HTTP、SQLite 或 SkillKit 路径。

参考：

- Apple `SecAccessCreate`：`trustedlist == nil` 时只信任调用应用。
- Apple `kSecUseAuthenticationUI`：未指定时默认允许认证 UI。
- Apple `SecItemCopyMatching`：调用会阻塞当前线程。

## 3. 选择的方案

烟测为每次测试运行生成独立的 Keychain service，并通过宿主启动配置传给真实子进程。第一次启动在该隔离 service 中创建数据密钥；第二次启动复用同一个二进制、service 和 key ID，因此能够无交互解密原 checkpoint。测试结束后只删除自己创建的精确 service/account 项。

生产继续使用原默认值。配置项只决定 Keychain 中的查找标识，不承载或导出密钥材料。

未采用的方案：

- 固定并签名烟测二进制：依赖本机签名环境，不能作为普通开发回归。
- 仅禁止认证 UI：可以避免挂起，但会直接失败，无法证明真实重启恢复闭环。
- 删除现有生产 Keychain 项后重测：可能破坏尚未消费的审批 checkpoint，禁止采用。
- 文件或环境变量密钥：违反现有“本机 Keychain 是唯一生产密钥后端”的约束。

## 4. 宿主配置契约

`examples/host-api.Config` 增加两个宿主拥有的字段：

```go
AgentApprovalKeychainService string
AgentApprovalKeyID           string
```

解析规则：

1. 两者都为空：使用现有生产默认值 `goagents.host-api.approvals` 和 `local-v1`。
2. 两者都为非空字符串：使用调用方显式配置的值。
3. 仅设置一个或值只包含空白：启动失败，不能静默拼接默认值。

真实 CLI 从以下环境变量读取这两个宿主配置：

- `HOST_API_AGENT_APPROVAL_KEYCHAIN_SERVICE`
- `HOST_API_AGENT_APPROVAL_KEY_ID`

HTTP 请求、workflow metadata、模型和 Skill 均不能改变这些值。它们不会写入 SQLite、日志或 HTTP 响应。

`hostAgentApprovalService` 保存解析后的 service/key ID；`activeCipher` 继续延迟打开 Keychain，只把这两个值传给 `approvalcrypto.OpenMacOSKeychainKeyProvider`。未发生工具审批暂停时仍不访问 Keychain。

## 5. 烟测数据流

1. 父测试确认当前登录 Keychain 可用。
2. 父测试生成唯一的 smoke service，例如 `goagents.host-api.approvals.smoke.<random>`，key ID 固定为 `local-v1`。
3. 第一次真实 Host 进程通过环境变量收到该 service/key ID，创建测试专属 Keychain 数据密钥并持久化加密 checkpoint。
4. 父测试停止进程并只读取 SQLite，确认 checkpoint 是非明文密文。
5. 第二次启动复用同一 Host 二进制、runtime home、service 和 key ID，批准工具并完成 workflow。
6. 所有子进程停止后，父测试删除精确的测试 service/account 项；不得查询、打印或导出密钥值。

清理只允许命中本次测试生成的、带 `.smoke.` 前缀的 service。即使测试中途失败，也通过 `t.Cleanup` 尝试清理；不存在的测试项视为无需清理，其他删除错误应报告。

## 6. 错误与安全边界

- Keychain 锁定、不可用、拒绝访问或读取失败时，工具审批继续失败关闭，不增加回退后端。
- 配置不完整时在 Host 启动阶段失败，避免运行到审批暂停后才暴露错误。
- 生产默认 Keychain 项永不由烟测删除或迁移。
- 本切片不改变现有密文格式、AAD、key rotation 语义或 checkpoint schema。
- 本切片不新增通用 Keychain 删除 API；清理逻辑仅存在于 Darwin 系统烟测文件中。
- 不通过 goroutine 超时掩盖无法取消的原生 Keychain 调用；测试通过隔离 ACL 根因避免交互。

## 7. 验证

### 单元与模块测试

- 默认空配置解析为原生产 service/key ID。
- 自定义 service/key ID 成对传入 approval service。
- 只配置一个字段时 `NewServer` 返回明确错误。
- `go test ./...`、`go test -race ./...` 和 `go vet ./...` 通过。

### 真实进程测试

先修改系统烟测使用唯一 service/key ID，在生产代码尚未读取新环境变量时观察原超时 RED；实现配置传递后，同一命令必须 GREEN：

```bash
go test -tags hostapisystemsmoke -run TestHostAPIProcessToolApprovalSurvivesRestart -count=1 -v
```

通过标准包括：

- 首次工具暂停成功返回；
- SQLite 中 checkpoint 非明文；
- 同一二进制重启后能解密并恢复；
- OIDC 拒绝与批准路径保持有效；
- 最终 workflow 成功；
- 测试专属 Keychain 项被清理；
- 生产 `goagents.host-api.approvals/local-v1` 项未被读取、修改或删除。

最后运行 `bash ./scripts/verify-all.sh` 与 `git diff --check`。

## 8. 非目标

- 不迁移现有生产 Keychain 项。
- 不设计生产 key rotation 或多租户密钥服务。
- 不切换到文件、环境变量、远端 KMS 或 data-protection Keychain。
- 不改变 Agent、SkillKit、tool registry 或审批业务协议。
- 不把系统烟测加入默认 `go test ./...`。
