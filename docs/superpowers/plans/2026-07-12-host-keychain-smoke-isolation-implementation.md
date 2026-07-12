# Host API Keychain 进程烟测隔离实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让真实 Host API 进程烟测使用测试专属 macOS Keychain 命名空间并可靠完成重启恢复，同时保持生产默认 Keychain 标识和失败关闭策略不变。

**Architecture:** `Config` 新增成对的 Keychain service/key ID 宿主配置，CLI 仅从环境变量读取，approval service 延迟使用解析后的标识。Darwin 系统烟测为每次运行生成唯一 service，两个真实子进程复用该 service 和同一二进制，完成后只删除精确的测试项。

**Tech Stack:** Go 1.26.1、macOS Security.framework、`github.com/99designs/go-keychain`、`examples/host-api`、SQLite、OIDC。

## Global Constraints

- 生产默认 service 必须保持 `goagents.host-api.approvals`，默认 key ID 必须保持 `local-v1`。
- Keychain 仍是本机生产密钥的唯一后端；禁止文件、环境变量密钥材料或 SQLite 明文回退。
- 新环境变量只传 Keychain 查找标识，不包含、打印或持久化密钥材料。
- service 和 key ID 必须同时为空或同时为非空；部分配置与纯空白配置必须在启动阶段失败。
- HTTP、workflow metadata、模型和 Skill 均不能改变 Keychain 标识。
- 烟测只能删除带 `goagents.host-api.approvals.smoke.` 前缀的精确测试 service/account，禁止读取、修改或删除生产项。
- 不改变 checkpoint schema、AES-GCM 信封、AAD、Agent、SkillKit 或工具审批业务协议。

---

### Task 1: 增加宿主拥有的 Keychain 标识配置

**Files:**

- Modify: `examples/host-api/server.go:31-49,388-475`
- Modify: `examples/host-api/agent_approval.go:22-191`
- Modify: `examples/host-api/agent_approval_test.go`
- Modify: `examples/host-api/main.go:10-28`

**Interfaces:**

- Consumes: 现有 `approvalcrypto.OpenMacOSKeychainKeyProvider(serviceName, activeKeyID)`。
- Produces: `Config.AgentApprovalKeychainService`、`Config.AgentApprovalKeyID`、`agentApprovalKeychainConfig`、`resolveAgentApprovalKeychainConfig`，供 Task 2 的真实进程环境变量使用。

- [ ] **Step 1: 写配置解析失败测试**

在 `agent_approval_test.go` 添加：

```go
func TestResolveAgentApprovalKeychainConfig(t *testing.T) {
	tests := []struct {
		name        string
		service     string
		keyID       string
		wantService string
		wantKeyID   string
		wantErr     bool
	}{
		{name: "defaults", wantService: localApprovalKeychainService, wantKeyID: localApprovalKeyID},
		{name: "custom", service: "goagents.host-api.approvals.smoke.test", keyID: "smoke-v1", wantService: "goagents.host-api.approvals.smoke.test", wantKeyID: "smoke-v1"},
		{name: "missing service", keyID: "smoke-v1", wantErr: true},
		{name: "missing key id", service: "goagents.host-api.approvals.smoke.test", wantErr: true},
		{name: "whitespace service", service: " ", keyID: "smoke-v1", wantErr: true},
		{name: "both whitespace", service: " ", keyID: "\t", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := resolveAgentApprovalKeychainConfig(test.service, test.keyID)
			if test.wantErr {
				if err == nil {
					t.Fatalf("resolve config = %#v, want error", config)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve config: %v", err)
			}
			if config.service != test.wantService || config.keyID != test.wantKeyID {
				t.Fatalf("config = %#v, want service=%q key_id=%q", config, test.wantService, test.wantKeyID)
			}
		})
	}
}

func TestNewServerRejectsPartialAgentApprovalKeychainConfig(t *testing.T) {
	_, err := NewServer(Config{
		RuntimeHome:                     t.TempDir(),
		AgentApprovalKeychainService: "goagents.host-api.approvals.smoke.test",
	})
	if err == nil {
		t.Fatal("NewServer returned nil error for partial Keychain config")
	}
}
```

