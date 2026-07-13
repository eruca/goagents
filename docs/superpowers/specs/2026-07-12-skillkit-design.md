# Skillkit 设计：可移植 Skill 的宿主受控运行契约

**日期：** 2026-07-12
**状态：** 首版已实现
**范围：** 为 `goagents` 增加独立的 `skillkit` sibling module；不把动态 Skill 平台塞入 `goagent/agentcore`。

## 1. 决策摘要

`skillkit` 将 Skill 视为一个可移植的、以 `SKILL.md` 为入口的**过程知识包**，而不是一个可自行获得 shell、网络、文件或密钥权限的可执行插件。

Skill 可以：

- 声明何时适用、所需输入、步骤、输出和引用资源；
- 声明运行所需的宿主能力；
- 被宿主发现、校验、准入、选择和按需注入到 Agent 上下文。

Skill 不可以：

- 因为目录中存在脚本就直接执行该脚本；
- 因为声明了工具、环境变量或依赖就自动取得权限；
- 在 Agent 运行中安装包、修改全局 `PATH` 或执行任意安装命令；
- 通过同名覆盖悄悄替换另一个 Skill。

实际副作用仍只来自宿主注册的 `tools.Tool`，并继续经过现有 `policy`、tool approval、checkpoint 和审计链路。

这使 `skillkit` 兼容当前 Agent Skills 的可移植目录格式，同时保留 `goagent` 小 ReAct core 的边界。

## 2. 背景与依据

Agent Skills 开放格式的最小单元是一个目录与根目录 `SKILL.md`；其中可带有 `scripts/`、`references/`、`assets/` 等资源。格式强调元数据先发现、正文按需加载、资源最后读取的渐进披露模式。

`smallnest/goclaw` 已证明这条产品路径是可用的：它扫描多个根目录，读取 frontmatter，建立 Skill 摘要，按名称加载正文，并提供 OS/二进制/环境依赖检查。其 README 同时明确 Skill 是指导模型调用既有工具的 prompt-driven 指令，而不是独立执行器。

但 goclaw 的 loader 允许同名静默覆盖、递归暴露资源物理路径，并提供 `brew`、`apt`、`npm`、`pip`、`go`、`sh -c` 等依赖安装路径。这适合本地个人 Agent 产品，但不符合本仓库现有的可恢复审批、最小权限和可审计宿主方向。

**采用结论：** 参考 goclaw 的 discovery、gating 和分层目录；不参考其自动安装、任意 shell installer 与静默覆盖策略。

## 3. 目标与非目标

### 3.1 目标

1. 支持标准 `SKILL.md` 包的本机发现与按需使用。
2. 让宿主以确定性方式判断某 Skill 是否可用于当前运行，并给出不含秘密的不可用原因。
3. 让 Skill 的工具声明成为“需求”，而非“授权”；最终工具集由宿主和调用主体决定。
4. 允许安全读取同一 Skill 根目录内的引用资源，不泄露宿主绝对路径。
5. 为本机 SQLite host 保留可审计的来源、版本和内容摘要。

### 3.2 非目标

首版不做：

- 远程 registry、市场、下载、升级或依赖解析；
- `brew`、`pip`、`npm`、`go install`、`apt` 或任意 shell 自动安装；
- 把 `scripts/` 解释为可直接执行的入口点；
- Docker/VM 沙箱运行时；
- 多 Agent skill handoff；
- 向量检索式 Skill 匹配；
- 在一次 `agentcore.Run` 中动态增减工具集合。

最后一项是刻意约束：现有 `ToolProviderStage` 在运行开始时生成 request-scoped registry。首版将 Skill 激活固定在 run start，避免中途改变工具面造成批准、checkpoint 与审计语义漂移。

## 4. 核心原则

### 4.1 Skill 是声明，宿主是权威

`SKILL.md` 描述工作流与期望能力；它不能授予权限。每次运行的有效能力由下面的交集决定：

```text
Host 已注册工具
    ∩ 调用主体获授权工具
    ∩ 当前工作流允许工具
    ∩ 已激活 Skill 声明的所需/可选工具
    = 本次 Run 的可见工具
```

缺失、未授权或被 workflow 禁用的能力使 Skill 不可激活，而不是退化为“仍加载正文、让模型自行想办法绕过”。MCP 也采用按请求授权决定工具可见集的思路；这里将同一原则保留在本机 host。

### 4.2 渐进披露，不全量污染上下文

- **发现阶段：** 仅读取元数据，建立 catalog。
- **选择阶段：** 宿主从用户显式 Skill 引用或其受控 selector 得到候选。
- **激活阶段：** 才读取所选 `SKILL.md` 正文并生成 `agentcore.Skill`。
- **资源阶段：** 只有已激活 Skill 才能经受限 resolver 读取其引用资源。

