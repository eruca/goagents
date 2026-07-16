# Skillkit Activation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让宿主能以 `name@digest` 在 run start 激活已准入的 Skill、按 allowlist 读取资源，并将正文安全适配为现有 `agentcore.SkillProvider`。

**Architecture:** 保持 `skillkit` 的 catalog 记录私有 canonical skill root，仅在激活或资源读取时重新计算 digest。Activation 是不可变的宿主对象，既不暴露物理路径，也不执行脚本；`agentadapter` 只将已激活正文映射为 `agentcore.Skill`，不注册、删除或扩大工具。

**Tech Stack:** Go 1.26.1、`net/url`、标准测试包、既有 `github.com/eruca/goagents/goagent/agentcore`。

## Global Constraints

- 不修改 `goagent/agentcore`；既有 `ToolProvider` 只可追加 tool，不能安全收缩 base registry，工具投影留给后续 host slice。
- Activation 只能使用 `Catalog` 已发现的 `Ref`；每次激活和资源读取必须重新校验 package digest。
- 所有失败关闭错误只返回稳定 sentinel/reason，不泄露绝对路径、资源内容或环境值。
- `SKILL.md` 正文最多 128 KiB；发现超限正文时标记为 invalid。
- 资源 URI 固定为 `skill://<name>@<digest>/<allowlisted-relative-path>`；只返回最多 1 MiB 的字节流。
- 不执行 `scripts/`、不安装依赖、不发网络请求、不修改环境。
- 每个生产行为先写测试并观察预期 RED，再写最小实现。

---

## File Structure

- Modify: `skillkit/catalog.go` — 私有记录保留 canonical skill root，公开 `Entry` 保持无路径。
- Modify: `skillkit/catalog_test.go` — 覆盖 catalog 的私有记录仍不泄露路径。
- Create: `skillkit/activation.go` — run-start 激活、digest recheck、正文提取、受限资源 URI resolver。
- Create: `skillkit/activation_test.go` — 激活、漂移、allowlist、URI 越界、大小上限与无路径泄露。
- Modify: `skillkit/errors.go` — 增加稳定的 unavailable/digest mismatch sentinel。
- Modify: `skillkit/go.mod`, `skillkit/go.sum` — 加入可选 adapter 所需的 local `goagent` module。
- Create: `skillkit/agentadapter/provider.go` — `ActivationResolver` 到 `agentcore.SkillProvider` 的薄适配。
- Create: `skillkit/agentadapter/provider_test.go` — provider 注入正文并透传 resolver 失败。
- Modify: `skillkit/README.md` — 更新 activation、URI 与不做工具投影的边界。

## Public Interfaces

```go
package skillkit

type ActivationRequest struct {
	Skills      []Ref
	GateContext GateContext
}

type ActivatedSkill struct {
	Ref             Ref
	Name            string
	Description     string
	Content         string
	RequiredToolIDs []string
	OptionalToolIDs []string
}

type Activation struct { /* private records only */ }

func (c *Catalog) Activate(request ActivationRequest) (*Activation, error)
func (a *Activation) Skills() []ActivatedSkill
func (a *Activation) ResourceURI(ref Ref, resource string) (string, error)
func (a *Activation) ReadResource(uri string) ([]byte, error)
```

```go
package agentadapter

type ActivationResolver func(context.Context, agentcore.RunRequest) (*skillkit.Activation, error)

type Provider struct {
	Resolve ActivationResolver
}

func (p Provider) Skills(context.Context, agentcore.RunRequest) ([]agentcore.Skill, error)
```

### Task 1: 为 catalog 保留私有 activation record

**Files:**
- Modify: `skillkit/catalog.go`
- Modify: `skillkit/catalog_test.go`

**Interfaces:**
- Consumes: `Root`、`Entry`、`packageDigest`、现有 conflict collapse。
- Produces: `Catalog` 内可按完整 `Ref` 查询的私有 canonical root；`List` 与 `Resolve` 的公开行为不变。

- [x] **Step 1: 写失败测试，锁定私有 root 记录**

在 `catalog_test.go` 增加 `TestCatalogRetainsPrivateRecordWithoutExposingPath`：发现一个临时 skill，确认 `catalog.Resolve` 的格式化输出不包含临时 root；再由下一 task 的 `Activate` 使用相同 catalog 成功读取正文。测试应只依赖公开 API。

