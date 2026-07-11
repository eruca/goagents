# Skillkit Catalog Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新建独立 `skillkit` Go module，安全发现本机 `SKILL.md` 包，生成可复现 catalog，并对当前宿主环境计算可激活状态。

**Architecture:** `skillkit` 只解析、校验、发现和准入；它不依赖 `goagent`，不执行脚本，也不安装依赖。manifest parser 解析标准前置字段与 `metadata.goagents` 扩展；catalog 在私有 canonical path 上计算 package digest，而公开 `Entry` 不暴露物理路径；gate 是纯函数，按 root 信任、OS、host feature 与已授权工具生成稳定 reason code。

**Tech Stack:** Go 1.26.1、标准库（`crypto/sha256`、`path/filepath`、`os`）、`gopkg.in/yaml.v3 v3.0.1`、Go 标准测试包。

## Global Constraints

- 创建 `github.com/eruca/skillkit`，不得 import `goagent`、`workflowkit`、`runkit`、`llmkit`、`mcpkit` 或 host example。
- 仅识别 `<root>/<name>/SKILL.md`；名称须匹配 `[a-z0-9-]{1,64}` 且等于目录名。
- 未知标准 `metadata` 必须保留；只有 `metadata.goagents` 被解释。
- 发现过程不得执行进程、网络请求、依赖安装或修改环境。
- 同名不同 digest 必须歧义，不能采用路径优先级静默覆盖。
- 不导出 Skill 根目录物理路径；资源 resolver、Agent adapter、host API 和脚本执行都不属于本切片。
- 每个行为先写失败测试并观察预期 RED，再添加最小实现。

---

## File Structure

- Create: `skillkit/go.mod` — 独立 module 与 YAML 依赖。
- Create: `skillkit/doc.go` — package boundary 注释。
- Create: `skillkit/errors.go` — 可分类、无路径泄露的公开错误。
- Create: `skillkit/manifest.go` — YAML frontmatter、manifest 与扩展 requirements。
- Create: `skillkit/manifest_test.go` — manifest 的解析与拒绝行为。
- Create: `skillkit/catalog.go` — root discovery、digest、冲突与公开 catalog API。
- Create: `skillkit/catalog_test.go` — 真实临时目录下的 scan/digest/conflict/symlink 行为。
- Create: `skillkit/gate.go` — 纯 availability evaluator。
- Create: `skillkit/gate_test.go` — 信任、OS、feature、required/optional tool 矩阵。
- Create: `skillkit/README.md` — 用法、边界和验证方式。
- Modify: `go.work` — 添加 `./skillkit`。
- Modify: `docs/modules.md` — 列出独立 core-adjacent capability module 及依赖规则。
- Modify: `scripts/verify-all.sh` — 添加 `(cd skillkit && go test ./...)`。
- Create: `skillkit/go.sum` — 由 `go mod tidy` 生成。

## Public Interfaces

```go
package skillkit

type Scope string

const (
	ScopeBuiltin   Scope = "builtin"
	ScopeUser      Scope = "user"
	ScopeWorkspace Scope = "workspace"
)

type Root struct {
	ID      string
	Dir     string
	Scope   Scope
	Trusted bool
	Enabled bool
}

type Requirements struct {
	OS           []string
	HostFeatures []string
	RequiredToolIDs []string
	OptionalToolIDs []string
}

type Manifest struct {
	Name        string
	Description string
	License     string
	Requirements Requirements
	Resources   []string
	Metadata    map[string]any
}

type Ref struct {
	Name   string
	Digest string
}

type EntryState string

const (
	EntryReady     EntryState = "ready"
	EntryInvalid   EntryState = "invalid"
	EntryAmbiguous EntryState = "ambiguous"
)

type Entry struct {
	Ref      Ref
	Manifest Manifest
	RootID   string
	SourceRootIDs []string
	Scope    Scope
	Trusted  bool
	State    EntryState
	Reasons  []Reason
}

type Catalog struct { /* private records */ }

func Discover(roots []Root) (*Catalog, error)
func (c *Catalog) List() []Entry
func (c *Catalog) Resolve(ref Ref) (Entry, error)

type Availability string

const (
	AvailabilityEligible    Availability = "eligible"
	AvailabilityUnavailable Availability = "unavailable"
	AvailabilityInvalid     Availability = "invalid"
	AvailabilityAmbiguous   Availability = "ambiguous"
)

type GateContext struct {
	OS             string
	HostFeatures   map[string]bool
	AllowedToolIDs map[string]bool
}

type Reason struct {
	Code    string
	Subject string
}

type AvailabilityReport struct {
	State   Availability
	Reasons []Reason
}

func Evaluate(entry Entry, ctx GateContext) AvailabilityReport
```