- [ ] **Step 2: 运行测试并确认 RED**

Run:

```bash
cd examples/host-api && go test ./... -run 'TestResolveAgentApprovalKeychainConfig|TestNewServerRejectsPartialAgentApprovalKeychainConfig' -count=1
```

Expected: 编译失败，提示 `resolveAgentApprovalKeychainConfig` 或 `Config.AgentApprovalKeychainService` 尚不存在。

- [ ] **Step 3: 实现严格的配置解析**

在 `agent_approval.go` 的常量和 service 定义附近加入：

```go
const (
	agentApprovalKeychainServiceEnv = "HOST_API_AGENT_APPROVAL_KEYCHAIN_SERVICE"
	agentApprovalKeyIDEnv           = "HOST_API_AGENT_APPROVAL_KEY_ID"
)

type agentApprovalKeychainConfig struct {
	service string
	keyID   string
}

func resolveAgentApprovalKeychainConfig(service, keyID string) (agentApprovalKeychainConfig, error) {
	if service == "" && keyID == "" {
		return agentApprovalKeychainConfig{
			service: localApprovalKeychainService,
			keyID:   localApprovalKeyID,
		}, nil
	}
	service = strings.TrimSpace(service)
	keyID = strings.TrimSpace(keyID)
	if service == "" || keyID == "" {
		return agentApprovalKeychainConfig{}, fmt.Errorf("agent approval Keychain service and key ID must be configured together")
	}
	return agentApprovalKeychainConfig{service: service, keyID: keyID}, nil
}
```

给 `hostAgentApprovalService` 添加配置，并收窄构造器：

```go
type hostAgentApprovalService struct {
	checkpoints runkit.CheckpointStore
	runner      routingAgentRunner
	keychain    agentApprovalKeychainConfig

	mu     sync.Mutex
	cipher goagentapproval.Cipher
}

func newHostAgentApprovalService(
	runs runkit.Store,
	cipher goagentapproval.Cipher,
	runner routingAgentRunner,
	keychain agentApprovalKeychainConfig,
) (*hostAgentApprovalService, error) {
	checkpoints, ok := runs.(runkit.CheckpointStore)
	if !ok {
		return nil, fmt.Errorf("host run store does not implement approval checkpoint persistence")
	}
	return &hostAgentApprovalService{
		checkpoints: checkpoints,
		cipher:      cipher,
		runner:      runner,
		keychain:    keychain,
	}, nil
}
```

把 `activeCipher` 的固定常量替换为解析后的字段：

```go
keys, err := approvalcrypto.OpenMacOSKeychainKeyProvider(s.keychain.service, s.keychain.keyID)
```

- [ ] **Step 4: 在 NewServer 启动阶段解析配置**

在 `NewServer` 第一行解析，确保部分配置在创建目录或打开 SQLite 前失败：

```go
func NewServer(config Config) (*Server, error) {
	approvalKeychain, err := resolveAgentApprovalKeychainConfig(
		config.AgentApprovalKeychainService,
		config.AgentApprovalKeyID,
	)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveRuntimeConfig(config)
```

在 `Config` 增加：

```go
// AgentApprovalKeychainService and AgentApprovalKeyID select the host-owned
// Keychain item. They identify key material but never contain it.
AgentApprovalKeychainService string
AgentApprovalKeyID           string
```

调用构造器时传入 `approvalKeychain`：

```go
agentApprovals, err := newHostAgentApprovalService(runs, config.AgentApprovalCipher, runner, approvalKeychain)
```

- [ ] **Step 5: 让真实 CLI 读取宿主环境变量**

在 `main.go` 的 `Config` 字面量加入：