未选择 Skill 的正文、脚本与资源不得进入 prompt。首版不向模型暴露“任意读取本机 Skill 文件”的工具。

### 4.3 失败关闭与可复现

无效 manifest、摘要变化、路径越界、歧义名称或能力不足都阻止激活。一次运行记录 Skill 的规范名称、来源 root、package digest 和有效工具 ID；不记录正文、环境变量值、密钥或资源原文。

### 4.4 兼容开放格式，扩展必须命名空间化

标准前置字段保持可移植：`name`、`description`、可选 `license`、`metadata`。本项目扩展只放入 `metadata.goagents`，未知 metadata 一律保留但不执行。

## 5. 包布局与依赖方向

新增独立 Go module：

```text
skillkit/
  manifest.go       # 标准 frontmatter 与 goagents extension 的解析/校验
  root.go           # 信任根与 scope
  catalog.go        # 发现结果、冲突和查询
  gate.go           # 环境、宿主能力和授权准入
  resource.go       # skill:// 受限资源解析
  provider.go       # 到 agentcore.SkillProvider / ToolProvider 的 host adapter
  audit.go          # 可选的无敏感审计 DTO
  *_test.go
```

依赖方向必须保持：

```text
host application
  ├── imports skillkit
  ├── imports goagent
  └── owns identity, policy, tools, approvals, storage

skillkit
  └── may expose an optional adapter importing goagent

goagent/agentcore
  └── does not import skillkit
```

解析、catalog、gating 和资源 resolver 应不依赖 `goagent`，使它们可用于 CLI、operator UI 或未来 host。仅 `provider.go` 是可选的适配层。

## 6. Skill 包格式

首版识别每个 root 下的一层目录：`<root>/<skill-name>/SKILL.md`。目录名与 `name` 必须一致；不再兼容大小写不确定的 `skill.md`，以降低跨平台歧义。

一个可移植包示例：

```text
clinical-summary/
  SKILL.md
  references/
    output-schema.md
  scripts/
    validate_output.py
```

```yaml
---
name: clinical-summary
description: Produce a bounded clinical summary with explicit evidence and uncertainty.
license: Apache-2.0
metadata:
  goagents:
    requires:
      os: [darwin, linux]
      host_features: [artifacts.v1]
      tools:
        required: [artifact.read]
        optional: [web.search]
    resources:
      allow: [references/output-schema.md, scripts/validate_output.py]
---

# Clinical summary

Use `artifact.read` to obtain the approved source material. State evidence,
uncertainty, and unsupported inferences separately. Validate against
`references/output-schema.md` before finalizing.
```

约束如下：

- `name`：`[a-z0-9-]{1,64}`，且等于目录名；
- `description`：必填、非空、上限 280 UTF-8 字符，作为发现摘要；
- `SKILL.md` 正文：首版上限 128 KiB；超过上限视为无效；
- `metadata.goagents.requires`：只描述平台、feature 与工具 ID，不能声明权限、网络域名、文件路径或密钥；
- `resources.allow`：显式 allowlist。包内存在不等于可被运行读取；
- `scripts/` 只是允许引用的资源。要运行它，必须由另一个已注册、已授权的宿主 Tool/Adapter 接收该资源引用并自行验证。

`metadata.goagents` 缺失时，Skill 仍是有效的纯指导型 Skill；它只在没有额外工具需求时可激活。

## 7. 发现、冲突与来源

### 7.1 Root 配置

host 显式提供 roots，而不是扫描用户机器上的任意目录：

```go
type Scope string

const (
    ScopeBuiltin   Scope = "builtin"
    ScopeUser      Scope = "user"
    ScopeWorkspace Scope = "workspace"
)

type Root struct {
    ID       string
    Dir      string // 只在宿主进程内使用，绝不进入 prompt 或 audit payload。
    Scope    Scope
    Trusted  bool
    Enabled  bool
}
```

首版 host 只配置 `builtin` 与当前 `workspace` root；`user` root 作为接口能力保留但默认关闭。所有根目录先 `EvalSymlinks`，再以其 canonical path 作为边界。

`Trusted=false` 的 root 只进入 operator 诊断 catalog，所有 entry 均以 `unavailable: untrusted_root` 呈现，不能被 workflow 激活。生产 host 只能显式启用受信任 root；开发环境若要试验 workspace Skill，也必须把该 root 明确配置为 trusted，而不是根据目录位置自动信任。

### 7.2 冲突规则

goclaw 的“后加载同名覆盖”不采用。`name` 必须在启用 roots 中唯一：