`Entry` 不含 root path、manifest file path 或 resource path。`Ref.Digest` 为完整 64 位十六进制 SHA-256；`Resolve(Ref{Name: name})` 只有在该 name 恰有一个 `EntryReady` digest 时才成功。

### Task 1: 建立 module 与 workspace 接线

**Files:**
- Create: `skillkit/go.mod`
- Create: `skillkit/doc.go`
- Modify: `go.work`
- Modify: `docs/modules.md`
- Modify: `scripts/verify-all.sh`

**Interfaces:**
- Consumes: workspace Go 1.26.1 与现有 `gopkg.in/yaml.v3 v3.0.1` 版本。
- Produces: 可通过 `(cd skillkit && go test ./...)` 执行的独立 package；本 task 不引入业务 API。

- [x] **Step 1: 创建 module 声明与最小 package 注释**

创建 `skillkit/go.mod`：

```go
module github.com/eruca/skillkit

go 1.26.1

require gopkg.in/yaml.v3 v3.0.1
```

创建 `skillkit/doc.go`：

```go
// Package skillkit discovers and gates portable SKILL.md packages.
//
// It does not execute skill scripts, install dependencies, or grant tools.
package skillkit
```

- [x] **Step 2: 将 module 纳入 workspace 与验证链**

在 `go.work` 的 `use` 块加入 `./skillkit`；在 `scripts/verify-all.sh` 的 `runkit` 后加入：

```bash
run_in "$ROOT/skillkit" go test ./...
```

在 `docs/modules.md` 的 core modules 下新增 `github.com/eruca/skillkit`，并在依赖规则中说明：`skillkit` 不 import 其他 workspace module；只有未来可选 adapter 才可依赖 `goagent`。

- [x] **Step 3: 验证空 module 可构建**

Run: `(cd skillkit && go mod tidy && go test ./...)`

Expected: PASS，且只生成 `skillkit/go.sum`；根目录不产生 module 文件。

### Task 2: 实现并锁定 manifest 契约

**Files:**
- Create: `skillkit/errors.go`
- Create: `skillkit/manifest.go`
- Create: `skillkit/manifest_test.go`

**Interfaces:**
- Consumes: Task 1 的 YAML 依赖。
- Produces: `ParseManifest(directory string, source []byte) (Manifest, error)` 与 `ValidateManifest(directory string, manifest Manifest) error`，供 catalog 在读取 `SKILL.md` 后调用。

- [x] **Step 1: 写失败测试，表达可移植 manifest 的最小输入**

在 `manifest_test.go` 写入：

```go
func TestParseManifestReadsPortableFieldsAndGoagentsRequirements(t *testing.T) {
	source := []byte("---\n" +
		"name: clinical-summary\n" +
		"description: Produce a bounded summary.\n" +
		"license: Apache-2.0\n" +
		"metadata:\n" +
		"  vendor: preserved\n" +
		"  goagents:\n" +
		"    requires:\n" +
		"      os: [darwin]\n" +
		"      host_features: [artifacts.v1]\n" +
		"      tools:\n" +
		"        required: [artifact.read]\n" +
		"        optional: [web.search]\n" +
		"    resources:\n" +
		"      allow: [references/schema.md]\n" +
		"---\n# Instructions\n")

	manifest, err := ParseManifest("clinical-summary", source)
	if err != nil { t.Fatalf("ParseManifest: %v", err) }
	if manifest.Name != "clinical-summary" || manifest.License != "Apache-2.0" { t.Fatalf("manifest = %#v", manifest) }
	if got := manifest.Requirements.RequiredToolIDs; !reflect.DeepEqual(got, []string{"artifact.read"}) { t.Fatalf("required = %#v", got) }
	if got := manifest.Resources; !reflect.DeepEqual(got, []string{"references/schema.md"}) { t.Fatalf("resources = %#v", got) }
	if manifest.Metadata["vendor"] != "preserved" { t.Fatalf("metadata = %#v", manifest.Metadata) }
}
```

另写 table-driven `TestParseManifestRejectsInvalidNameDescriptionAndResource`，覆盖 name 与目录不一致、非小写连字符 name、空 description、超过 280 字符 description、没有 frontmatter、`../secret`、绝对资源路径和重复资源路径。