- [x] **Step 2: 运行测试确认 RED**

Run: `(cd skillkit && go test ./... -run TestCatalogRetainsPrivateRecordWithoutExposingPath -count=1)`

Expected: FAIL，原因是 `Catalog.Activate` 未定义。

- [x] **Step 3: 最小重构 catalog 内部表示**

在 `catalog.go` 增加未导出的 `catalogRecord{entry Entry, skillPath string}`；`Discover` 以 record 聚合与去重，选择已存在的“trusted 优先、root ID 次序”规则对应路径。公开 `Catalog.List`、`Resolve` 继续从 cloned `Entry` 返回，任何 public DTO 和 error 都不出现 `skillPath`。

- [x] **Step 4: 运行既有 catalog 测试**

Run: `(cd skillkit && go test ./... -run TestDiscover -count=1)`

Expected: PASS。

### Task 2: 实现 fail-closed activation 与资源 resolver

**Files:**
- Create: `skillkit/activation.go`
- Create: `skillkit/activation_test.go`
- Modify: `skillkit/errors.go`

**Interfaces:**
- Consumes: Task 1 的私有 record、`Evaluate`、`readContainedFile` 与 `packageDigest`。
- Produces: 不含路径的 `Activation` 和 `ActivatedSkill`；`ResourceURI`、`ReadResource`。

- [x] **Step 1: 写 activation 的失败测试**

在 `activation_test.go` 创建真实临时 root，并覆盖：

```go
func TestActivateLoadsOnlyRequestedSkillBody(t *testing.T) {
	catalog := discoverTwoSkills(t)
	activation, err := catalog.Activate(ActivationRequest{
		Skills: []Ref{{Name: "clinical-summary"}},
		GateContext: GateContext{AllowedToolIDs: map[string]bool{"artifact.read": true}},
	})
	if err != nil { t.Fatalf("Activate: %v", err) }
	skills := activation.Skills()
	if len(skills) != 1 || skills[0].Name != "clinical-summary" || strings.Contains(skills[0].Content, "metadata:") {
		t.Fatalf("skills = %#v", skills)
	}
}
```

另写：

- `TestActivateRejectsUnavailableSkill`：缺 required tool 时 `errors.Is(err, ErrSkillUnavailable)`；
- `TestActivateRejectsChangedPackageDigest`：发现后修改 allowlisted resource，激活返回 `ErrSkillDigestMismatch`；
- `TestActivationReadsOnlyAllowedResource`：由 `ResourceURI` 生成 URI，可读 allowlisted 内容；未激活 ref、`../`、绝对路径与未允许资源均失败；
- `TestActivationRejectsChangedResourceAndOversizedRead`：激活后修改资源或写入超过 1 MiB 的文件，失败关闭；
- `TestActivationErrorsDoNotLeakRootPath`：所有上述 error 文本不含临时目录。
- `TestDiscoverRejectsOversizedSkillBody`：正文超过 128 KiB 时 catalog 保留 invalid entry。

- [x] **Step 2: 运行 activation 测试确认 RED**

Run: `(cd skillkit && go test ./... -run 'TestActivate|TestActivation' -count=1)`

Expected: FAIL，原因是 activation API 和 errors 未定义。

- [x] **Step 3: 实现最小 activation**

在 `errors.go` 添加：

```go
ErrSkillUnavailable    = errors.New("skill is unavailable")
ErrSkillDigestMismatch = errors.New("skill content changed")
```

在 `activation.go`：

1. 逐个 `Resolve` 请求的 ref；空 digest 仅沿用 `Resolve` 的唯一 ready 规则，`ActivatedSkill.Ref` 永远返回完整 digest；
2. 对每个 resolved entry 运行 `Evaluate`；非 eligible 返回包装后的 `ErrSkillUnavailable`，不返回物理路径；
3. 重新读取 `SKILL.md`、解析当前 manifest、以当前 allowlist 计算 digest；任意读取/解析失败或 digest 不同返回 `ErrSkillDigestMismatch`；
4. 从 frontmatter closing delimiter 后提取正文，拒绝空正文，按 name/digest 稳定排序；
5. `ResourceURI` 只接受当前 activation 的完整 ref 和 manifest allowlist path；`ReadResource` 严格 parse `skill://name@digest/path`，重新校验 digest、allowlist、regular file 与 1 MiB 上限，再返回内容副本。

