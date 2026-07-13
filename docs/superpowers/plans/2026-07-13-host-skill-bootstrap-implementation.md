# Host Skill Bootstrap v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the default local `host-api` process discover and activate instruction-only Skills from one explicitly trusted local directory while preserving exact-digest replay and fail-closed capability boundaries.

**Architecture:** Add a host-only startup helper that converts `HOST_API_SKILL_ROOT` into the existing `SkillCatalog` and an OS-only `SkillGateContext`, then injects both through the unchanged `Config` contract. Reuse the existing HTTP, SQLite `skill_refs`, activation, and eval paths; add one real Skill package plus an independent real-process restart smoke.

**Tech Stack:** Go 1.26.1, `skillkit`, `examples/host-api`, SQLite-backed `workflowkit`/`runkit`, `httptest`, macOS tagged process smoke.

## Global Constraints

- `HOST_API_SKILL_ROOT` is optional; empty means Skill bootstrap is disabled.
- A non-empty root must be absolute and identify one readable directory.
- The configured root uses constant ID `local-user-skills`, `ScopeUser`, `Trusted: true`, and `Enabled: true`.
- Default CLI `SkillGateContext` contains only `OS: runtime.GOOS`; `HostFeatures` and `AllowedToolIDs` stay empty.
- Do not change `NewServer(Config)` or accept root/trust/gate settings from HTTP requests.
- Do not add multi-root config, auto-discovery, refresh, scripts, installers, dynamic activation, tool projection, or multi-agent behavior.
- Errors and API responses must not expose the root path, Skill body, resources, or environment variable value.
- Use TDD for production and test-helper behavior; commit each independently reviewable task with a Simplified Chinese message.

---

### Task 1: Load one explicit Skill root during host startup

**Files:**

- Create: `examples/host-api/skill_bootstrap.go`
- Create: `examples/host-api/skill_bootstrap_test.go`
- Modify: `examples/host-api/main.go:32-57`
- Modify: `examples/host-api/main_test.go:1-45`

**Interfaces:**

- Consumes: `skillkit.Discover([]skillkit.Root)`, `skillkit.GateContext`, existing `loadHostConfig`.
- Produces: `const hostAPISkillRootEnv = "HOST_API_SKILL_ROOT"` and `loadHostSkillConfig(func(string) string) (*skillkit.Catalog, skillkit.GateContext, error)`.

- [ ] **Step 1: Write failing tests for disabled, valid, gated, and unsafe roots**

Create `skill_bootstrap_test.go` with these tests and helper:

```go
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eruca/skillkit"
)

func TestLoadHostSkillConfigDisabled(t *testing.T) {
	catalog, gate, err := loadHostSkillConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadHostSkillConfig returned error: %v", err)
	}
	if catalog != nil || gate.OS != "" || len(gate.HostFeatures) != 0 || len(gate.AllowedToolIDs) != 0 {
		t.Fatalf("disabled Skill config = catalog:%v gate:%#v, want zero values", catalog, gate)
	}
}

func TestLoadHostSkillConfigDiscoversTrustedInstructionOnlySkill(t *testing.T) {
	root := t.TempDir()
	writeHostAPISkill(t, root, "workflow-review", "---\nname: workflow-review\ndescription: Review a workflow safely.\n---\n# Instructions\nReview scope and evidence.\n", nil)
	writeHostAPISkill(t, root, "tool-review", "---\nname: tool-review\ndescription: Requires a host tool.\nmetadata:\n  goagents:\n    requires:\n      tools:\n        required: [record_review]\n---\n# Instructions\nUse the required tool.\n", nil)

	catalog, gate, err := loadHostSkillConfig(func(key string) string {
		if key == hostAPISkillRootEnv {
			return root
		}
		return ""
	})
	if err != nil {
		t.Fatalf("loadHostSkillConfig returned error: %v", err)
	}
	if gate.OS != runtime.GOOS || len(gate.HostFeatures) != 0 || len(gate.AllowedToolIDs) != 0 {
		t.Fatalf("gate = %#v, want OS-only context", gate)
	}
	entries := catalog.List()
	if len(entries) != 2 {
		t.Fatalf("entries = %#v, want two Skills", entries)
	}
	for _, entry := range entries {
		if entry.RootID != localUserSkillRootID || entry.Scope != skillkit.ScopeUser || !entry.Trusted {
			t.Fatalf("entry trust = %#v, want trusted local user root", entry)
		}
		report := skillkit.Evaluate(entry, gate)
		if entry.Ref.Name == "workflow-review" && report.State != skillkit.AvailabilityEligible {
			t.Fatalf("workflow-review availability = %#v, want eligible", report)
		}
		if entry.Ref.Name == "tool-review" && report.State != skillkit.AvailabilityUnavailable {
			t.Fatalf("tool-review availability = %#v, want unavailable", report)
		}
	}
}

func TestLoadHostSkillConfigRejectsUnsafeRootWithoutPathLeak(t *testing.T) {
	file := filepath.Join(t.TempDir(), "skills.txt")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write ordinary file: %v", err)
	}
	tests := []struct {
		name string
		root string
	}{
		{name: "relative", root: "relative/skills"},
		{name: "missing", root: filepath.Join(t.TempDir(), "missing")},
		{name: "ordinary file", root: file},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := loadHostSkillConfig(func(key string) string {
				if key == hostAPISkillRootEnv {
					return test.root
				}
				return ""
			})
			if err == nil {
				t.Fatal("loadHostSkillConfig returned nil error")
			}
			if !strings.Contains(err.Error(), hostAPISkillRootEnv) {
				t.Fatalf("error = %v, want environment variable name", err)
			}
			if strings.Contains(err.Error(), test.root) {
				t.Fatalf("error leaks configured root: %v", err)
			}
		})
	}
}
```

