# Host Skill Bootstrap v1 设计

**日期：** 2026-07-13

**状态：** 已实现

## 1. 背景

`skillkit` 已实现 Skill manifest 解析、可信根发现、digest、准入、run-start activation、受限资源读取和 Agent adapter。`examples/host-api` 已实现安全的 `GET /skills`、workflow `skill_refs` 持久化、重启恢复和 host-side `evalkit` 发布门禁。

当前缺口位于真实进程组合层：默认 `host-api` 启动过程没有构建 `SkillCatalog`，因此 `GET /skills` 始终返回空数组；仓库也没有可供该进程直接使用的真实 `SKILL.md` 包。底层契约已经存在，但默认程序还不能使用本机 Skill。

本切片只补齐显式本机 Skill root 到现有 host composition 的启动闭环，不扩展 SkillKit 核心能力。

## 2. 目标

1. 允许本机 operator 通过一个显式环境变量为默认 `host-api` 配置单一可信 Skill root。
2. 在启动阶段生成不可变 catalog，并继续复用现有 `Config`、HTTP、workflow persistence 和 activation 路径。
3. 提供一个真实 instruction-only Skill，使默认二进制具备可验证的 Skill 使用路径。
4. 用独立真实进程 smoke 证明发现、workflow 选择、完整 digest 和重启后稳定重建。
5. 保持失败关闭，不把目录配置解释为工具授权。

## 3. 非目标

本切片不实现：

- 多 Skill root、scope 配置文件或目录优先级；
- 自动扫描当前目录、`.agents/skills` 或用户 home；
- 远程 registry、下载、升级或依赖安装；
- Skill 脚本执行或沙箱；
- 模型驱动动态激活；
- 多 Agent handoff；
- Skill 声明工具到 request-scoped registry 的投影；
- 运行时 catalog 刷新或文件监听。

## 4. 方案选择

评估过三种启动方式：

1. **单一显式可信目录：** 通过一个环境变量配置，未配置时保持空 catalog。
2. **多目录配置文件：** 支持 builtin、user、workspace 多 scope 和 trust 配置。
3. **约定目录自动发现：** 自动扫描 `.agents/skills`。

采用方案 1。它没有隐式扫描和 cwd 依赖，信任边界清晰，也不需要提前设计多 root 冲突、配置格式和热更新生命周期。

## 5. 启动配置契约

新增可选环境变量：

```text
HOST_API_SKILL_ROOT=/absolute/path/to/skills
```

语义如下：

- 未配置或值为空时，Skill bootstrap 关闭，行为与当前默认 CLI 相同；
- 非空值必须是绝对目录；
- operator 显式配置该目录即表示信任其当前 canonical target；
- catalog 仅在进程启动时构建一次；
- Skill 内容变更后需要重启进程才能形成新 catalog；
- HTTP 请求不能新增 root、修改 trust、设置 GateContext 或触发刷新。

相对路径被拒绝，避免服务启动目录改变后信任目标漂移。

## 6. 组件边界

### 6.1 启动 helper

在 `examples/host-api` 增加小型启动 helper，例如：

```go
func loadHostSkillConfig(getenv func(string) string) (*skillkit.Catalog, skillkit.GateContext, error)
```

它只负责：

1. 读取并校验 `HOST_API_SKILL_ROOT`；
2. 使用固定、无路径信息的 root ID；
3. 调用现有 `skillkit.Discover`；
4. 返回 catalog 和默认 CLI 的 GateContext。

建议固定 root 配置为：

```go
skillkit.Root{
    ID:      "local-user-skills",
    Scope:   skillkit.ScopeUser,
    Trusted: true,
    Enabled: true,
}
```

物理目录只存在于私有配置和 `skillkit` 内部，不进入公开 catalog、错误响应或审计 DTO。

### 6.2 现有 Config 保持不变

`loadHostConfig` 将 helper 的结果写入已有字段：

```go
Config{
    SkillCatalog:     catalog,
    SkillGateContext: gate,
}
```

`NewServer` 的公开契约不改变。嵌入式宿主仍可自行构建 catalog 和 GateContext；默认 CLI 只是第一次使用现有注入点。

本地 Skill 配置应在 OIDC discovery 前完成，以便纯本机配置错误尽早失败，避免无意义的外部初始化。

## 7. 信任与能力边界

默认 CLI 的 GateContext 只声明当前操作系统：

```go
skillkit.GateContext{OS: runtime.GOOS}
```

`HostFeatures` 和 `AllowedToolIDs` 保持为空。因此：

- 无额外能力要求的 instruction-only Skill 可以激活；
- 声明 required tool 或 required host feature 的 Skill 显示为 `unavailable`；
- Skill 配置不会自动注册 `record_review`、授予 write permission 或绕过 approval；
- optional tool 也不会因此进入 Agent registry。

这是刻意限制。现有 `SkillGateContext` 是 host 注入的静态值，而 `record_review` 是否可见由 workflow task profile 决定。没有 request-scoped 交集设计前，默认 CLI 不能把 host 最大能力误当作当前 workflow 的授权。

工具型 Skill 的投影必须作为后续独立设计处理。

