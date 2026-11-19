package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/workflowkit"
)

const (
	mrProviderAPIKeyEnv = "HOST_API_MULTI_REPLICA_PROVIDER_KEY"
	mrProviderAPIKey    = "multi-replica-provider-key"
	// mrBlockMarker in a request (the workflow id appears in the prompt)
	// holds the provider call until the test ends.
	mrBlockMarker = "wf-mr-blocked"
)

// TestHostAPIMultiReplicaProcesses runs two real Host processes with
// separate runtime homes, every backend in one PostgreSQL database, and one
// approval keyring, and checks that they behave as one deployment.
func TestHostAPIMultiReplicaProcesses(t *testing.T) {
	dsn := hostPostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	binary := mrBuildHostBinary(t)
	oidc := newOIDCTestProvider(t)
	provider := newMRProviderStub(t)
	keyring := testKeyring(t, "mr")
	start := func(name string) *mrHost {
		return mrStartHost(t, name, binary, map[string]string{
			"HOST_API_OIDC_ISSUER":                   oidc.issuer,
			"HOST_API_OIDC_AUDIENCE":                 "host-api",
			"HOST_API_AGENT_APPROVAL_SWEEP_INTERVAL": time.Hour.String(),
			"HOST_API_QUEUED_LEASE_DURATION":         "2s",
			hostAPISkillRootEnv:                      "",
			workflowPostgresDSNEnv:                   dsn,
			llmkitHealthPostgresDSNEnv:               dsn,
			artifactPostgresDSNEnv:                   dsn,
			agentRunPostgresDSNEnv:                   dsn,
			llmkitAuditPostgresDSNEnv:                dsn,
			agentApprovalKeyringEnv:                  keyring,
			agentApprovalKeyIDEnv:                    "mr",
			mrProviderAPIKeyEnv:                      mrProviderAPIKey,
		}, provider.server.URL)
	}
	a, b := start("A"), start("B")
	hosts := []*mrHost{a, b}

	t.Run("queued workflows run once and read the same from both hosts", func(t *testing.T) {
		const count = 30
		var wg sync.WaitGroup
		for i := range count {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("wf-mr-%02d", i)
				status, body := hosts[i%2].post(t, "/workflows", map[string]any{"id": id, "input": "input " + id, "run_mode": "queued"})
				if status != http.StatusAccepted {
					t.Errorf("create %s on %s: %d %s", id, hosts[i%2].name, status, body)
				}
			}()
		}
		wg.Wait()
		for i := range count {
			id := fmt.Sprintf("wf-mr-%02d", i)
			fromA := a.waitForStatus(t, id, workflowkit.StatusWaitingApproval)
			fromB := b.getWorkflow(t, id)
			if fromA.Status != fromB.Status || fromA.OutputRef != fromB.OutputRef || fromA.AgentRunID != fromB.AgentRunID || fromA.OutputRef == "" {
				t.Fatalf("%s: host A sees %+v, host B %+v", id, fromA, fromB)
			}
			var runs int
			if err := db.QueryRow(`SELECT count(*) FROM agent_runs WHERE workflow_id = $1`, id).Scan(&runs); err != nil {
				t.Fatal(err)
			}
			if runs != 1 {
				t.Fatalf("%s has %d agent runs, want exactly 1", id, runs)
			}
			for _, host := range hosts {
				var routes llmRoutesResponse
				host.getJSON(t, "/workflows/"+id+"/llm-routes", &routes)
				if len(routes.Routes) == 0 {
					t.Fatalf("host %s reads no LLM routes for %s", host.name, id)
				}
			}
		}
		claimed := 0
		for _, host := range hosts {
			var worker queuedWorkerResponse
			host.getJSON(t, "/workers/queued", &worker)
			if worker.Claimed > 0 {
				claimed++
			}
		}
		t.Logf("hosts that claimed work: %d of 2", claimed)
	})

	t.Run("concurrent creates of one id keep the winner's input", func(t *testing.T) {
		type result struct {
			status int
			body   []byte
			input  string
		}
		results := make([]result, 2)
		var ready, wg sync.WaitGroup
		ready.Add(1)
		for i, host := range hosts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready.Wait()
				input := "input from " + host.name
				status, body := host.post(t, "/workflows", map[string]any{"id": "wf-mr-race", "input": input, "run_mode": "queued"})
				results[i] = result{status: status, body: body, input: input}
			}()
		}
		ready.Done()
		wg.Wait()
		var winner *result
		for i := range results {
			switch results[i].status {
			case http.StatusAccepted:
				if winner != nil {
					t.Fatalf("both creates succeeded: %+v", results)
				}
				winner = &results[i]
			case http.StatusConflict:
			default:
				t.Fatalf("create returned %d: %s", results[i].status, results[i].body)
			}
		}
		if winner == nil {
			t.Fatalf("no create succeeded: %+v", results)
		}
		var created workflowResponse
		if err := json.Unmarshal(winner.body, &created); err != nil {
			t.Fatal(err)
		}
		stored := a.waitForStatus(t, "wf-mr-race", workflowkit.StatusWaitingApproval)
		if stored.InputRef != created.InputRef {
			t.Fatalf("stored input ref %q, winner's %q", stored.InputRef, created.InputRef)
		}
		var content []byte
		if err := db.QueryRow(`SELECT content FROM artifacts WHERE ref = $1`, stored.InputRef).Scan(&content); err != nil {
			t.Fatal(err)
		}
		if string(content) != winner.input {
			t.Fatalf("workflow input = %q, want the winner's %q", content, winner.input)
		}
	})

	t.Run("a killed host's running workflow fails on the other host", func(t *testing.T) {
		status, body := a.post(t, "/workflows", map[string]any{"id": mrBlockMarker, "input": "block", "run_mode": "queued"})
		if status != http.StatusAccepted {
			t.Fatalf("create blocked workflow: %d %s", status, body)
		}
		provider.waitForBlocked(t)
		var owner string
		if err := db.QueryRow(`SELECT lease_owner FROM workflow_runs WHERE id = $1`, mrBlockMarker).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		var running, survivor *mrHost
		for _, host := range hosts {
			var worker queuedWorkerResponse
			host.getJSON(t, "/workers/queued", &worker)
			if worker.WorkerID == owner {
				running = host
			} else {
				survivor = host
			}
		}
		if running == nil || survivor == nil {
			t.Fatalf("lease owner %q matches neither host", owner)
		}
		running.kill(t)

		survivor.waitForStatus(t, mrBlockMarker, workflowkit.StatusFailed)
		var reason string
		if err := db.QueryRow(`SELECT error FROM workflow_runs WHERE id = $1`, mrBlockMarker).Scan(&reason); err != nil {
			t.Fatal(err)
		}
		if reason != queuedLeaseExpiredCode {
			t.Fatalf("workflow failed with %q, want %s", reason, queuedLeaseExpiredCode)
		}
		if calls := provider.blockedCalls(); calls != 1 {
			t.Fatalf("blocked workflow reached the provider %d times, want 1 (never re-executed)", calls)
		}
	})
}