- [ ] **Step 2: Run the focused tests and confirm RED**

Run:

```bash
cd examples/host-api
go test -run '^TestLoadHostSkillConfig' -count=1
```

Expected: FAIL to compile because `loadHostSkillConfig`, `hostAPISkillRootEnv`, and `localUserSkillRootID` do not exist.

- [ ] **Step 3: Implement the minimal startup helper**

Create `skill_bootstrap.go`:

```go
package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/eruca/skillkit"
)

const (
	hostAPISkillRootEnv = "HOST_API_SKILL_ROOT"
	localUserSkillRootID = "local-user-skills"
)

func loadHostSkillConfig(getenv func(string) string) (*skillkit.Catalog, skillkit.GateContext, error) {
	root := strings.TrimSpace(getenv(hostAPISkillRootEnv))
	if root == "" {
		return nil, skillkit.GateContext{}, nil
	}
	if !filepath.IsAbs(root) {
		return nil, skillkit.GateContext{}, fmt.Errorf("%s must be an absolute directory", hostAPISkillRootEnv)
	}
	catalog, err := skillkit.Discover([]skillkit.Root{{
		ID:      localUserSkillRootID,
		Dir:     root,
		Scope:   skillkit.ScopeUser,
		Trusted: true,
		Enabled: true,
	}})
	if err != nil {
		return nil, skillkit.GateContext{}, fmt.Errorf("load %s: %w", hostAPISkillRootEnv, err)
	}
	return catalog, skillkit.GateContext{OS: runtime.GOOS}, nil
}
```

- [ ] **Step 4: Run focused helper tests and confirm GREEN**

Run:

```bash
cd examples/host-api
gofmt -w skill_bootstrap.go skill_bootstrap_test.go
go test -run '^TestLoadHostSkillConfig' -count=1
```

Expected: PASS.

- [ ] **Step 5: Write failing `loadHostConfig` composition tests**

Append to `main_test.go` and add `path/filepath` plus `strings` imports:

```go
func TestLoadHostConfigIncludesConfiguredSkillCatalog(t *testing.T) {
	root := t.TempDir()
	writeHostAPISkill(t, root, "workflow-review", "---\nname: workflow-review\ndescription: Review a workflow safely.\n---\n# Instructions\nReview scope and evidence.\n", nil)
	expectedAuthenticator := &OIDCApprovalAuthenticator{}
	env := map[string]string{hostAPISkillRootEnv: root}

	config, err := loadHostConfig(func(key string) string { return env[key] }, func(context.Context, func(string) string) (*OIDCApprovalAuthenticator, error) {
		return expectedAuthenticator, nil
	})
	if err != nil {
		t.Fatalf("loadHostConfig returned error: %v", err)
	}
	if config.ApprovalAuthenticator != expectedAuthenticator || config.SkillCatalog == nil || config.SkillGateContext.OS == "" {
		t.Fatalf("config = %#v, want authenticator and Skill config", config)
	}
	if entries := config.SkillCatalog.List(); len(entries) != 1 || entries[0].Ref.Name != "workflow-review" {
		t.Fatalf("Skill entries = %#v, want workflow-review", entries)
	}
}

func TestLoadHostConfigRejectsInvalidSkillRootBeforeOIDCDiscovery(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	oidcLoads := 0
	_, err := loadHostConfig(func(key string) string {
		if key == hostAPISkillRootEnv {
			return missing
		}
		return ""
	}, func(context.Context, func(string) string) (*OIDCApprovalAuthenticator, error) {
		oidcLoads++
		return nil, errors.New("OIDC loader called")
	})
	if oidcLoads != 0 {
		t.Fatalf("OIDC loader calls = %d, want 0", oidcLoads)
	}
	if err == nil || !strings.Contains(err.Error(), hostAPISkillRootEnv) || strings.Contains(err.Error(), missing) {
		t.Fatalf("loadHostConfig error = %v, want path-free Skill root error", err)
	}
}
```