```go
AgentApprovalKeychainService: os.Getenv(agentApprovalKeychainServiceEnv),
AgentApprovalKeyID:           os.Getenv(agentApprovalKeyIDEnv),
```

- [ ] **Step 6: 运行聚焦和模块测试，确认 GREEN**

Run:

```bash
cd examples/host-api && go test ./... -run 'TestResolveAgentApprovalKeychainConfig|TestNewServerRejectsPartialAgentApprovalKeychainConfig' -count=1
cd examples/host-api && go test ./... -count=1
```

Expected: 两个命令退出码均为 0。

- [ ] **Step 7: 语义提交宿主配置**

```bash
git add examples/host-api/server.go examples/host-api/agent_approval.go examples/host-api/agent_approval_test.go examples/host-api/main.go
git commit -m "feat(host-api): 支持配置Keychain标识"
```

---

### Task 2: 使用测试专属 Keychain 项跑通真实进程重启

**Files:**

- Modify: `examples/host-api/host_process_smoke_test.go:28-205`

**Interfaces:**

- Consumes: Task 1 的 `HOST_API_AGENT_APPROVAL_KEYCHAIN_SERVICE`、`HOST_API_AGENT_APPROVAL_KEY_ID` 环境变量。
- Produces: 每次测试唯一的 smoke service、同一测试内稳定的 key ID、精确且有前缀保护的测试项清理。

- [ ] **Step 1: 保存当前真实进程失败证据**

Run:

```bash
cd examples/host-api && go test -tags hostapisystemsmoke -run TestHostAPIProcessToolApprovalSurvivesRestart -count=1 -v
```

Expected RED: 首个 `POST /workflows` 因旧生产 Keychain 项触发交互而 `context deadline exceeded`。该失败已在 `6554e81` 基线复现。

- [ ] **Step 2: 为烟测生成唯一 service 并注册安全清理**

在测试开头、创建第一个进程之前加入：

```go
smokeKeychainService := fmt.Sprintf(
	"%s.smoke.%d",
	localApprovalKeychainService,
	time.Now().UnixNano(),
)
cleanupKeychain := smokeKeychainCleanup(t, smokeKeychainService, localApprovalKeyID)
t.Cleanup(cleanupKeychain)
```

两次启动都传同一组标识：

```go
first := startHostProcess(t, binary, runtimeHome, provider.issuer, smokeKeychainService, localApprovalKeyID)
second := startHostProcess(t, binary, runtimeHome, provider.issuer, smokeKeychainService, localApprovalKeyID)
```

在测试末尾、第二个进程停止后显式执行一次清理；`sync.Once` 保证 `t.Cleanup` 不会重复删除：

```go
stopHostProcess(t, second)
assertCompletedProcessWorkflow(t, runtimeHome, created)
cleanupKeychain()
```

- [ ] **Step 3: 把 Keychain 标识传给真实子进程**

扩展 helper：

```go
func startHostProcess(t *testing.T, binary, runtimeHome, issuer, keychainService, keyID string) *hostProcess {
```

在 `overrideEnvironment` map 加入：

```go
agentApprovalKeychainServiceEnv: keychainService,
agentApprovalKeyIDEnv:           keyID,
```

- [ ] **Step 4: 实现只允许 smoke 前缀的精确清理**

在 `host_process_smoke_test.go` 添加：

```go
func smokeKeychainCleanup(t *testing.T, service, keyID string) func() {
	t.Helper()
	var once sync.Once
	return func() {
		once.Do(func() {
			if !strings.HasPrefix(service, localApprovalKeychainService+".smoke.") {
				t.Errorf("refusing to delete non-smoke Keychain service %q", service)
				return
			}
			command := exec.Command(
				"security", "delete-generic-password",
				"-s", service,
				"-a", "approval-data-key:"+keyID,
			)
			output, err := command.CombinedOutput()
			if err != nil && !bytes.Contains(output, []byte("could not be found")) {
				t.Errorf("delete smoke Keychain item: %v: %s", err, strings.TrimSpace(string(output)))
			}
		})
	}
}
```