type mrHost struct {
	name    string
	baseURL string
	cmd     *exec.Cmd
	output  *bytes.Buffer
	mu      sync.Mutex
	client  *http.Client
}

func mrBuildHostBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "host-api")
	command := exec.Command("go", "build", "-o", binary, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build host binary: %v\n%s", err, output)
	}
	return binary
}

func mrStartHost(t *testing.T, name, binary string, environment map[string]string, providerURL string) *mrHost {
	t.Helper()
	runtimeHome := t.TempDir()
	llmkitHome := filepath.Join(runtimeHome, ".llmkit")
	mrWriteLLMKitConfig(t, llmkitHome, providerURL)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	host := &mrHost{
		name:    name,
		baseURL: "http://" + address,
		cmd:     exec.Command(binary),
		output:  &bytes.Buffer{},
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	env := os.Environ()
	for key, value := range environment {
		env = append(env, key+"="+value)
	}
	env = append(env, "HOST_API_ADDR="+address, "HOST_RUNTIME_HOME="+runtimeHome, "LLMKIT_HOME="+llmkitHome)
	host.cmd.Env = env
	writer := mrLockedWriter{host: host}
	host.cmd.Stdout, host.cmd.Stderr = writer, writer
	if err := host.cmd.Start(); err != nil {
		t.Fatalf("start host %s: %v", name, err)
	}
	t.Cleanup(func() {
		host.kill(t)
		if t.Failed() {
			host.mu.Lock()
			t.Logf("host %s output:\n%s", name, host.output.String())
			host.mu.Unlock()
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, err := host.client.Get(host.baseURL + "/workers/queued")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return host
			}
		}
		if time.Now().After(deadline) {
			host.mu.Lock()
			defer host.mu.Unlock()
			t.Fatalf("host %s did not become ready:\n%s", name, host.output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type mrLockedWriter struct{ host *mrHost }

func (w mrLockedWriter) Write(p []byte) (int, error) {
	w.host.mu.Lock()
	defer w.host.mu.Unlock()
	return w.host.output.Write(p)
}

func (h *mrHost) kill(t *testing.T) {
	t.Helper()
	if h.cmd.Process == nil || h.cmd.ProcessState != nil {
		return
	}
	_ = h.cmd.Process.Kill()
	_ = h.cmd.Wait()
}

func (h *mrHost) post(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Error(err)
		return 0, nil
	}
	response, err := h.client.Post(h.baseURL+path, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Errorf("POST %s on %s: %v", path, h.name, err)
		return 0, nil
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, data
}

func (h *mrHost) getJSON(t *testing.T, path string, out any) {
	t.Helper()
	response, err := h.client.Get(h.baseURL + path)
	if err != nil {
		t.Fatalf("GET %s on %s: %v", path, h.name, err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s on %s: %d %s", path, h.name, response.StatusCode, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode GET %s on %s: %v; body=%s", path, h.name, err, data)
	}
}

func (h *mrHost) getWorkflow(t *testing.T, id string) workflowResponse {
	t.Helper()
	var workflow workflowResponse
	h.getJSON(t, "/workflows/"+id, &workflow)
	return workflow
}

func (h *mrHost) waitForStatus(t *testing.T, id string, want workflowkit.Status) workflowResponse {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		workflow := h.getWorkflow(t, id)
		if workflow.Status == string(want) {
			return workflow
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow %s on host %s = %+v, want %s", id, h.name, workflow, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mrWriteLLMKitConfig(t *testing.T, home, providerURL string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`
accounts:
  - alias: mr-account
    provider: openai_compatible
    base_url: %q
    api_key_env: %s
    max_concurrency: 8
models:
  - alias: mr-model
    model: mr-model
    provider: openai_compatible
    account_alias: mr-account
    capability_level: advanced
    supports_tools: false
    supports_json: true
    context_window_class: long
    price_class: free
    latency_class: fast
    max_concurrency: 8
routing:
  defaults:
    complexity: simple
    latency_requirement: normal
    failure_cost: low
    privacy_level: cloud_allowed
`, strings.TrimRight(providerURL, "/")+"/v1", mrProviderAPIKeyEnv)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(strings.TrimSpace(config)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mrProviderStub is an OpenAI-compatible provider that answers with text and
// holds any request mentioning mrBlockMarker until the test ends.
type mrProviderStub struct {
	server  *httptest.Server
	release chan struct{}
	mu      sync.Mutex
	blocked int
	notify  chan struct{}
}

func newMRProviderStub(t *testing.T) *mrProviderStub {
	t.Helper()
	stub := &mrProviderStub{release: make(chan struct{}), notify: make(chan struct{}, 1)}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+mrProviderAPIKey {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(mrBlockMarker)) {
			stub.mu.Lock()
			stub.blocked++
			stub.mu.Unlock()
			select {
			case stub.notify <- struct{}{}:
			default:
			}
			select {
			case <-stub.release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "multi-replica response"}}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
		})
	}))
	t.Cleanup(func() {
		close(stub.release)
		stub.server.Close()
	})
	return stub
}

func (s *mrProviderStub) waitForBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-s.notify:
	case <-time.After(30 * time.Second):
		t.Fatal("blocked workflow never reached the provider")
	}
}

func (s *mrProviderStub) blockedCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked
}