- [x] **Step 4: 运行 activation 测试确认 GREEN**

Run: `(cd skillkit && go test ./... -run 'TestActivate|TestActivation' -count=1)`

Expected: PASS。

### Task 3: 添加可选 agentcore SkillProvider adapter

**Files:**
- Modify: `skillkit/go.mod`
- Modify: `skillkit/go.sum`
- Create: `skillkit/agentadapter/provider.go`
- Create: `skillkit/agentadapter/provider_test.go`

**Interfaces:**
- Consumes: `Activation.Skills` 与 `agentcore.SkillProvider`。
- Produces: 由 host 提供 request-scoped `ActivationResolver` 的薄 `Provider`；无 tool registry API。

- [x] **Step 1: 写 adapter 失败测试**

在 `agentadapter/provider_test.go` 写入：

```go
func TestProviderMapsActivatedSkillToAgentcoreSkill(t *testing.T) {
	activation := activatedSkill(t, "clinical-summary", "Use approved sources only.")
	provider := Provider{Resolve: func(context.Context, agentcore.RunRequest) (*skillkit.Activation, error) {
		return activation, nil
	}}
	skills, err := provider.Skills(context.Background(), agentcore.RunRequest{})
	if err != nil { t.Fatalf("Skills: %v", err) }
	if len(skills) != 1 || skills[0].Name != "clinical-summary" || skills[0].Content != "Use approved sources only." {
		t.Fatalf("skills = %#v", skills)
	}
}
```

另写 `TestProviderPropagatesResolverError` 与 compile-time `var _ agentcore.SkillProvider = Provider{}`。测试不得注册或执行 tool。

- [x] **Step 2: 运行 adapter 测试确认 RED**

Run: `(cd skillkit && go test ./agentadapter -count=1)`

Expected: FAIL，原因是 package/`Provider` 未定义。

- [x] **Step 3: 写最小 adapter 与 module 接线**

在 `skillkit/go.mod` 加入与其他 sibling module 相同的 `goagent` require/replace。实现 `Provider.Skills`：nil resolver 返回稳定 error；调用 resolver；把 `ActivatedSkill` 映射为 `agentcore.Skill{Name, Description, Content, Cacheable: true}`。不得实现 `ToolProvider`，不得读取 `RunRequest.Metadata`，不得修改 tool registry。

- [x] **Step 4: 运行 adapter 测试确认 GREEN**

Run: `(cd skillkit && go test ./agentadapter -count=1)`

Expected: PASS。

### Task 4: 文档、全量验证与语义提交

**Files:**
- Modify: `skillkit/README.md`
- Modify: `docs/superpowers/plans/2026-07-12-skillkit-activation-implementation.md`

- [x] **Step 1: 更新 README**

将 current scope 改为已支持 catalog、gate、run-start activation、受限资源 URI 与 `agentadapter.Provider`；明确仍未实现 host API、workflow 持久化、动态激活、工具投影和脚本执行。

- [x] **Step 2: 整理与模块验证**

Run:

```bash
(cd skillkit && gofmt -w *.go agentadapter/*.go)
(cd skillkit && go mod tidy)
(cd skillkit && go test ./...)
```

Expected: PASS。

- [x] **Step 3: 全 workspace 验证**

Run:

```bash
git diff --check
bash ./scripts/verify-all.sh
rg -n 'os/exec|exec\.Command|net/http|http\.Get' skillkit
```

Expected: 前两项 exit 0；最后一项无 production hit。

- [x] **Step 4: 仅提交 activation 切片**

```bash
git add skillkit docs/superpowers/plans/2026-07-12-skillkit-activation-implementation.md
git commit -m "feat(skillkit): 支持技能激活与受限资源读取"
```

Expected: commit 不包含 host API、SQLite schema、动态 tool registry 或脚本执行器。

## Plan Self-Review

- **Spec coverage:** 设计第 9 节的 run-start resolve/gate/body adapter 与第 10 节的 URI/resource/digest boundary 分别由 Task 2 和 Task 3 覆盖。
- **Scope check:** 工具投影依赖当前 `agentcore` 的 base registry 收缩能力；本计划不伪造该能力，保留给 host slice 通过真实 request-scoped registry 接线。
- **Failure behavior:** 所有 catalog mutation、unavailable requirement、URI 越界和大资源路径都有显式 fail-closed tests。