- [ ] **Step 6: Run composition tests and confirm RED**

Run:

```bash
cd examples/host-api
go test -run '^TestLoadHostConfig.*Skill' -count=1
```

Expected: FAIL because `loadHostConfig` does not populate `SkillCatalog` and calls OIDC discovery before rejecting an invalid Skill root.

- [ ] **Step 7: Wire Skill bootstrap before OIDC discovery**

In `loadHostConfig`, insert this block immediately after Keychain identity validation:

```go
	catalog, skillGate, err := loadHostSkillConfig(getenv)
	if err != nil {
		return Config{}, err
	}
```

Add these fields to the returned `Config`:

```go
		SkillCatalog:                  catalog,
		SkillGateContext:              skillGate,
```

- [ ] **Step 8: Run Task 1 verification**

Run:

```bash
cd examples/host-api
gofmt -w main.go main_test.go skill_bootstrap.go skill_bootstrap_test.go
go test -run '^(TestLoadHostSkillConfig|TestLoadHostConfig)' -count=1
go test ./... -count=1
go vet ./...
```

Expected: every command exits 0.

- [ ] **Step 9: Commit Task 1**

```bash
git add examples/host-api/main.go examples/host-api/main_test.go examples/host-api/skill_bootstrap.go examples/host-api/skill_bootstrap_test.go
git commit -m "feat(host-api): 加载显式Skill根目录"
```

### Task 2: Add one real instruction-only Skill and HTTP proof

**Files:**

- Create: `examples/host-api/skills/workflow-review/SKILL.md`
- Modify: `examples/host-api/skill_bootstrap_test.go`

**Interfaces:**

- Consumes: Task 1 `loadHostSkillConfig` and existing `NewServer`, `GET /skills`, `POST /workflows`.
- Produces: bundled `workflow-review` Skill package and `TestBundledWorkflowReviewSkillRunsThroughHostAPI`.

- [ ] **Step 1: Write the failing bundled-Skill HTTP test**

Append to `skill_bootstrap_test.go` and add `net/http` import:

```go
func TestBundledWorkflowReviewSkillRunsThroughHostAPI(t *testing.T) {
	root, err := filepath.Abs("skills")
	if err != nil {
		t.Fatalf("resolve bundled Skill root: %v", err)
	}
	catalog, gate, err := loadHostSkillConfig(func(key string) string {
		if key == hostAPISkillRootEnv {
			return root
		}
		return ""
	})
	if err != nil {
		t.Fatalf("load bundled Skill config: %v", err)
	}
	server, err := NewServer(Config{
		RuntimeHome:      t.TempDir(),
		SkillCatalog:     catalog,
		SkillGateContext: gate,
	})
	if err != nil {
		t.Fatalf("NewServer returned error: %v", err)
	}
	t.Cleanup(func() {
		closeStoreIfPossible(t, server.workflows)
		closeStoreIfPossible(t, server.runs)
	})

	listed := doJSON[skillListPayload](t, server.Handler(), http.MethodGet, "/skills", nil)
	if len(listed.Skills) != 1 {
		t.Fatalf("GET /skills = %#v, want one bundled Skill", listed.Skills)
	}
	skill := listed.Skills[0]
	if skill.Name != "workflow-review" || skill.Scope != string(skillkit.ScopeUser) || skill.Availability != string(skillkit.AvailabilityEligible) || len(skill.Digest) != 64 {
		t.Fatalf("bundled Skill = %#v, want eligible workflow-review with digest", skill)
	}

	created := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-bundled-skill",
		"input": "Review this local workflow.",
		"skill_refs": []map[string]string{{
			"name": "workflow-review",
		}},
	})
	if len(created.SkillRefs) != 1 || created.SkillRefs[0].Name != skill.Name || created.SkillRefs[0].Digest != skill.Digest {
		t.Fatalf("workflow Skill refs = %#v, want bundled name@digest", created.SkillRefs)
	}
}
```