- 同名但 digest 相同：保留一个 canonical entry，并列出来源；
- 同名但 digest 不同：两个 entry 都标记 `ambiguous`，不得激活；
- root 禁用、无效或不可用的 entry 不能遮蔽有效 entry；
- 首版没有隐式 override。若业务需要 fork，必须改名。

这使 workflow 的 `SkillRef{Name, Digest}` 可重放，不会因当前工作目录中出现一个同名文件而改变行为。

### 7.3 Package digest

每次发现时，catalog 对 allowlist 中的 `SKILL.md` 和资源内容以 canonical relative path 排序后计算 SHA-256。激活和资源读取前必须重新核对 digest；不一致时返回 `ErrSkillDigestMismatch`，要求重新发现后重试。

符号链接可存在，但其最终路径必须仍在 Skill canonical root 内；任何 `..`、绝对路径、无法解析的链接或越界链接一律拒绝。

## 8. 准入契约

```go
type Availability string

const (
    AvailabilityEligible    Availability = "eligible"
    AvailabilityUnavailable Availability = "unavailable"
    AvailabilityInvalid     Availability = "invalid"
    AvailabilityAmbiguous   Availability = "ambiguous"
)

type GateContext struct {
    OS               string
    HostFeatures     map[string]bool
    AllowedToolIDs   map[string]bool
    EnvironmentProbe EnvironmentProbe // 仅报告变量是否存在，不读取或记录值。
}

type AvailabilityReport struct {
    State   Availability
    Reasons []Reason // 例如 missing_tool:artifact.read、unsupported_os:darwin
}
```

判定顺序：

1. manifest、路径与 digest 必须有效；
2. 当前 OS 和 host feature 必须满足；
3. `required` tool 必须同时存在于宿主注册表与当前 `AllowedToolIDs`；
4. `optional` tool 不阻止激活，但不会加入有效工具集；
5. 如果将来加入 `requires.env`，只检查存在性，且必须由 host allowlist 允许检查该变量名；绝不记录值。

catalog 可向 operator/UI 展示不可用状态；模型只看见可激活 Skill 的最小摘要。这样，Skill 不能用“依赖缺失”诱导模型去安装软件或寻找未授权替代工具。

## 9. 运行时选择与 Agent 集成

### 9.1 首版选择模型：run start activation

host 通过 `RunRequest` 的受控字段传入精确 Skill 引用，而非让模型在运行中动态重配 registry：

```go
type SkillRef struct {
    Name   string
    Digest string // 可选；生产 workflow 应填写，开发环境可在解析后回填。
}

type ActivationRequest struct {
    PrincipalID string
    WorkflowID  string
    Skills      []SkillRef
}
```

来源可为用户在 API 中显式选择、workflow 模板配置，或一个可审计的 host selector；selector 只能提出候选，不能绕过准入或修改 `AllowedToolIDs`。

激活成功后：

1. `skillkit` 加载正文，转换为 `agentcore.Skill`；
2. host 按第 4.1 节的交集过滤 request-scoped tools；
3. `WithSkillProvider` 与 `WithToolProvider` 在 `Agent.Run` 前完成 wiring；
4. 既有 policy、approval、checkpoint 和 audit 继续处理每个实际 Tool call。

没有显式 Skill 时，Agent 仍可按现有方式运行；`skillkit` 不改变基础工具集。

### 9.2 面向用户的发现

host 可提供 `GET /skills`，返回 `name`、`description`、`digest`、scope、availability 和不含秘密的 reason。它不返回正文、绝对路径或完整资源目录。

用户可在创建 workflow 时传入 `skill_refs`。host 将已解析的 `name@digest` 写入 workflow metadata，保证重启和 requeue 时使用相同版本；若 digest 不再存在，workflow 失败关闭并提示 operator 重新选择，而不静默升级。

### 9.3 后续动态激活

真正的模型驱动按需激活需要一个新、可 checkpoint 的 core/host protocol：模型提出 `activate_skill`，host 重新 gate、重新生成 tool registry、写入审计事件，并在下一次 Think 前重新编译 prompt。它会影响 tool approval 和 durable resume 的重放语义。

该能力明确延后到首版 run-start activation 完成并有评估证据后再设计，不能通过在 Tool executor 中偷偷修改 registry 实现。

## 10. 资源解析与脚本边界

`skillkit` 提供给宿主的资源引用是：

```text
skill://<name>@<digest>/<allowlisted-relative-path>
```

resolver 的输入只能是该 URI，输出为受限 reader 或受大小限制的字节流。它必须：

- 验证 Skill 已在当前 activation 中；
- 验证 digest 与 catalog 一致；
- 验证路径在 `resources.allow` 内；
- 拒绝绝对路径、`..`、未允许资源与越界符号链接；
- 不将宿主物理路径回传给模型。

