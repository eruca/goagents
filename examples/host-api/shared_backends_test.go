package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eruca/goagents/evalkit"
	"github.com/eruca/goagents/llmkit"
	"github.com/eruca/goagents/llmkit/pghealth"
	"github.com/eruca/goagents/workflowkit"
	workflowpg "github.com/eruca/goagents/workflowkit/pgstore"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func testKeyring(t *testing.T, ids ...string) string {
	t.Helper()
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, id+":"+base64.StdEncoding.EncodeToString(key))
	}
	return strings.Join(entries, ",")
}

func TestResolveAgentApprovalKeysFromKeyring(t *testing.T) {
	keyring := testKeyring(t, "k1", "k2")
	cipher, keychain, err := resolveAgentApprovalKeys(Config{AgentApprovalKeyring: keyring, AgentApprovalKeyID: "k2"})
	if err != nil {
		t.Fatalf("resolveAgentApprovalKeys: %v", err)
	}
	if cipher == nil || keychain != (agentApprovalKeychainConfig{}) {
		t.Fatalf("cipher=%v keychain=%+v, want a static cipher and no Keychain item", cipher, keychain)
	}
	sealed, err := cipher.Encrypt(context.Background(), []byte("state"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	// Another process given the same keyring decrypts it.
	other, _, err := resolveAgentApprovalKeys(Config{AgentApprovalKeyring: keyring, AgentApprovalKeyID: "k2"})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := other.Decrypt(context.Background(), sealed, []byte("aad"))
	if err != nil || string(opened) != "state" {
		t.Fatalf("Decrypt = %q, %v", opened, err)
	}
}

func TestResolveAgentApprovalKeysRejectsBadKeyringConfig(t *testing.T) {
	keyring := testKeyring(t, "k1")
	for name, config := range map[string]Config{
		"missing active id":  {AgentApprovalKeyring: keyring},
		"unknown active id":  {AgentApprovalKeyring: keyring, AgentApprovalKeyID: "k9"},
		"with keychain":      {AgentApprovalKeyring: keyring, AgentApprovalKeyID: "k1", AgentApprovalKeychainService: "svc"},
		"malformed keyring":  {AgentApprovalKeyring: "k1:not-base64", AgentApprovalKeyID: "k1"},
		"with injected test": {AgentApprovalKeyring: keyring, AgentApprovalKeyID: "k1", AgentApprovalCipher: testAgentApprovalCipherForBackends(t)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := resolveAgentApprovalKeys(config); err == nil {
				t.Fatal("resolveAgentApprovalKeys accepted an invalid configuration")
			} else if strings.Contains(err.Error(), keyring) {
				t.Fatalf("error leaked the keyring: %v", err)
			}
		})
	}
}

func TestLoadHostSettingsReadsSharedBackendSwitches(t *testing.T) {
	keyring := testKeyring(t, "k1")
	env := map[string]string{
		workflowPostgresDSNEnv:     "postgres://workflows",
		llmkitHealthPostgresDSNEnv: "postgres://health",
		agentApprovalKeyringEnv:    keyring,
		agentApprovalKeyIDEnv:      "k1",
	}
	settings, err := loadHostSettings(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("loadHostSettings: %v", err)
	}
	if settings.workflowPostgresDSN != "postgres://workflows" || settings.llmkitHealthPostgresDSN != "postgres://health" || settings.agentApprovalKeyring != keyring {
		t.Fatalf("settings = %+v", settings)
	}
	delete(env, agentApprovalKeyIDEnv)
	if _, err := loadHostSettings(func(name string) string { return env[name] }); err == nil {
		t.Fatal("loadHostSettings accepted a keyring without an active key id")
	}
}

func TestNewServerWithoutSwitchesKeepsLocalBackends(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(context.Background()) })
	if _, ok := server.health.(*llmkit.MemoryHealthStore); !ok || server.healthCloser != nil {
		t.Fatalf("health = %T closer=%v, want in-memory store", server.health, server.healthCloser)
	}
	if _, ok := server.workflows.(*workflowpg.Store); ok {
		t.Fatal("workflow store is PostgreSQL without a DSN")
	}
}