- [ ] **Step 2: Run the test and confirm RED**

Run:

```bash
cd examples/host-api
go test -run '^TestBundledWorkflowReviewSkillRunsThroughHostAPI$' -count=1
```

Expected: FAIL because `examples/host-api/skills` does not exist and `loadHostSkillConfig` cannot discover the configured root.

- [ ] **Step 3: Write the real Skill package**

Create `skills/workflow-review/SKILL.md`:

```markdown
---
name: workflow-review
description: Review a workflow for scope, evidence, risks, and approval boundaries.
---
# Workflow review

Review only the supplied workflow and artifacts.

- State the workflow scope and intended outcome.
- Distinguish observed evidence from assumptions.
- Identify blocking risks separately from non-blocking improvements.
- Preserve explicit operator approval boundaries.
- Do not claim that an action executed unless the host reports its result.
```

- [ ] **Step 4: Run the test and confirm GREEN**

Run:

```bash
cd examples/host-api
gofmt -w skill_bootstrap_test.go
go test -run '^TestBundledWorkflowReviewSkillRunsThroughHostAPI$' -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit Task 2**

```bash
git add examples/host-api/skills/workflow-review/SKILL.md examples/host-api/skill_bootstrap_test.go
git commit -m "feat(host-api): 添加本机Skill示例"
```

### Task 3: Prove Skill bootstrap across real process restart

**Files:**

- Create: `examples/host-api/host_skill_process_smoke_test.go`
- Modify: `examples/host-api/host_process_smoke_test.go:170-205`

**Interfaces:**

- Consumes: existing `buildHostBinary`, `hostProcess`, `processJSON`, `stopHostProcess`, and Task 1 `hostAPISkillRootEnv`.
- Produces: `startHostProcessWithEnv(t *testing.T, binary, runtimeHome, issuer, keychainService, keyID string, extraEnvironment map[string]string) *hostProcess` while preserving the existing `startHostProcess(t *testing.T, binary, runtimeHome, issuer, keychainService, keyID string) *hostProcess` signature.

- [ ] **Step 1: Write the failing real-process restart smoke**

Create `host_skill_process_smoke_test.go`:

```go
//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"net/http"
	"path/filepath"
	"testing"
)

func TestHostAPIProcessLoadsConfiguredSkillRootAcrossRestart(t *testing.T) {
	provider := newOIDCTestProvider(t)
	binary := buildHostBinary(t)
	runtimeHome := t.TempDir()
	skillRoot, err := filepath.Abs("skills")
	if err != nil {
		t.Fatalf("resolve bundled Skill root: %v", err)
	}
	extraEnvironment := map[string]string{hostAPISkillRootEnv: skillRoot}

	first := startHostProcessWithEnv(t, binary, runtimeHome, provider.issuer, "", "", extraEnvironment)
	listed, status := processJSON[skillListPayload](t, first, http.MethodGet, "/skills", nil, "")
	if status != http.StatusOK || len(listed.Skills) != 1 || listed.Skills[0].Name != "workflow-review" || len(listed.Skills[0].Digest) != 64 {
		t.Fatalf("first GET /skills status=%d skills=%#v", status, listed.Skills)
	}
	digest := listed.Skills[0].Digest
	firstWorkflow, status := processJSON[workflowResponse](t, first, http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-process-skill-first",
		"input": "Review the first process workflow.",
		"skill_refs": []map[string]string{{
			"name": "workflow-review",
		}},
	}, "")
	if status != http.StatusAccepted || len(firstWorkflow.SkillRefs) != 1 || firstWorkflow.SkillRefs[0].Digest != digest {
		t.Fatalf("first workflow status=%d refs=%#v, want persisted digest", status, firstWorkflow.SkillRefs)
	}
	stopHostProcess(t, first)

	second := startHostProcessWithEnv(t, binary, runtimeHome, provider.issuer, "", "", extraEnvironment)
	listedAfterRestart, status := processJSON[skillListPayload](t, second, http.MethodGet, "/skills", nil, "")
	if status != http.StatusOK || len(listedAfterRestart.Skills) != 1 || listedAfterRestart.Skills[0].Digest != digest {
		t.Fatalf("second GET /skills status=%d skills=%#v, want stable digest %q", status, listedAfterRestart.Skills, digest)
	}
	secondWorkflow, status := processJSON[workflowResponse](t, second, http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-process-skill-second",
		"input": "Review the second process workflow.",
		"skill_refs": []map[string]string{{
			"name":   "workflow-review",
			"digest": digest,
		}},
	}, "")
	if status != http.StatusAccepted || len(secondWorkflow.SkillRefs) != 1 || secondWorkflow.SkillRefs[0].Digest != digest {
		t.Fatalf("second workflow status=%d refs=%#v, want exact digest replay", status, secondWorkflow.SkillRefs)
	}
	stopHostProcess(t, second)
}
```

- [ ] **Step 2: Run the focused smoke and confirm RED**

Run:

```bash
cd examples/host-api
go test -tags hostapisystemsmoke -run '^TestHostAPIProcessLoadsConfiguredSkillRootAcrossRestart$' -count=1
```

Expected: FAIL to compile because `startHostProcessWithEnv` does not exist.

- [ ] **Step 3: Add an environment-extensible process helper without changing existing callers**

Replace the body boundary around `startHostProcess` with:

```go
func startHostProcess(t *testing.T, binary, runtimeHome, issuer, keychainService, keyID string) *hostProcess {
	t.Helper()
	return startHostProcessWithEnv(t, binary, runtimeHome, issuer, keychainService, keyID, nil)
}