`scripts/validate_output.py` 不是执行入口。一个未来的 `script.validate` Tool 可以读取该 URI，但它必须由 host 编译进来、独立声明输入 schema、限定解释器/容器/网络/工作目录，并照常走 permission 与 approval。`skillkit` 本身不启动进程。

## 11. 错误与审计

公共错误为可分类错误，正文不泄露本机路径或环境值：

```go
var (
    ErrSkillNotFound       = errors.New("skill not found")
    ErrSkillAmbiguous      = errors.New("skill name is ambiguous")
    ErrSkillUnavailable    = errors.New("skill is unavailable")
    ErrInvalidSkillManifest = errors.New("invalid skill manifest")
    ErrSkillDigestMismatch = errors.New("skill content changed")
    ErrInvalidSkillResource = errors.New("invalid skill resource")
)
```

host 可审计的事件：

- `skill.cataloged`：name、root ID、digest、availability；
- `skill.activation_requested`：workflow/run、`name@digest`；
- `skill.activation_rejected`：稳定 reason code；
- `skill.activated`：有效 required/optional tool ID；
- `skill.resource_read`：resource URI、字节数；
- `skill.digest_mismatch`：name、期待和观察到的 digest。

审计不保存 Skill 正文、资源内容、环境变量值、密钥或物理路径。

## 12. 验证策略

### 12.1 `skillkit` 单元测试

- 接受最小 Agent Skills `SKILL.md`，并校验 name/目录名/description；
- 解析 `metadata.goagents`，未知 metadata 不影响可移植性；
- 目录冲突同 digest 可去重、不同 digest 必须歧义；
- OS、feature、required/optional tool 与 environment probe 的准入矩阵；
- scan 与 activation 均无进程启动、无包安装、无网络请求；
- 资源 allowlist、`..`、绝对路径和越界 symlink 的拒绝；
- package mutation 后 digest mismatch；
- catalog 列表确定性排序。

### 12.2 Agent/host 集成测试

- 没有 `skill_refs` 时，现有 Agent prompt 和工具集不变；
- 已激活 Skill 正文只在对应 run 的 prompt 中出现；
- required tool 未授权时，Skill 不能激活且工具不进入 registry；
- optional tool 未授权时，Skill 可激活但不暴露该工具；
- 从 Skill 指导发起的写工具调用仍进入既有 OIDC approval、expiry、lease conflict 与安全重试闭环；
- SQLite 重启/requeue 后以相同 `name@digest` 重放，缺失 digest 时失败关闭。

### 12.3 评估门禁

`examples/host-api/skill_eval_test.go` 已实现 host-side `evalkit` 安全套件，并在发布前验证：

- 同名恶意 Skill 不能遮蔽受信任 Skill；
- Skill 正文中的提示注入不能扩大可见工具集；
- Skill 请求未授权工具、未允许资源或安装依赖时被拒绝；
- 同一 workflow 的两次运行使用相同 digest 与可见工具集。

## 13. 实施切片

1. **Catalog slice（已完成）：** 新建 `skillkit` module，实现 manifest、roots、scan、digest、冲突和 availability；只提供 Go API 与单元测试，不接 Agent。
2. **Activation slice（已完成）：** 实现 `SkillRef`、run-start gating、`agentcore.SkillProvider` adapter 和安全资源 resolver；不增加脚本执行器。
3. **Host slice（已完成）：** 在 `examples/host-api` 暴露 Skill 列表与 workflow `skill_refs`，将解析后的 `name@digest` 写入 SQLite metadata，补 restart/requeue smoke。
4. **Evaluation slice（已完成）：** 将 Skill 选择、授权和内容漂移案例加入 host-side `evalkit` 发布门禁。

每个切片单独语义提交并运行 `bash ./scripts/verify-all.sh`。只有第 2 个切片稳定后，才评估动态激活协议；只有存在真实受控脚本场景时，才单独设计执行器/沙箱。

## 14. 参考资料

- Agent Skills 格式与渐进披露：[Open Agent Skills Specification](https://openagentskills.dev/docs/specification)
- OpenAI 对可复用工作流和 `SKILL.md` 的说明：[Using skills](https://openai.com/academy/skills/)
- 参考的 discovery/gating 实现：[smallnest/goclaw README](https://github.com/smallnest/goclaw#%E6%8A%80%E8%83%BD%E7%B3%BB%E7%BB%9F-new)、[agent/skills.go](https://github.com/smallnest/goclaw/blob/master/agent/skills.go)
- 按调用者授权确定工具可见集的原则：[MCP Tools specification](https://modelcontextprotocol.io/specification/draft/server/tools)