该 helper 不读取数据值，不接受生产 service，并且只删除 service/account 完全匹配的测试项。

- [ ] **Step 5: 运行真实进程烟测，确认 GREEN**

Run:

```bash
cd examples/host-api && go test -tags hostapisystemsmoke -run TestHostAPIProcessToolApprovalSurvivesRestart -count=1 -v
```

Expected: `--- PASS: TestHostAPIProcessToolApprovalSurvivesRestart`；不能是 Keychain skip。

- [ ] **Step 6: 重复运行，证明没有残留 ACL 污染**

Run:

```bash
cd examples/host-api && go test -tags hostapisystemsmoke -run TestHostAPIProcessToolApprovalSurvivesRestart -count=2 -v
```

Expected: 两次均 PASS；第二次不等待授权 UI。

- [ ] **Step 7: 语义提交系统烟测修复**

```bash
git add examples/host-api/host_process_smoke_test.go
git commit -m "test(host-api): 隔离Keychain进程烟测"
```

---

### Task 3: 文档与全量验证

**Files:**

- Modify: `examples/host-api/README.md:238-260`
- Modify: `docs/host-api-contract.md:270-278`

**Interfaces:**

- Consumes: Task 1 的宿主环境变量和 Task 2 的测试专属 Keychain 生命周期。
- Produces: 对生产默认值、成对配置、烟测隔离与清理边界的可操作说明。

- [ ] **Step 1: 更新 Host API README**

把 Keychain 段落改为明确说明：

```text
Production defaults to Keychain service goagents.host-api.approvals and key ID local-v1.
Hosts may set HOST_API_AGENT_APPROVAL_KEYCHAIN_SERVICE and
HOST_API_AGENT_APPROVAL_KEY_ID together to select another host-owned item;
these variables are identifiers, never key material. Partial configuration
fails startup and there is still no file or environment key fallback.
```

把烟测段落改为：每次运行使用唯一的 `.smoke.` service，两个子进程复用同一项，测试结束只删除该测试项；它不访问或删除生产默认项。

- [ ] **Step 2: 更新 prose contract**

在 `POST /workflows/{id}/agent-approve` 的 Keychain 说明后加入同样的生产默认值、两个成对环境变量、无密钥材料和无回退约束。

- [ ] **Step 3: 运行模块、竞态和静态检查**

Run:

```bash
cd examples/host-api && go test ./... -count=1
cd examples/host-api && go test -race ./... -count=1
cd examples/host-api && go vet ./...
```

Expected: 所有命令退出码为 0，输出无警告。

- [ ] **Step 4: 运行真实进程与全仓验证**

Run:

```bash
cd examples/host-api && go test -tags hostapisystemsmoke -run TestHostAPIProcessToolApprovalSurvivesRestart -count=2 -v
cd ../.. && bash ./scripts/verify-all.sh
git diff --check
```

Expected: 真实进程测试两次 PASS、全仓脚本输出 `goagents workspace verification passed`、diff check 无输出。

- [ ] **Step 5: 语义提交文档**

```bash
git add examples/host-api/README.md docs/host-api-contract.md
git commit -m "docs(host-api): 说明Keychain烟测隔离"
```

## Plan Self-Review

- Task 1 覆盖生产默认值、自定义标识、部分/空白配置失败与真实 CLI 环境变量传递。
- Task 2 从已存在的真实失败出发，只隔离测试 service，并用同一真实二进制跨进程恢复。
- Task 2 的清理有固定前缀和精确 account 双重边界，不会命中生产 service。
- Task 3 覆盖生产与测试语义文档、模块/race/vet、重复系统烟测和全仓验证。
- 全计划未引入密钥回退、通用删除 API、schema 迁移或无关重构。