func startHostProcessWithEnv(t *testing.T, binary, runtimeHome, issuer, keychainService, keyID string, extraEnvironment map[string]string) *hostProcess {
	t.Helper()
	address := freeLoopbackAddress(t)
	process := &hostProcess{
		baseURL: "http://" + address,
		client:  &http.Client{Timeout: time.Second},
		cmd:     exec.Command(binary),
	}
	environment := make(map[string]string, len(extraEnvironment)+9)
	for name, value := range extraEnvironment {
		environment[name] = value
	}
	for name, value := range map[string]string{
		"HOST_API_ADDR":                          address,
		"HOST_RUNTIME_HOME":                      runtimeHome,
		"HOST_API_OIDC_ISSUER":                   issuer,
		"HOST_API_OIDC_AUDIENCE":                 "host-api",
		"HOST_API_AGENT_APPROVAL_SWEEP_INTERVAL": time.Hour.String(),
		"HOST_API_QUEUED_LEASE_DURATION":         time.Minute.String(),
		agentApprovalKeychainServiceEnv:          keychainService,
		agentApprovalKeyIDEnv:                    keyID,
		"LLMKIT_HOME":                            filepath.Join(runtimeHome, ".llmkit"),
	} {
		environment[name] = value
	}
	process.cmd.Env = overrideEnvironment(environment)
	process.cmd.Stdout = &process.output
	process.cmd.Stderr = &process.output
	if err := process.cmd.Start(); err != nil {
		t.Fatalf("start host process: %v", err)
	}
	t.Cleanup(func() { stopHostProcess(t, process) })
	if err := waitForHostReady(process); err != nil {
		stopHostProcess(t, process)
		t.Fatalf("host process did not become ready: %v\n%s", err, process.output.String())
	}
	return process
}
```

Base host variables overwrite any same-named extra value; tests can add the Skill root but cannot redirect the fixed process identity, address, OIDC, runtime, or Keychain configuration.

- [ ] **Step 4: Run focused and existing process smokes**

Run:

```bash
cd examples/host-api
gofmt -w host_process_smoke_test.go host_skill_process_smoke_test.go
go test -tags hostapisystemsmoke -run '^TestHostAPIProcessLoadsConfiguredSkillRootAcrossRestart$' -count=1
go test -tags hostapisystemsmoke -run '^TestHostAPIProcessToolApprovalSurvivesRestart$' -count=1
go vet -tags hostapisystemsmoke ./...
```

Expected: all commands exit 0. The new smoke does not create a Keychain item; the existing Keychain smoke remains independently green.

- [ ] **Step 5: Commit Task 3**

```bash
git add examples/host-api/host_process_smoke_test.go examples/host-api/host_skill_process_smoke_test.go
git commit -m "test(host-api): 覆盖本机Skill进程重启"
```

### Task 4: Synchronize public docs and close the slice

**Files:**

- Modify: `examples/host-api/README.md:111-126`
- Modify: `docs/host-api-contract.md:10-30`
- Modify: `skillkit/README.md:131-137`
- Modify: `docs/superpowers/specs/2026-07-12-skillkit-design.md:1-5, 376-382`
- Modify: `docs/superpowers/specs/2026-07-13-host-skill-bootstrap-design.md:1-6`

**Interfaces:**

- Consumes: Tasks 1-3 verified runtime behavior.
- Produces: operator-facing startup instructions and accurate implementation status.

- [ ] **Step 1: Document the default CLI Skill root**

Replace the stale default-CLI paragraph in `examples/host-api/README.md` with:

````markdown
The default CLI discovers no Skills unless `HOST_API_SKILL_ROOT` names one
absolute local directory. Setting the variable explicitly trusts that directory
as the `user`-scope root for the lifetime of the process:

```bash
export HOST_API_SKILL_ROOT="$PWD/skills"
```

The catalog is rebuilt only at process startup. The default CLI gate supplies
the current OS but no host features or allowed tools, so instruction-only Skills
can activate while capability-requiring Skills remain unavailable. HTTP callers
cannot add roots, change trust, or refresh the catalog.
````

- [ ] **Step 2: Add the environment contract**

Add this bullet near the other startup variables in `docs/host-api-contract.md`:

```markdown
- `HOST_API_SKILL_ROOT`: optional absolute path to one explicitly trusted local
  Skill root. Empty disables CLI Skill discovery. Invalid paths fail startup;
  the value is never returned by the API. The default CLI supplies only the
  current OS to Skill gating and does not grant tools or host features.