func testAgentApprovalCipherForBackends(t *testing.T) interface {
	Encrypt(context.Context, []byte, []byte) ([]byte, error)
	Decrypt(context.Context, []byte, []byte) ([]byte, error)
} {
	t.Helper()
	cipher, _, err := resolveAgentApprovalKeys(Config{AgentApprovalKeyring: testKeyring(t, "inj"), AgentApprovalKeyID: "inj"})
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

var hostPostgresSchemaSeq atomic.Int64

// hostPostgresDSN returns a DSN whose search_path is a fresh schema dropped
// after the test, so fixed workflow ids never meet another test's rows.
func hostPostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("HOST_API_POSTGRES_TEST_DSN"))
	if dsn == "" {
		if os.Getenv("HOST_API_REQUIRE_POSTGRES") == "1" {
			t.Fatal("HOST_API_POSTGRES_TEST_DSN is required")
		}
		t.Skip("set HOST_API_POSTGRES_TEST_DSN to run PostgreSQL host tests")
	}
	schema := fmt.Sprintf("host_api_test_%d_%d", os.Getpid(), hostPostgresSchemaSeq.Add(1))
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		_ = admin.Close()
	})
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("HOST_API_POSTGRES_TEST_DSN must be a PostgreSQL URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// TestHostAPISharedBackendsAcrossProcesses runs two Hosts with separate
// runtime homes on one PostgreSQL database for every backend and one approval
// keyring. A workflow queued through the first Host runs on the second Host's
// worker, and both Hosts then read the same workflow, output artifact, and
// LLM route audit.
func TestHostAPISharedBackendsAcrossProcesses(t *testing.T) {
	dsn := hostPostgresDSN(t)
	keyring := testKeyring(t, "shared")
	config := func() Config {
		return Config{
			RuntimeHome:             t.TempDir(),
			WorkflowPostgresDSN:     dsn,
			LLMKitHealthPostgresDSN: dsn,
			ArtifactPostgresDSN:     dsn,
			AgentRunPostgresDSN:     dsn,
			LLMKitAuditPostgresDSN:  dsn,
			AgentApprovalKeyring:    keyring,
			AgentApprovalKeyID:      "shared",
		}
	}
	first, err := NewServer(config())
	if err != nil {
		t.Fatalf("first NewServer: %v", err)
	}
	t.Cleanup(func() { _ = first.Close(context.Background()) })
	second, err := NewServer(config())
	if err != nil {
		t.Fatalf("second NewServer: %v", err)
	}
	if _, ok := first.workflows.(*workflowpg.Store); !ok {
		t.Fatalf("workflow store = %T, want PostgreSQL", first.workflows)
	}
	if _, ok := first.health.(*pghealth.Store); !ok {
		t.Fatalf("health store = %T, want PostgreSQL", first.health)
	}
	if _, ok := first.audit.(pgAudit); !ok {
		t.Fatalf("audit = %T, want PostgreSQL", first.audit)
	}

	created := doJSON[workflowResponse](t, first.Handler(), http.MethodPost, "/workflows", map[string]any{
		"id": "wf-shared-pg", "input": "Review shared backends.", "run_mode": "queued",
	})
	if created.Status != string(workflowkit.StatusPending) {
		t.Fatalf("queued create = %+v", created)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopWorkerOnCleanup(t, second, cancel)
	second.StartQueuedWorker(ctx)

	loaded := waitForWorkflowStatus(t, second.Handler(), "wf-shared-pg", workflowkit.StatusWaitingApproval)
	if loaded.AgentRunID == "" || loaded.OutputRef == "" {
		t.Fatalf("workflow = %+v, want agent output from the second Host", loaded)
	}
	stored := waitForWorkflowLeaseCleared(t, second.workflows, "wf-shared-pg")
	if stored.Status != workflowkit.StatusWaitingApproval {
		t.Fatalf("stored workflow = %+v", stored)
	}
	// The first Host never ran the workflow, yet reads everything it produced.
	seen := doJSON[workflowResponse](t, first.Handler(), http.MethodGet, "/workflows/wf-shared-pg", nil)
	if seen.Status != loaded.Status || seen.OutputRef != loaded.OutputRef || seen.AgentRunID != loaded.AgentRunID {
		t.Fatalf("first Host sees %+v, second Host %+v", seen, loaded)
	}
	if _, err := first.artifacts.Get(context.Background(), loaded.OutputRef); err != nil {
		t.Fatalf("first Host reads output artifact: %v", err)
	}
	if _, err := first.runs.Get(context.Background(), loaded.AgentRunID); err != nil {
		t.Fatalf("first Host reads agent run: %v", err)
	}
	routes := doJSON[llmRoutesResponse](t, first.Handler(), http.MethodGet, "/workflows/wf-shared-pg/llm-routes", nil)
	if len(routes.Routes) == 0 {
		t.Fatal("first Host reads no LLM routes for a workflow the second Host ran")
	}
	// Both Hosts build the same trajectory from the shared stores, including
	// the model call recorded by the Host that ran the workflow.
	firstTrace := doJSON[workflowTraceResponse](t, first.Handler(), http.MethodGet, "/workflows/wf-shared-pg/trace", nil)
	secondTrace := doJSON[workflowTraceResponse](t, second.Handler(), http.MethodGet, "/workflows/wf-shared-pg/trace", nil)
	firstJSON, _ := json.Marshal(firstTrace)
	secondJSON, _ := json.Marshal(secondTrace)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("Hosts disagree on the trace:\nfirst  %s\nsecond %s", firstJSON, secondJSON)
	}
	if len(evalkit.ModelCalls(evalkit.Trace{Steps: traceSteps(firstTrace)})) == 0 {
		t.Fatalf("shared trace has no model call: %s", firstJSON)
	}
}

func traceSteps(response workflowTraceResponse) []evalkit.TraceStep {
	steps := make([]evalkit.TraceStep, 0, len(response.Steps))
	for _, step := range response.Steps {
		steps = append(steps, evalkit.TraceStep{Type: step.Type, Name: step.Name, Status: step.Status})
	}
	return steps
}