- [x] **Step 2: 运行测试确认 RED**

Run: `(cd skillkit && go test ./... -run 'TestParseManifest')`

Expected: FAIL，原因是 `ParseManifest` 未定义；不得因为 YAML 语法错误或测试目录错误失败。

- [x] **Step 3: 只实现 manifest 解析与验证**

在 `errors.go` 定义：

```go
var (
	ErrInvalidSkillManifest = errors.New("invalid skill manifest")
	ErrInvalidSkillResource = errors.New("invalid skill resource")
)
```

在 `manifest.go` 实现以下规则：

```go
func ParseManifest(directory string, source []byte) (Manifest, error) {
	frontmatter, err := frontmatterDocument(source)
	if err != nil { return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidSkillManifest, err) }
	manifest := manifestFromDocument(frontmatter)
	if err := ValidateManifest(directory, manifest); err != nil { return Manifest{}, err }
	return manifest, nil
}
```

`frontmatterDocument` 必须只接受首行 `---` 与第二个单独 `---` 之间的 YAML；使用 `yaml.Node` 或 `map[string]any` 保留未知 `metadata`。`metadata.goagents.requires.tools.required`、`optional`、`os`、`host_features` 和 `metadata.goagents.resources.allow` 只接受 string list，排序并去重。`ValidateManifest` 执行 Task 2 Step 1 的所有拒绝条件，并以 `%w` 包装对应公开错误。

- [x] **Step 4: 运行 manifest 测试确认 GREEN**

Run: `(cd skillkit && go test ./... -run 'TestParseManifest')`

Expected: PASS，所有允许与拒绝断言通过。

### Task 3: 实现 catalog discovery、digest 与冲突

**Files:**
- Create: `skillkit/catalog.go`
- Create: `skillkit/catalog_test.go`

**Interfaces:**
- Consumes: `ParseManifest`、`Root`、`Manifest`、公开 errors。
- Produces: `Discover`、`Catalog.List`、`Catalog.Resolve`、`Entry`、`Ref` 与 `EntryState`。

- [x] **Step 1: 写失败测试，固定 discovery 的安全输出**

在 `catalog_test.go` 创建临时 `builtin/clinical-summary/SKILL.md`、`builtin/clinical-summary/references/schema.md`，其 manifest allowlist 该资源；然后写：

```go
func TestDiscoverBuildsSortedEntryWithDigestWithoutLeakingPath(t *testing.T) {
	root := writeSkill(t, "builtin", "clinical-summary", validManifest("clinical-summary", "references/schema.md"))
	catalog, err := Discover([]Root{{ID: "builtin", Dir: root, Scope: ScopeBuiltin, Trusted: true, Enabled: true}})
	if err != nil { t.Fatalf("Discover: %v", err) }
	entries := catalog.List()
	if len(entries) != 1 || entries[0].State != EntryReady { t.Fatalf("entries = %#v", entries) }
	if entries[0].Ref.Name != "clinical-summary" || len(entries[0].Ref.Digest) != 64 { t.Fatalf("entry = %#v", entries[0]) }
	if strings.Contains(fmt.Sprintf("%#v", entries[0]), root) { t.Fatalf("entry leaks root path: %#v", entries[0]) }
	resolved, err := catalog.Resolve(Ref{Name: "clinical-summary", Digest: entries[0].Ref.Digest})
	if err != nil || resolved.Ref != entries[0].Ref { t.Fatalf("Resolve = %#v, %v", resolved, err) }
}
```

再写：

- `TestDiscoverMarksDifferentDigestsWithSameNameAmbiguous`：两个 enabled trusted roots 下同名但正文不同，`List` 的两个 entry 均为 `EntryAmbiguous`，裸 name 的 `Resolve` 返回 `ErrSkillAmbiguous`；
- `TestDiscoverCollapsesEqualDigestAndRetainsSourceRoots`：两个 roots 中同名且内容相同的包只产生一个 ready entry，`SourceRootIDs` 按字典序保留两个 root ID；
- `TestDiscoverLeavesInvalidManifestVisibleButUnresolvable`：子目录中 manifest 无效，entry state 为 `EntryInvalid`，带 `invalid_manifest` reason；
- `TestDiscoverRejectsAllowedResourceOutsideSkillRoot`：allowlist 使用 `../outside.md` 或越界 symlink，entry 为 `EntryInvalid` 且不计算 digest；
- `TestDiscoverDigestChangesWhenAllowedResourceChanges`：修改 allowlisted resource 后第二次 `Discover` 产生不同 digest；
- `TestDiscoverIgnoresDisabledRoot`：`Enabled=false` 的 root 绝不出现在 `List`。