```

- [ ] **Step 3: Correct SkillKit scope and design status drift**

Replace `skillkit/README.md` Current scope with:

```markdown
SkillKit includes catalog discovery, availability evaluation, run-start
activation, bounded resource reads, and `agentcore.SkillProvider` wiring. The
host-api example adds safe catalog exposure, durable workflow `skill_refs`, a
single explicit local-root bootstrap, and a host-side evaluation gate. Remote
registries, dependency installation, request-scoped tool projection, dynamic
activation, and script sandboxing remain separate future slices.
```

Set the 2026-07-12 SkillKit design status to:

```markdown
**状态：** 首版已实现
```

Replace its implementation slice list with:

```markdown
1. **Catalog slice（已完成）：** 新建 `skillkit` module，实现 manifest、roots、scan、digest、冲突和 availability；只提供 Go API 与单元测试，不接 Agent。
2. **Activation slice（已完成）：** 实现 `SkillRef`、run-start gating、`agentcore.SkillProvider` adapter 和安全资源 resolver；不增加脚本执行器。
3. **Host slice（已完成）：** 在 `examples/host-api` 暴露 Skill 列表与 workflow `skill_refs`，将解析后的 `name@digest` 写入 SQLite metadata，补 restart/requeue smoke。
4. **Evaluation slice（已完成）：** 将 Skill 选择、授权和内容漂移案例加入 host-side `evalkit` 发布门禁。
```

Set the 2026-07-13 bootstrap design status to:

```markdown
**状态：** 已实现
```

Do not change either design's non-goals.

- [ ] **Step 4: Run full verification**

Run:

```bash
cd examples/host-api
go test ./... -count=1
go vet ./...
go test -tags hostapisystemsmoke -run '^TestHostAPIProcessLoadsConfiguredSkillRootAcrossRestart$' -count=1
go vet -tags hostapisystemsmoke ./...
cd ../..
bash ./scripts/verify-all.sh
git diff --check
git status --short
```

Expected: tests, vet, process smoke, and workspace verification exit 0; the workspace script ends with `goagents workspace verification passed`; status contains only the five intended documentation files.

- [ ] **Step 5: Commit Task 4**

```bash
git add examples/host-api/README.md docs/host-api-contract.md skillkit/README.md docs/superpowers/specs/2026-07-12-skillkit-design.md docs/superpowers/specs/2026-07-13-host-skill-bootstrap-design.md
git commit -m "docs(skillkit): 记录本机Skill启动方式"
```

- [ ] **Step 6: Verify the final branch**

Run:

```bash
git status --short --branch
git log -4 --oneline
```

Expected: clean feature worktree with four semantic implementation commits after the plan commit.