## 8. 数据流

```text
HOST_API_SKILL_ROOT
        │
        ▼
loadHostSkillConfig
        │
        ├── validate absolute directory
        ├── skillkit.Discover
        └── GateContext{OS: runtime.GOOS}
        │
        ▼
existing Config
        │
        ▼
NewServer
        │
        ├── GET /skills: safe catalog projection
        ├── POST /workflows: resolve and persist name@digest
        └── Agent run/resume: existing exact-digest activation
```

不增加第二套 manifest parser、catalog cache、workflow metadata 或 Agent Skill loader。

## 9. 真实示例 Skill

新增：

```text
examples/host-api/skills/workflow-review/SKILL.md
```

该 Skill：

- 使用标准 `name` 和 `description`；
- 正文只提供明确、可复现的 workflow review 指令；
- 不声明 required/optional tool；
- 不声明 host feature；
- 不包含脚本、安装步骤或网络依赖。

示例的职责是证明本机 package discovery 和 instruction activation，不承担通用 Skill 市场或执行器示范。

## 10. 失败语义

- 环境变量未配置或为空：返回 nil catalog 和零值 GateContext，不报错。
- 值不是绝对路径：启动失败。
- 路径不存在、不是目录或不可读取：启动失败。
- 单个 Skill manifest 无效：catalog 保留 `invalid` 条目，host 可启动，但该 Skill 不能解析或激活。
- Skill 声明未满足的 tool/feature：条目为 `unavailable`，workflow 创建失败关闭。
- Skill package 在进程运行期间改变：现有 catalog 不刷新，activation 的 digest recheck 失败关闭。
- 进程重启且 package 未改变：重新发现得到相同 digest。
- 进程重启且 package 已改变：新 catalog 产生新 digest；持久化旧 digest 的 workflow 不能静默升级。

配置错误和 HTTP 错误不得回显 root 物理路径、Skill 正文、资源内容或环境变量值。

## 11. 测试策略

### 11.1 启动配置单元测试

覆盖：

- 环境变量未配置；
- 合法绝对目录；
- 相对路径；
- 缺失目录；
- 普通文件而非目录；
- 错误文本不包含配置的物理路径。

### 11.2 Catalog/host 集成测试

覆盖：

- 仓库内真实示例 Skill 可发现且 eligible；
- `GET /skills` 返回完整 digest，不泄露正文和路径；
- required tool/feature Skill 在默认 CLI gate 下 unavailable；
- workflow 创建继续持久化完整 `name@digest`。

### 11.3 独立真实进程 smoke

新增独立测试文件，使用与现有进程 smoke 相同的显式 build tag 和通用进程 helper，但不复用 Keychain 测试场景：

1. 构建真实 `host-api` 二进制；
2. 以绝对 `HOST_API_SKILL_ROOT` 启动；
3. 断言 `GET /skills` 返回示例 Skill 和 digest；
4. 创建引用该 Skill 的 instruction-only workflow；
5. 停止进程并以相同 runtime home 和 Skill root 重启；
6. 使用相同显式 `name@digest` 创建第二个 workflow；
7. 断言 digest 稳定且 activation 成功。

该 smoke 不请求工具，因此不创建或读取 Keychain item。Keychain 重启 smoke 保持原职责，不附带 Skill bootstrap 断言。

### 11.4 全量验证

实现完成后运行：

```bash
cd examples/host-api && go test ./...
cd examples/host-api && go vet ./...
cd examples/host-api && go test -tags hostapisystemsmoke -run '^TestHostAPIProcessLoadsConfiguredSkillRootAcrossRestart$' -count=1
bash ./scripts/verify-all.sh
git diff --check
```

## 12. 文档同步

实现切片应同步：

- `examples/host-api/README.md`：新增环境变量、默认关闭行为和使用示例；
- `docs/host-api-contract.md`：记录启动配置、信任和错误边界；
- `skillkit/README.md`：修正仍将 host API exposure 和 durable `skill_refs` 标为未来能力的陈旧描述；
- `docs/superpowers/specs/2026-07-12-skillkit-design.md`：修正首版实现状态，同时保留动态激活和脚本执行的延后边界。

## 13. 成功标准

本切片完成的判定标准是：

1. 默认 CLI 未配置 root 时完全兼容现有行为；
2. 配置一个绝对可信 root 后，真实进程可列出并激活真实 Skill；
3. workflow 响应和 SQLite metadata 持有完整稳定 digest；
4. 同一 package 跨进程重启保持相同 digest；
5. required tool/feature 不会因目录可信而获得授权；
6. 配置、catalog 和 activation 错误不泄露本机路径或 Skill 内容；
7. 独立进程 smoke 与全仓验证通过；
8. 没有引入多 root、脚本执行、依赖安装、动态激活或多 Agent 抽象。

## 14. 后续顺序

本切片稳定后，再按真实需求分别设计：

1. request-scoped Skill tool projection；
2. 可 checkpoint 的动态 Skill activation；
3. 只有出现真实受控脚本场景时，才设计脚本执行器和沙箱。

三者不能混入本次 bootstrap。