- [x] **Step 2: 运行 catalog 测试确认 RED**

Run: `(cd skillkit && go test ./... -run 'TestDiscover')`

Expected: FAIL，原因是 `Discover`、`Root`、`Entry` 或 `Ref` 尚未定义。

- [x] **Step 3: 只实现无副作用的 scanner**

在 `catalog.go` 实现：

```go
func Discover(roots []Root) (*Catalog, error) {
	// 只读取启用 root 的直接子目录；不执行命令，不修改文件或环境。
}

func (c *Catalog) List() []Entry {
	// 返回深拷贝，按 Name、Digest、RootID 稳定排序。
}

func (c *Catalog) Resolve(ref Ref) (Entry, error) {
	// digest 指定时精确匹配；未指定 digest 时只接受唯一 EntryReady。
}
```

scanner 必须：

1. canonicalize 每个 root；root ID 为空、dir 为空、scope 非法或 enabled root 不可读时让 `Discover` 返回配置错误；
2. 只遍历直接子目录，且只读取准确大小写的 `SKILL.md`；
3. 对单个无效 Skill 创建 `EntryInvalid`，不阻断其余 Skill；
4. 以 `SKILL.md` 与 manifest `Resources` allowlist 的文件内容计算 SHA-256；相对路径排序后编码 `path + NUL + content + NUL`；
5. 对每个 allowlisted resource 先拒绝绝对路径、`..` 和不可解析链接，`EvalSymlinks` 后确认仍在 canonical skill root 内；
6. 把物理路径只保存在未导出的 record 中；`Entry` 和 errors 只能含 stable reason code/subject；
7. 所有有效 entry 完成 scan 后按 name 聚类，存在两个不同 digest 时把该 name 的全部 entry 置为 `EntryAmbiguous` 并写入 `duplicate_name` reason。

公开 errors 在 `errors.go` 增加：

```go
var (
	ErrSkillNotFound  = errors.New("skill not found")
	ErrSkillAmbiguous = errors.New("skill name is ambiguous")
)
```

- [x] **Step 4: 运行 catalog 测试确认 GREEN**

Run: `(cd skillkit && go test ./... -run 'TestDiscover')`

Expected: PASS，且测试不会产生 root 目录以外的文件。

### Task 4: 实现纯 availability gate

**Files:**
- Create: `skillkit/gate.go`
- Create: `skillkit/gate_test.go`

**Interfaces:**
- Consumes: Task 2 的 `Requirements` 和 Task 3 的 `Entry` state/trust。
- Produces: `Evaluate(entry Entry, ctx GateContext) AvailabilityReport`；不访问文件系统、环境变量、网络或工具 registry。

- [x] **Step 1: 写失败测试，表达 fail-closed 准入矩阵**

在 `gate_test.go` 写入：

```go
func TestEvaluateRequiresTrustedRootOSFeatureAndRequiredTool(t *testing.T) {
	entry := Entry{
		State: EntryReady, Trusted: true,
		Manifest: Manifest{Requirements: Requirements{
			OS: []string{"darwin"}, HostFeatures: []string{"artifacts.v1"},
			RequiredToolIDs: []string{"artifact.read"}, OptionalToolIDs: []string{"web.search"},
		}},
	}
	report := Evaluate(entry, GateContext{OS: "darwin", HostFeatures: map[string]bool{"artifacts.v1": true}, AllowedToolIDs: map[string]bool{"artifact.read": true}})
	if report.State != AvailabilityEligible { t.Fatalf("report = %#v", report) }
	report = Evaluate(entry, GateContext{OS: "linux", HostFeatures: map[string]bool{}, AllowedToolIDs: map[string]bool{}})
	if report.State != AvailabilityUnavailable || !hasReason(report.Reasons, "unsupported_os", "linux") || !hasReason(report.Reasons, "missing_tool", "artifact.read") { t.Fatalf("report = %#v", report) }
}
```

另写：

- `TestEvaluateDoesNotBlockOnMissingOptionalTool`；
- `TestEvaluateRejectsUntrustedInvalidAndAmbiguousEntries`；
- `TestEvaluateSortsReasons`，固定按 code、subject 排序，确保 audit/UI 输出可重现。

