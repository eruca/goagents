//go:build darwin && cgo && hostapisystemsmoke

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
	workflowsql "github.com/eruca/goagents/workflowkit/sqlitestore"
)

const (
	stabilityRequestTimeout           = 2 * time.Second
	stabilityBatchTimeout             = 10 * time.Second
	stabilityConvergenceTimeout       = 30 * time.Second
	maxFDOverBaseline                 = 5
	maxFDGrowthBetweenWaves           = 2
	maxRSSOverBaseline          int64 = 64 << 20
	maxRSSGrowthSecondWave      int64 = 32 << 20
)

type stabilityCall[T any] struct {
	Value    T
	Status   int
	Duration time.Duration
	Err      error
}

type namedStabilityCall[T any] struct {
	ID   string
	Call stabilityCall[T]
}

type processResourceSnapshot struct {
	RSSBytes        int64
	FileDescriptors int
}

type stabilityWaveResult struct {
	IDs                []string
	SubmitLatencies    []time.Duration
	ApproveLatencies   []time.Duration
	WaitingConvergence time.Duration
	SuccessConvergence time.Duration
	Resources          processResourceSnapshot
}

func TestWaitForStabilityWorkflowsRejectsMatchAfterDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(20 * time.Millisecond)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(workflowResponse{
			ID:     "wf-late",
			Status: string(workflowkit.StatusSucceeded),
		})
	}))
	t.Cleanup(server.Close)

	process := &hostProcess{
		baseURL: server.URL,
		client:  &http.Client{Timeout: time.Second},
	}
	_, err := waitForStabilityWorkflows(
		process,
		[]string{"wf-late"},
		workflowkit.StatusSucceeded,
		5*time.Millisecond,
	)
	if err == nil {
		t.Fatal("workflow matched after the convergence deadline, want timeout")
	}
}

func TestOpenStabilityDatabaseReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := workflowsql.Open(path)
	if err != nil {
		t.Fatalf("create workflow database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close workflow database: %v", err)
	}

	database, err := openStabilityDatabaseReadOnly(path)
	if err != nil {
		t.Fatalf("open workflow database read-only: %v", err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE stability_write_must_fail (id TEXT)`); err == nil {
		t.Fatal("write through stability database connection succeeded, want read-only failure")
	}
}

func TestHostAPIProcessMVPStability(t *testing.T) {
	requireInteractiveLoginKeychain(t)
	requireStabilityResourceTools(t)

	const (
		waves       = 3
		perWave     = 50
		concurrency = 10
	)
	if waves*perWave != 150 {
		t.Fatal("stability workload must remain 3x50")
	}

	binary := buildHostBinary(t)
	provider := newMVPProviderStub(t, mvpProviderReady)
	oidc := newOIDCTestProvider(t)
	runtimeHome := t.TempDir()
	writeMVPLLMKitConfig(t, runtimeHome, provider.URL())
	token := oidc.mintToken(t, "operator-mvp-stability", "host-api", time.Now().Add(time.Hour))
	keychainService := fmt.Sprintf("%s.smoke.stability.%d", localApprovalKeychainService, time.Now().UnixNano())
	cleanupKeychain := smokeKeychainCleanup(t, keychainService, localApprovalKeyID)
	t.Cleanup(cleanupKeychain)
	environment := map[string]string{
		hostAPISkillRootEnv:  "",
		mvpProviderAPIKeyEnv: mvpProviderAPIKey,
	}

	first := startHostProcessWithEnv(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment)
	first.client.Timeout = stabilityRequestTimeout
	baselineFirst := runStabilityWave(t, first, token, 0, 1, 1)
	waveOne := runStabilityWave(t, first, token, 1, perWave, concurrency)
	assertProcessResourceBounds(t, "wave 1", baselineFirst.Resources, waveOne.Resources)
	waveTwo := runStabilityWave(t, first, token, 2, perWave, concurrency)
	assertProcessResourceBounds(t, "wave 2", baselineFirst.Resources, waveTwo.Resources)
	assertSecondWaveResourceGrowth(t, waveOne.Resources, waveTwo.Resources)
	stopHostProcess(t, first)

	formalIDs := append(append([]string(nil), waveOne.IDs...), waveTwo.IDs...)
	restartStarted := time.Now()
	second := startHostProcessWithEnv(t, binary, runtimeHome, oidc.issuer, keychainService, localApprovalKeyID, environment)
	second.client.Timeout = stabilityRequestTimeout
	if err := verifyStabilityWorkflows(second, formalIDs, workflowkit.StatusSucceeded, stabilityConvergenceTimeout); err != nil {
		t.Fatalf("verify first 100 workflows after restart: %v", err)
	}
	if elapsed := time.Since(restartStarted); elapsed > stabilityConvergenceTimeout {
		t.Fatalf("restart readiness and verification took %s, want <= %s", elapsed, stabilityConvergenceTimeout)
	}

	baselineSecond := runStabilityWave(t, second, token, 4, 1, 1)
	waveThree := runStabilityWave(t, second, token, 3, perWave, concurrency)
	assertProcessResourceBounds(t, "wave 3", baselineSecond.Resources, waveThree.Resources)
	formalIDs = append(formalIDs, waveThree.IDs...)

	requests := provider.Requests()
	if len(requests) != 152 {
		t.Fatalf("provider requests = %d, want 152", len(requests))
	}
	for _, request := range requests {
		if request.Authorization != "Bearer "+mvpProviderAPIKey {
			t.Fatal("provider authorization was not the synthetic stability key")
		}
	}

	stopHostProcess(t, second)
	processOutput := first.output.String() + second.output.String()
	if strings.Contains(processOutput, mvpProviderAPIKey) {
		t.Fatal("host process output leaked the synthetic provider key")
	}
	if strings.Contains(processOutput, token) {
		t.Fatal("host process output leaked the OIDC bearer token")
	}
	assertStabilityRuntime(t, runtimeHome, formalIDs)
	cleanupKeychain()
}

func runStabilityWave(
	t *testing.T,
	process *hostProcess,
	token string,
	wave int,
	count int,
	concurrency int,
) stabilityWaveResult {
	t.Helper()
	ids := make([]string, count)
	for index := range count {
		ids[index] = fmt.Sprintf("wf-mvp-stability-w%d-%03d", wave, index)
	}

	beforeWorker, status := processJSON[queuedWorkerStatusResponse](t, process, http.MethodGet, "/workers/queued", nil, "")
	if status != http.StatusOK {
		t.Fatalf("wave %d worker status before submit = %d, want 200", wave, status)
	}

	submitStarted := time.Now()
	submits := runStabilityBatch(ids, concurrency, func(id string) stabilityCall[workflowResponse] {
		return callProcessJSON[workflowResponse](process, http.MethodPost, "/workflows", map[string]any{
			"id":       id,
			"input":    "MVP single-host stability workflow " + id,
			"run_mode": string(RunModeQueued),
			"task_profile": map[string]any{
				"complexity": "simple",
				"privacy":    "cloud_allowed",
			},
		}, "")
	})
	submitElapsed := time.Since(submitStarted)
	if submitElapsed > stabilityBatchTimeout {
		t.Fatalf("wave %d submissions took %s, want <= %s", wave, submitElapsed, stabilityBatchTimeout)
	}
	submitLatencies := make([]time.Duration, 0, count)
	for _, id := range ids {
		call := submits[id]
		if call.Err != nil {
			t.Fatalf("wave %d submit %s: %v", wave, id, call.Err)
		}
		if call.Status != http.StatusAccepted || call.Value.Status != string(workflowkit.StatusPending) {
			t.Fatalf("wave %d submit %s status=%d workflow=%+v, want 202 pending", wave, id, call.Status, call.Value)
		}
		if call.Duration > stabilityRequestTimeout {
			t.Fatalf("wave %d submit %s took %s, want <= %s", wave, id, call.Duration, stabilityRequestTimeout)
		}
		submitLatencies = append(submitLatencies, call.Duration)
	}

	waitingStarted := time.Now()
	waiting, err := waitForStabilityWorkflows(process, ids, workflowkit.StatusWaitingApproval, stabilityConvergenceTimeout)
	if err != nil {
		t.Fatalf("wave %d waiting approval convergence: %v", wave, err)
	}
	waitingElapsed := time.Since(waitingStarted)

	approveStarted := time.Now()
	approvals := runStabilityBatch(ids, concurrency, func(id string) stabilityCall[workflowResponse] {
		return callProcessJSON[workflowResponse](process, http.MethodPost, "/workflows/"+id+"/approve", map[string]string{
			"note": "MVP stability accepted",
		}, "Bearer "+token)
	})
	approveElapsed := time.Since(approveStarted)
	if approveElapsed > stabilityBatchTimeout {
		t.Fatalf("wave %d approvals took %s, want <= %s", wave, approveElapsed, stabilityBatchTimeout)
	}
	approveLatencies := make([]time.Duration, 0, count)
	for _, id := range ids {
		call := approvals[id]
		if call.Err != nil {
			t.Fatalf("wave %d approve %s: %v", wave, id, call.Err)
		}
		if call.Status != http.StatusOK || call.Value.Status != string(workflowkit.StatusSucceeded) {
			t.Fatalf("wave %d approve %s status=%d workflow=%+v, want 200 succeeded", wave, id, call.Status, call.Value)
		}
		approveLatencies = append(approveLatencies, call.Duration)
	}

	succeeded, err := waitForStabilityWorkflows(process, ids, workflowkit.StatusSucceeded, stabilityConvergenceTimeout)
	if err != nil {
		t.Fatalf("wave %d success convergence: %v", wave, err)
	}
	successElapsed := time.Since(approveStarted)
	if successElapsed > stabilityConvergenceTimeout {
		t.Fatalf("wave %d approval-to-success convergence took %s, want <= %s", wave, successElapsed, stabilityConvergenceTimeout)
	}

	agentRunIDs := make(map[string]string, count)
	for _, id := range ids {
		workflow := succeeded[id]
		if workflow.AgentRunID == "" || workflow.OutputRef == "" || workflow.AgentApproval != nil {
			t.Fatalf("wave %d workflow %s missing stable terminal refs: %+v", wave, id, workflow)
		}
		if previous, exists := agentRunIDs[workflow.AgentRunID]; exists {
			t.Fatalf("wave %d workflows %s and %s share agent run %s", wave, previous, id, workflow.AgentRunID)
		}
		agentRunIDs[workflow.AgentRunID] = id

		runCall := callProcessJSON[agentRunResponse](process, http.MethodGet, "/agent-runs/"+workflow.AgentRunID, nil, "")
		if runCall.Err != nil || runCall.Status != http.StatusOK || runCall.Value.Status != string(runkit.StatusSucceeded) || runCall.Value.Summary.LLMCalls != 1 || runCall.Value.Summary.ToolCalls != 0 {
			t.Fatalf("wave %d agent run for %s call=%+v, want one succeeded LLM call and no tools", wave, id, runCall)
		}

		routeCall := callProcessJSON[llmRoutesResponse](process, http.MethodGet, "/workflows/"+id+"/llm-routes", nil, "")
		if routeCall.Err != nil || routeCall.Status != http.StatusOK || countSuccessfulRoutes(routeCall.Value.Routes) != 1 {
			t.Fatalf("wave %d routes for %s call=%+v, want one successful route", wave, id, routeCall)
		}
		if waiting[id].AgentRunID != workflow.AgentRunID {
			t.Fatalf("wave %d workflow %s agent run changed across final approval: waiting=%s succeeded=%s", wave, id, waiting[id].AgentRunID, workflow.AgentRunID)
		}
	}

	afterWorker := waitForStabilityWorker(t, process, beforeWorker.Claimed+count, beforeWorker.Completed+count)
	if afterWorker.Errors != 0 || afterWorker.HeartbeatErrors != 0 || afterWorker.LastError != "" || afterWorker.LastHeartbeatError != "" {
		t.Fatalf("wave %d worker status=%+v, want no execution or heartbeat errors", wave, afterWorker)
	}
	resources, err := sampleProcessResources(process.cmd.Process.Pid)
	if err != nil {
		t.Fatalf("wave %d sample process resources: %v", wave, err)
	}

	t.Logf(
		"stability wave=%d workflows=%d submit_p50=%s submit_p95=%s waiting=%s approve_p50=%s approve_p95=%s succeeded=%s rss_bytes=%d fds=%d",
		wave,
		count,
		percentileDuration(submitLatencies, 0.50),
		percentileDuration(submitLatencies, 0.95),
		waitingElapsed,
		percentileDuration(approveLatencies, 0.50),
		percentileDuration(approveLatencies, 0.95),
		successElapsed,
		resources.RSSBytes,
		resources.FileDescriptors,
	)

	return stabilityWaveResult{
		IDs:                ids,
		SubmitLatencies:    submitLatencies,
		ApproveLatencies:   approveLatencies,
		WaitingConvergence: waitingElapsed,
		SuccessConvergence: successElapsed,
		Resources:          resources,
	}
}

func callProcessJSON[T any](process *hostProcess, method, path string, body any, authorization string) stabilityCall[T] {
	return callProcessJSONWithContext[T](context.Background(), process, method, path, body, authorization)
}

func callProcessJSONWithContext[T any](ctx context.Context, process *hostProcess, method, path string, body any, authorization string) stabilityCall[T] {
	started := time.Now()
	var result T
	payload, err := json.Marshal(body)
	if err != nil {
		return stabilityCall[T]{Duration: time.Since(started), Err: fmt.Errorf("marshal request: %w", err)}
	}
	request, err := http.NewRequestWithContext(ctx, method, process.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return stabilityCall[T]{Duration: time.Since(started), Err: fmt.Errorf("build request: %w", err)}
	}
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := process.client.Do(request)
	if err != nil {
		return stabilityCall[T]{Duration: time.Since(started), Err: fmt.Errorf("request: %w", err)}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return stabilityCall[T]{Status: response.StatusCode, Duration: time.Since(started), Err: fmt.Errorf("read response: %w", err)}
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return stabilityCall[T]{Status: response.StatusCode, Duration: time.Since(started), Err: fmt.Errorf("decode response %q: %w", strings.TrimSpace(string(responseBody)), err)}
	}
	return stabilityCall[T]{Value: result, Status: response.StatusCode, Duration: time.Since(started)}
}

func runStabilityBatch[T any](ids []string, concurrency int, call func(string) stabilityCall[T]) map[string]stabilityCall[T] {
	jobs := make(chan string)
	results := make(chan namedStabilityCall[T], len(ids))
	for range concurrency {
		go func() {
			for id := range jobs {
				results <- namedStabilityCall[T]{ID: id, Call: call(id)}
			}
		}()
	}
	go func() {
		for _, id := range ids {
			jobs <- id
		}
		close(jobs)
	}()

	byID := make(map[string]stabilityCall[T], len(ids))
	for range ids {
		result := <-results
		byID[result.ID] = result.Call
	}
	return byID
}

func waitForStabilityWorkflows(
	process *hostProcess,
	ids []string,
	want workflowkit.Status,
	timeout time.Duration,
) (map[string]workflowResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	matched := make(map[string]workflowResponse, len(ids))
	lastStatus := make(map[string]string, len(ids))
	for ctx.Err() == nil {
		for _, id := range ids {
			if ctx.Err() != nil {
				break
			}
			if _, ok := matched[id]; ok {
				continue
			}
			call := callProcessJSONWithContext[workflowResponse](ctx, process, http.MethodGet, "/workflows/"+id, nil, "")
			if call.Err != nil {
				if ctx.Err() != nil {
					break
				}
				return nil, fmt.Errorf("GET %s: %w", id, call.Err)
			}
			if call.Status != http.StatusOK {
				return nil, fmt.Errorf("GET %s status=%d, want 200", id, call.Status)
			}
			lastStatus[id] = call.Value.Status
			if call.Value.Status == string(want) {
				matched[id] = call.Value
				continue
			}
			if call.Value.Status == string(workflowkit.StatusFailed) || call.Value.Status == string(workflowkit.StatusCancelled) {
				return nil, fmt.Errorf("workflow %s reached terminal status %s while waiting for %s", id, call.Value.Status, want)
			}
		}
		if len(matched) == len(ids) && ctx.Err() == nil {
			return matched, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(25 * time.Millisecond):
		}
	}
	missing := make([]string, 0, len(ids)-len(matched))
	for _, id := range ids {
		if _, ok := matched[id]; !ok {
			missing = append(missing, fmt.Sprintf("%s=%s", id, lastStatus[id]))
		}
	}
	return nil, fmt.Errorf("%d workflows did not reach %s within %s: %s", len(missing), want, timeout, strings.Join(missing, ", "))
}

func verifyStabilityWorkflows(process *hostProcess, ids []string, want workflowkit.Status, timeout time.Duration) error {
	_, err := waitForStabilityWorkflows(process, ids, want, timeout)
	return err
}

func waitForStabilityWorker(t *testing.T, process *hostProcess, claimed, completed int) queuedWorkerStatusResponse {
	t.Helper()
	deadline := time.Now().Add(stabilityConvergenceTimeout)
	var last queuedWorkerStatusResponse
	for time.Now().Before(deadline) {
		call := callProcessJSON[queuedWorkerStatusResponse](process, http.MethodGet, "/workers/queued", nil, "")
		if call.Err != nil {
			t.Fatalf("read queued worker status: %v", call.Err)
		}
		if call.Status != http.StatusOK {
			t.Fatalf("queued worker status=%d, want 200", call.Status)
		}
		last = call.Value
		if last.Claimed >= claimed && last.Completed >= completed {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("queued worker status=%+v, want claimed >= %d and completed >= %d", last, claimed, completed)
	return queuedWorkerStatusResponse{}
}

func countSuccessfulRoutes(routes []llmRouteResponse) int {
	count := 0
	for _, route := range routes {
		if route.Outcome != nil && route.Outcome.Success {
			count++
		}
	}
	return count
}

func percentileDuration(values []time.Duration, percentile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(float64(len(sorted))*percentile)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func requireStabilityResourceTools(t *testing.T) {
	t.Helper()
	for _, command := range []string{"ps", "lsof"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("MVP stability requires %s: %v", command, err)
		}
	}
	if _, err := sampleProcessResources(os.Getpid()); err != nil {
		t.Skipf("MVP stability cannot sample process resources in this environment: %v", err)
	}
}

func sampleProcessResources(pid int) (processResourceSnapshot, error) {
	psContext, cancelPS := context.WithTimeout(context.Background(), stabilityRequestTimeout)
	psOutput, err := exec.CommandContext(psContext, "ps", "-o", "rss=", "-p", strconv.Itoa(pid)).CombinedOutput()
	cancelPS()
	if err != nil {
		return processResourceSnapshot{}, fmt.Errorf("read RSS: %w: %s", err, strings.TrimSpace(string(psOutput)))
	}
	rssKiB, err := strconv.ParseInt(strings.TrimSpace(string(psOutput)), 10, 64)
	if err != nil {
		return processResourceSnapshot{}, fmt.Errorf("parse RSS %q: %w", strings.TrimSpace(string(psOutput)), err)
	}
	if rssKiB <= 0 {
		return processResourceSnapshot{}, fmt.Errorf("RSS %d KiB must be positive", rssKiB)
	}
	lsofContext, cancelLSOF := context.WithTimeout(context.Background(), stabilityRequestTimeout)
	lsofOutput, err := exec.CommandContext(lsofContext, "lsof", "-nP", "-a", "-p", strconv.Itoa(pid), "-Fn").CombinedOutput()
	cancelLSOF()
	if err != nil {
		return processResourceSnapshot{}, fmt.Errorf("read file descriptors: %w: %s", err, strings.TrimSpace(string(lsofOutput)))
	}
	fileDescriptors := 0
	for _, line := range strings.Split(string(lsofOutput), "\n") {
		if strings.HasPrefix(line, "f") {
			fileDescriptors++
		}
	}
	if fileDescriptors == 0 {
		return processResourceSnapshot{}, fmt.Errorf("lsof returned no file descriptors for pid %d", pid)
	}
	return processResourceSnapshot{RSSBytes: rssKiB * 1024, FileDescriptors: fileDescriptors}, nil
}

func openStabilityDatabaseReadOnly(path string) (*sql.DB, error) {
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if err := database.PingContext(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

func assertProcessResourceBounds(t *testing.T, label string, baseline, current processResourceSnapshot) {
	t.Helper()
	fdDelta := current.FileDescriptors - baseline.FileDescriptors
	rssDelta := current.RSSBytes - baseline.RSSBytes
	if fdDelta > maxFDOverBaseline {
		t.Fatalf("%s file descriptors baseline=%d current=%d delta=%d, want delta <= %d", label, baseline.FileDescriptors, current.FileDescriptors, fdDelta, maxFDOverBaseline)
	}
	if rssDelta > maxRSSOverBaseline {
		t.Fatalf("%s RSS baseline=%d current=%d delta=%d, want delta <= %d", label, baseline.RSSBytes, current.RSSBytes, rssDelta, maxRSSOverBaseline)
	}
}

func assertSecondWaveResourceGrowth(t *testing.T, first, second processResourceSnapshot) {
	t.Helper()
	fdDelta := second.FileDescriptors - first.FileDescriptors
	rssDelta := second.RSSBytes - first.RSSBytes
	if fdDelta > maxFDGrowthBetweenWaves {
		t.Fatalf("wave 2 file descriptors wave1=%d wave2=%d delta=%d, want delta <= %d", first.FileDescriptors, second.FileDescriptors, fdDelta, maxFDGrowthBetweenWaves)
	}
	if rssDelta > maxRSSGrowthSecondWave {
		t.Fatalf("wave 2 RSS wave1=%d wave2=%d delta=%d, want delta <= %d", first.RSSBytes, second.RSSBytes, rssDelta, maxRSSGrowthSecondWave)
	}
}

func assertStabilityRuntime(t *testing.T, runtimeHome string, ids []string) {
	t.Helper()
	database, err := openStabilityDatabaseReadOnly(filepath.Join(runtimeHome, "workflow.db"))
	if err != nil {
		t.Fatalf("open stability workflow database read-only: %v", err)
	}
	defer database.Close()
	for _, id := range ids {
		var status string
		var leaseOwner string
		var leaseUntil time.Time
		err := database.QueryRowContext(context.Background(), `
SELECT status, lease_owner, lease_until
FROM workflow_runs
WHERE id = ?
`, id).Scan(&status, &leaseOwner, &leaseUntil)
		if err != nil {
			t.Fatalf("read persisted stability workflow %s: %v", id, err)
		}
		if status != string(workflowkit.StatusSucceeded) || leaseOwner != "" || !leaseUntil.IsZero() {
			t.Fatalf(
				"persisted stability workflow %s status=%s lease_owner=%q lease_until=%s, want succeeded with cleared lease",
				id,
				status,
				leaseOwner,
				leaseUntil,
			)
		}
	}
}