- [x] **Step 2: 运行 gate 测试确认 RED**

Run: `(cd skillkit && go test ./... -run 'TestEvaluate')`

Expected: FAIL，原因是 `Evaluate` 与 availability DTO 未定义。

- [x] **Step 3: 实现最小纯函数 gate**

在 `gate.go` 实现公开 DTO 与下面的优先规则：

```go
func Evaluate(entry Entry, ctx GateContext) AvailabilityReport {
	// EntryInvalid -> invalid，EntryAmbiguous -> ambiguous；
	// EntryReady 但 !Trusted -> unavailable(untrusted_root)；
	// 其余缺失 OS/feature/required tool 累积 unavailable reasons；
	// optional tool 不添加 failure reason；没有 failure 才 eligible。
}
```

`Reason` 只包含 `Code` 与 `Subject`，允许的 code 为 `invalid_manifest`、`duplicate_name`、`untrusted_root`、`unsupported_os`、`missing_feature`、`missing_tool`。在返回前排序并深拷贝 slice/map，防止调用者修改 catalog 内状态。

- [x] **Step 4: 运行 gate 测试确认 GREEN**

Run: `(cd skillkit && go test ./... -run 'TestEvaluate')`

Expected: PASS，且 gate 无 I/O 依赖。

### Task 5: 补齐文档、全量验证并按语义提交

**Files:**
- Create: `skillkit/README.md`
- Modify: `docs/modules.md`
- Modify: `scripts/verify-all.sh`
- Modify: `go.work`
- Modify: `go.work.sum`（仅当 `go work sync` 或全量验证产生必要和可解释的 checksum 变化）

**Interfaces:**
- Consumes: Task 1–4 的稳定 API。
- Produces: 外部使用者可理解 module 边界、catalog/gate 使用方式与验证路径。

- [x] **Step 1: 写 README 使用示例与安全边界**

README 必须包含：

```go
catalog, err := skillkit.Discover([]skillkit.Root{{
	ID: "workspace", Dir: ".agents/skills", Scope: skillkit.ScopeWorkspace,
	Trusted: true, Enabled: true,
}})
if err != nil { return err }

entry, err := catalog.Resolve(skillkit.Ref{Name: "clinical-summary"})
if err != nil { return err }

report := skillkit.Evaluate(entry, skillkit.GateContext{
	OS: runtime.GOOS,
	HostFeatures: map[string]bool{"artifacts.v1": true},
	AllowedToolIDs: map[string]bool{"artifact.read": true},
})
if report.State != skillkit.AvailabilityEligible { return fmt.Errorf("skill unavailable: %#v", report.Reasons) }
```

并明确：catalog 不执行 `scripts/`、不安装依赖、不授予工具、不导出真实路径；Agent adapter、resource resolver、host API 是后续独立切片。

- [x] **Step 2: 运行模块级格式化和测试**

Run:

```bash
(cd skillkit && gofmt -w *.go)
(cd skillkit && go test ./...)
```

Expected: PASS。

- [x] **Step 3: 运行 workspace 边界和完整验证**

Run:

```bash
git diff --check
bash ./scripts/verify-all.sh
```

Expected: 两个命令均以 exit 0 完成，输出 `goagents workspace verification passed`。

- [ ] **Step 4: 核对仅包含 catalog 切片的差异并提交**

Run:

```bash
git status --short
git diff --check
git add go.work go.work.sum docs/modules.md scripts/verify-all.sh skillkit
git commit -m "feat(skillkit): 新增安全技能目录与准入"
```

Expected: commit 只包含新 `skillkit` module、workspace 接线与本任务文档；不包含 host API、Agent adapter、脚本执行器或用户已有改动。

## Plan Self-Review

- **Spec coverage：** catalog 的格式、roots、digest、冲突、准入、无副作用、独立 module、验证和提交分别由 Task 2、3、4、1/5 覆盖；资源实际读取、Agent wiring、host API 和评估门禁均按设计明确不在本切片。
- **Placeholder scan：** 本计划没有未定义的后续任务占位；每个行为都有明确文件、函数、测试或命令。
- **Type consistency：** `Manifest`/`Requirements` 由 Task 2 产出，`Entry`/`Catalog` 由 Task 3 产出，`Evaluate` 在 Task 4 消费它们；Task 5 只展示前述稳定 API。
