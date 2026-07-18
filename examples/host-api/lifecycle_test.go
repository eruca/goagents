package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eruca/goagents/artifactkit"
	"github.com/eruca/goagents/hostkit"
	"github.com/eruca/goagents/runkit"
	"github.com/eruca/goagents/workflowkit"
)

func TestHostAPIServiceStartBindsBeforeStartingBackgroundComponents(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback address: %v", err)
	}
	defer occupied.Close()

	server := newBareLifecycleServer()
	var stdout bytes.Buffer
	service := newHostAPIService(server, occupied.Addr().String(), &stdout)
	result := hostkit.Run(t.Context(), service, nil, hostkit.Options{
		DrainTimeout:   time.Second,
		CleanupTimeout: time.Second,
	})

	if result.Code() != string(hostkit.CodeListenFailed) {
		t.Fatalf("Run() code = %q, want %q; err=%v", result.Code(), hostkit.CodeListenFailed, result.Err())
	}
	if result.Err() == nil || result.Err().Error() != "host API listen failed" {
		t.Fatalf("Run() error = %v, want safe listen failure", result.Err())
	}
	if strings.Contains(result.Err().Error(), occupied.Addr().String()) {
		t.Fatalf("safe error leaked listen address: %v", result.Err())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty output after listen failure", stdout.String())
	}
	if server.worker.snapshot().Started {
		t.Fatal("queued worker started before the listener bound")
	}
	workerStartAvailable := false
	server.workerStart.Do(func() { workerStartAvailable = true })
	if !workerStartAvailable {
		t.Fatal("queued worker start once was consumed after listen failure")
	}
	janitorStartAvailable := false
	server.janitorStart.Do(func() { janitorStartAvailable = true })
	if !janitorStartAvailable {
		t.Fatal("approval janitor start once was consumed after listen failure")
	}
}

func TestHostAPIServiceReportsUnexpectedServeFailure(t *testing.T) {
	server := newBareLifecycleServer()
	stdout := newSignallingWriter()
	service := newHostAPIService(server, "127.0.0.1:0", stdout)
	resultCh := make(chan hostkit.Result, 1)
	go func() {
		resultCh <- hostkit.Run(t.Context(), service, nil, hostkit.Options{
			DrainTimeout:   time.Second,
			CleanupTimeout: time.Second,
		})
	}()

	<-stdout.wrote
	if got, want := stdout.String(), "host_api_addr="+service.listener.Addr().String()+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if err := service.listener.Close(); err != nil {
		t.Fatalf("close listener unexpectedly: %v", err)
	}

	result := <-resultCh
	if result.Code() != string(hostkit.CodeServeFailed) {
		t.Fatalf("Run() code = %q, want %q; err=%v", result.Code(), hostkit.CodeServeFailed, result.Err())
	}
	if result.Err() == nil || result.Err().Error() != "host API serve failed" {
		t.Fatalf("Run() error = %v, want safe serve failure", result.Err())
	}
	select {
	case duplicate := <-service.Done():
		t.Fatalf("Done() emitted more than once: %v", duplicate)
	default:
	}
}

func TestHostAPIServiceDrainStopsIntakeAndWaitsExecutions(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	blockingArtifacts := &blockingArtifactStore{
		Store:   server.artifacts,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	server.artifacts = blockingArtifacts
	service := newHostAPIService(server, "127.0.0.1:0", io.Discard)
	if err := service.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"http://"+service.listener.Addr().String()+"/workflows",
		bytes.NewBufferString(`{"id":"wf-drain","input":"hello"}`),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	responseCh := make(chan *http.Response, 1)
	requestErrCh := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			requestErrCh <- requestErr
			return
		}
		responseCh <- response
	}()

	<-blockingArtifacts.started
	drainResult := make(chan error, 1)
	go func() {
		drainResult <- service.Drain(t.Context())
	}()
	<-service.intakeCtx.Done()
	if err := service.executionCtx.Err(); err != nil {
		t.Fatalf("execution context was cancelled during Drain(): %v", err)
	}
	if err := server.WaitQueuedWorker(t.Context()); err != nil {
		t.Fatalf("WaitQueuedWorker() error = %v", err)
	}
	if err := server.WaitAgentApprovalJanitor(t.Context()); err != nil {
		t.Fatalf("WaitAgentApprovalJanitor() error = %v", err)
	}
	select {
	case err := <-drainResult:
		t.Fatalf("Drain() returned before the active request completed: %v", err)
	default:
	}

	close(blockingArtifacts.release)
	select {
	case requestErr := <-requestErrCh:
		t.Fatalf("request failed: %v", requestErr)
	case response := <-responseCh:
		defer response.Body.Close()
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("response status = %d, want %d", response.StatusCode, http.StatusAccepted)
		}
	}
	if err := <-drainResult; err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if err := <-service.Done(); err != nil {
		t.Fatalf("Done() error after graceful drain = %v", err)
	}
	if _, err := server.workflows.Get(t.Context(), "wf-drain"); err != nil {
		t.Fatalf("workflow store was closed by Drain(): %v", err)
	}
	if err := service.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestHostAPIServiceForceStopWaitsForShortHandlers(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T) (*Server, lifecycleShortRequest, *lifecycleStoreBarrier)
	}{
		{
			name:  "queued create",
			setup: setupQueuedCreateShortRequest,
		},
		{
			name:  "requeue",
			setup: setupRequeueShortRequest,
		},
		{
			name:  "agent approval reject",
			setup: setupAgentRejectShortRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, request, barrier := test.setup(t)
			service := startLifecycleService(t, server)
			requestDone := startLifecycleHTTPRequest(
				t,
				service,
				request.method,
				request.path,
				request.body,
				request.authorization,
			)
			waitLifecycleSignal(t, barrier.entered, "short handler store boundary")

			forceResult := make(chan error, 1)
			go func() {
				forceResult <- service.ForceStop(t.Context())
			}()
			waitLifecycleSignal(t, barrier.contextCancelled, "short handler request cancellation")
			if doneErr := <-service.Done(); doneErr != nil {
				t.Fatalf("Done() error after ForceStop(): %v", doneErr)
			}
			waitLifecycleSignal(t, server.workerDone, "queued worker exit")
			waitLifecycleSignal(t, server.janitorDone, "approval janitor exit")
			waitBound := time.NewTimer(50 * time.Millisecond)
			returnedEarly := false
			var forceErr error
			select {
			case forceErr = <-forceResult:
				returnedEarly = true
				if !waitBound.Stop() {
					<-waitBound.C
				}
			case <-waitBound.C:
			}

			close(barrier.release)
			waitLifecycleSignal(t, barrier.returned, "short handler store return")
			waitLifecycleSignal(t, requestDone, "short handler return")
			if !returnedEarly {
				forceErr = <-forceResult
			}
			if forceErr != nil {
				t.Fatalf("ForceStop() after the short handler exited = %v", forceErr)
			}
			if err := service.Close(t.Context()); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			if returnedEarly {
				t.Fatal("ForceStop() returned before the short handler exited")
			}
		})
	}
}

func TestHostAPIServiceForceStopShortHandlerConsumesOnlyCleanupBudget(t *testing.T) {
	server, request, barrier := setupQueuedCreateShortRequest(t)
	service := startLifecycleService(t, server)
	requestDone := startLifecycleHTTPRequest(
		t,
		service,
		request.method,
		request.path,
		request.body,
		request.authorization,
	)
	waitLifecycleSignal(t, barrier.entered, "short handler store boundary")

	cleanupCtx, cancelCleanup := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelCleanup()
	if err := service.ForceStop(cleanupCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ForceStop() error = %v, want context.DeadlineExceeded while short handler remains active", err)
	}

	close(barrier.release)
	waitLifecycleSignal(t, barrier.returned, "short handler store return")
	waitLifecycleSignal(t, requestDone, "short handler return")
	if err := service.Close(t.Context()); err != nil {
		t.Fatalf("Close() after short handler exit = %v", err)
	}
}

func TestHostAPIServiceForceStopCancelsExecutionsAndRunsCleanup(t *testing.T) {
	tests := []struct {
		name       string
		kind       executionKind
		startOwner func(*testing.T) *lifecycleForceOwner
		assert     func(*testing.T, *lifecycleForceOwner)
	}{
		{
			name:       "sync workflow handler",
			kind:       executionSyncWorkflow,
			startOwner: startSyncWorkflowOwner,
			assert:     assertWorkflowForceStopped,
		},
		{
			name:       "queued workflow worker",
			kind:       executionQueuedWorkflow,
			startOwner: startQueuedWorkflowOwner,
			assert:     assertQueuedWorkflowForceStopped,
		},
		{
			name:       "final workflow approval handler",
			kind:       executionFinalApproval,
			startOwner: startFinalApprovalOwner,
			assert:     assertWorkflowForceStopped,
		},
		{
			name:       "agent approval handler",
			kind:       executionAgentApproval,
			startOwner: startAgentApprovalOwner,
			assert:     assertAgentApprovalForceStopped,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owner := test.startOwner(t)
			snapshot := requireLifecycleOwnerSnapshot(t, owner.server, test.kind)

			forceResult := make(chan error, 1)
			go func() {
				forceResult <- owner.service.ForceStop(t.Context())
			}()
			<-owner.service.executionCtx.Done()
			owner.release()
			if err := <-forceResult; err != nil {
				t.Fatalf("ForceStop() error = %v", err)
			}
			<-owner.done

			assertChannelClosed(t, snapshot.done, "production owner done")
			assertChannelClosed(t, owner.server.workerDone, "worker done")
			assertChannelClosed(t, owner.server.janitorDone, "janitor done")
			if !errors.Is(owner.service.intakeCtx.Err(), context.Canceled) {
				t.Fatalf("intake context error = %v, want context.Canceled", owner.service.intakeCtx.Err())
			}
			if doneErr := <-owner.service.Done(); doneErr != nil {
				t.Fatalf("Done() error after ForceStop() = %v", doneErr)
			}
			test.assert(t, owner)
			if err := owner.service.Close(t.Context()); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		})
	}
}

func TestHostAPIServiceForceStopReconcilesFailOnceWorkflowState(t *testing.T) {
	tests := []struct {
		name   string
		inject func(*Server, *workflowkit.MemoryStore, *runkit.MemoryStore) error
	}{
		{
			name: "workflow write",
			inject: func(server *Server, workflows *workflowkit.MemoryStore, _ *runkit.MemoryStore) error {
				writeErr := errors.New("workflow write failed once")
				server.workflows = &failOnceLifecycleWorkflowStore{Store: workflows, err: writeErr}
				return writeErr
			},
		},
		{
			name: "AgentRun write",
			inject: func(server *Server, _ *workflowkit.MemoryStore, runs *runkit.MemoryStore) error {
				writeErr := errors.New("AgentRun write failed once")
				server.runs = &failOnceLifecycleRunStore{Store: runs, err: writeErr}
				return writeErr
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const workflowID = "wf-force-reconcile"
			const agentRunID = "agent-force-reconcile"
			server := newBareLifecycleServer()
			workflows := workflowkit.NewMemoryStore()
			runs := runkit.NewMemoryStore()
			server.workflows = workflows
			server.runs = runs
			saveLifecycleTestAgentRun(t, runs, runkit.RunRecord{
				RunID:      agentRunID,
				WorkflowID: workflowID,
				Status:     runkit.StatusRunning,
			})
			saveLifecycleTestRun(t, workflows, workflowkit.WorkflowRun{
				ID:         workflowID,
				Status:     workflowkit.StatusRunning,
				AgentRunID: agentRunID,
			})
			writeErr := test.inject(server, workflows, runs)
			service := startLifecycleService(t, server)
			handle, accepted := server.executions.Begin(workflowID, executionSyncWorkflow, func(ctx context.Context) error {
				return server.finalizeWorkflowShutdown(ctx, workflowID)
			})
			if !accepted {
				t.Fatal("execution registry rejected reconciliation owner")
			}
			go func() {
				<-service.executionCtx.Done()
				handle.Done()
			}()

			if err := service.ForceStop(t.Context()); err != nil {
				t.Fatalf("ForceStop() error = %v, want fail-once state to converge in the same cleanup budget; first error=%v", err, writeErr)
			}
			workflow := getLifecycleTestRun(t, workflows, workflowID)
			if workflow.Status != workflowkit.StatusFailed || workflow.Error != hostShutdownTimeoutCode {
				t.Fatalf("workflow after ForceStop() = %q/%q, want shutdown failed", workflow.Status, workflow.Error)
			}
			agentRun := getLifecycleAgentRun(t, runs, agentRunID)
			if agentRun.Status != runkit.StatusFailed ||
				agentRun.Summary.Status != runkit.StatusFailed ||
				agentRun.Summary.AbortReason != hostShutdownTimeoutCode {
				t.Fatalf("AgentRun after ForceStop() = status %q summary %q abort %q, want shutdown failed",
					agentRun.Status, agentRun.Summary.Status, agentRun.Summary.AbortReason)
			}
			if err := service.Close(t.Context()); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		})
	}
}

func TestHostAPIServiceForceStopPermanentCleanupFailureExhaustsBudgetWithoutClosingStores(t *testing.T) {
	server := newBareLifecycleServer()
	workflows := workflowkit.NewMemoryStore()
	runs := runkit.NewMemoryStore()
	callSignals := map[string]chan struct{}{
		"wf-permanent-first":  make(chan struct{}, 8),
		"wf-permanent-second": make(chan struct{}, 8),
	}
	firstErr := errors.New("first workflow cleanup remains unavailable")
	secondErr := errors.New("second workflow cleanup remains unavailable")
	server.workflows = &alwaysFailLifecycleWorkflowStore{
		Store: workflows,
		errs: map[string]error{
			"wf-permanent-first":  firstErr,
			"wf-permanent-second": secondErr,
		},
		calls: callSignals,
	}
	server.runs = runs
	recorder := newCloseRecorder()
	server.workflowCloser = recorder.closer("workflow", nil)
	server.runCloser = recorder.closer("run", nil)
	for workflowID, agentRunID := range map[string]string{
		"wf-permanent-first":  "agent-permanent-first",
		"wf-permanent-second": "agent-permanent-second",
	} {
		saveLifecycleTestAgentRun(t, runs, runkit.RunRecord{
			RunID:      agentRunID,
			WorkflowID: workflowID,
			Status:     runkit.StatusRunning,
		})
		saveLifecycleTestRun(t, workflows, workflowkit.WorkflowRun{
			ID:         workflowID,
			Status:     workflowkit.StatusRunning,
			AgentRunID: agentRunID,
		})
	}

	service := startLifecycleService(t, server)
	for workflowID := range callSignals {
		workflowID := workflowID
		handle, accepted := server.executions.Begin(workflowID, executionSyncWorkflow, func(ctx context.Context) error {
			return server.finalizeWorkflowShutdown(ctx, workflowID)
		})
		if !accepted {
			t.Fatalf("execution registry rejected %q", workflowID)
		}
		go func() {
			<-service.executionCtx.Done()
			handle.Done()
		}()
	}

	cleanupCtx, cancelCleanup := context.WithCancel(t.Context())
	forceResult := make(chan error, 1)
	go func() {
		forceResult <- service.ForceStop(cleanupCtx)
	}()
	for workflowID, calls := range callSignals {
		for attempt := 1; attempt <= 2; attempt++ {
			select {
			case <-calls:
			case err := <-forceResult:
				t.Fatalf("ForceStop() returned before %q received reconciliation attempt %d: %v", workflowID, attempt, err)
			case <-t.Context().Done():
				t.Fatalf("timed out waiting for %q reconciliation attempt %d", workflowID, attempt)
			}
		}
	}
	cancelCleanup()
	err := <-forceResult
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("ForceStop() error = %v, want both final cleanup errors and context.Canceled", err)
	}
	if err := service.Close(cleanupCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want exhausted cleanup context", err)
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("stores closed after cleanup budget expired: %v", got)
	}
}

func TestHostAPIServiceForceStopReconcilesAllSnapshotsAfterErrors(t *testing.T) {
	server := newBareLifecycleServer()
	service := startLifecycleService(t, server)
	firstErr := errors.New("first force cleanup failed")
	secondErr := errors.New("second force cleanup failed")
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	first, accepted := server.executions.Begin("wf-force-first", executionSyncWorkflow, func(context.Context) error {
		if firstCalls.Add(1) == 1 {
			return firstErr
		}
		return nil
	})
	if !accepted {
		t.Fatal("first execution was rejected")
	}
	second, accepted := server.executions.Begin("wf-force-second", executionQueuedWorkflow, func(context.Context) error {
		if secondCalls.Add(1) == 1 {
			return secondErr
		}
		return nil
	})
	if !accepted {
		t.Fatal("second execution was rejected")
	}
	executionsDone := make(chan struct{})
	go func() {
		<-service.executionCtx.Done()
		first.Done()
		second.Done()
		close(executionsDone)
	}()

	err := service.ForceStop(t.Context())
	if err != nil {
		t.Fatalf("ForceStop() error = %v, want both fail-once cleanups to converge", err)
	}
	<-executionsDone
	if got := firstCalls.Load(); got != 2 {
		t.Fatalf("first ForceStop cleanup calls = %d, want 2 after one failure", got)
	}
	if got := secondCalls.Load(); got != 2 {
		t.Fatalf("second ForceStop cleanup calls = %d, want 2 after one failure", got)
	}
	if doneErr := <-service.Done(); doneErr != nil {
		t.Fatalf("Done() error after ForceStop() = %v", doneErr)
	}
	if err := service.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestHostAPIServiceCloseClosesStoresInReverseOpenOrderOnce(t *testing.T) {
	recorder := newCloseRecorder()
	workflowErr := errors.New("workflow close failed")
	runErr := errors.New("run close failed")
	server := &Server{
		executions:     newExecutionRegistry(),
		workflowCloser: recorder.closer("workflow", workflowErr),
		runCloser:      recorder.closer("run", runErr),
	}
	service := newHostAPIService(server, "127.0.0.1:0", io.Discard)

	firstErr := service.Close(t.Context())
	if !errors.Is(firstErr, workflowErr) || !errors.Is(firstErr, runErr) {
		t.Fatalf("first Close() error = %v, want both close errors", firstErr)
	}
	secondErr := service.Close(t.Context())
	if !errors.Is(secondErr, workflowErr) || !errors.Is(secondErr, runErr) {
		t.Fatalf("second Close() error = %v, want stable joined close errors", secondErr)
	}
	if got, want := recorder.snapshot(), []string{"workflow", "run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("close order = %v, want %v", got, want)
	}
}

func TestHostAPIServiceCleanupTimeoutDoesNotCloseStoresUnderActiveExecution(t *testing.T) {
	recorder := newCloseRecorder()
	server := &Server{
		executions:     newExecutionRegistry(),
		workflowCloser: recorder.closer("workflow", nil),
		runCloser:      recorder.closer("run", nil),
	}
	service := newHostAPIService(server, "127.0.0.1:0", io.Discard)
	handle, accepted := server.executions.Begin("wf-active", executionSyncWorkflow, nil)
	if !accepted {
		t.Fatal("execution registry rejected active execution")
	}

	deadlineCtx := newControlledDeadlineContext(t.Context())
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- service.Close(deadlineCtx)
	}()
	<-deadlineCtx.observed
	deadlineCtx.expire()
	if err := <-closeResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want context.DeadlineExceeded", err)
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("stores closed while execution remained active: %v", got)
	}

	handle.Done()
	if err := service.Close(t.Context()); err != nil {
		t.Fatalf("Close() after registry became idle = %v", err)
	}
	if got, want := recorder.snapshot(), []string{"workflow", "run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("close order after retry = %v, want %v", got, want)
	}
}

func TestHostAPIServiceCloseDrainsRegistryBeforeWaitingForIdle(t *testing.T) {
	recorder := newCloseRecorder()
	server := &Server{
		executions:     newExecutionRegistry(),
		workflowCloser: recorder.closer("workflow", nil),
		runCloser:      recorder.closer("run", nil),
	}
	service := newHostAPIService(server, "127.0.0.1:0", io.Discard)
	active, accepted := server.executions.Begin("wf-active", executionSyncWorkflow, nil)
	if !accepted {
		t.Fatal("execution registry rejected initial execution")
	}

	closeCtx := newControlledDeadlineContext(t.Context())
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- service.Close(closeCtx)
	}()
	<-closeCtx.observed

	late, lateAccepted := server.executions.Begin("wf-late", executionQueuedWorkflow, nil)
	if late != nil {
		late.Done()
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("stores closed while the initial execution remained active: %v", got)
	}
	active.Done()
	if err := <-closeResult; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if lateAccepted {
		t.Fatal("execution registry accepted a new execution after Close() started")
	}
	if got, want := recorder.snapshot(), []string{"workflow", "run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("close order = %v, want %v", got, want)
	}
}

func TestHostAPIServiceCloseStopsBetweenStoresWhenContextExpires(t *testing.T) {
	recorder := newCloseRecorder()
	closeCtx := newControlledDeadlineContext(t.Context())
	server := &Server{
		executions: newExecutionRegistry(),
		workflowCloser: closeFunc(func() error {
			recorder.record("workflow")
			closeCtx.expire()
			return nil
		}),
		runCloser: recorder.closer("run", nil),
	}
	service := newHostAPIService(server, "127.0.0.1:0", io.Discard)

	if err := service.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close() error = %v, want context.DeadlineExceeded", err)
	}
	if got, want := recorder.snapshot(), []string{"workflow"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("close order after deadline = %v, want %v", got, want)
	}
	if err := service.Close(t.Context()); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}
	if err := service.Close(t.Context()); err != nil {
		t.Fatalf("idempotent Close() error = %v", err)
	}
	if got, want := recorder.snapshot(), []string{"workflow", "run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("close order after retry = %v, want %v", got, want)
	}
}

func TestHostAPIServiceConcurrentCloseCallsClosersAtMostOnce(t *testing.T) {
	recorder := newCloseRecorder()
	workflowEntered := make(chan struct{})
	releaseWorkflow := make(chan struct{})
	var enterOnce sync.Once
	server := &Server{
		executions: newExecutionRegistry(),
		workflowCloser: closeFunc(func() error {
			recorder.record("workflow")
			enterOnce.Do(func() { close(workflowEntered) })
			<-releaseWorkflow
			return nil
		}),
		runCloser: recorder.closer("run", nil),
	}
	service := newHostAPIService(server, "127.0.0.1:0", io.Discard)

	const callers = 8
	results := make(chan error, callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			<-start
			results <- service.Close(t.Context())
		}()
	}
	close(start)
	<-workflowEntered
	close(releaseWorkflow)
	for range callers {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Close() error = %v", err)
		}
	}
	if got, want := recorder.snapshot(), []string{"workflow", "run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("concurrent close order = %v, want %v", got, want)
	}
}

func TestHostAPIServicePartialStartFailureCanClose(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback address: %v", err)
	}
	defer occupied.Close()

	recorder := newCloseRecorder()
	server := newBareLifecycleServer()
	server.workflowCloser = recorder.closer("workflow", nil)
	server.runCloser = recorder.closer("run", nil)
	service := newHostAPIService(server, occupied.Addr().String(), io.Discard)
	if err := service.Start(t.Context()); err == nil {
		t.Fatal("Start() error = nil, want listen failure")
	}
	if err := service.Close(t.Context()); err != nil {
		t.Fatalf("Close() after partial Start() failure = %v", err)
	}
	if got, want := recorder.snapshot(), []string{"workflow", "run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("close order = %v, want %v", got, want)
	}
	assertChannelOpen(t, server.workerDone, "worker done")
	assertChannelOpen(t, server.janitorDone, "janitor done")
}

func TestHostAPIServiceAllOwnedGoroutinesExit(t *testing.T) {
	server := newBareLifecycleServer()
	service := newHostAPIService(server, "127.0.0.1:0", io.Discard)
	if err := service.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	executionRelease := make(chan struct{})
	executionExited := make(chan struct{})
	handle, accepted := server.executions.Begin("wf-owned", executionSyncWorkflow, nil)
	if !accepted {
		t.Fatal("execution registry rejected active execution")
	}
	snapshot := server.executions.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("Snapshot() length = %d, want 1", len(snapshot))
	}
	go func() {
		<-executionRelease
		handle.Done()
		close(executionExited)
	}()

	drainResult := make(chan error, 1)
	go func() {
		drainResult <- service.Drain(t.Context())
	}()
	<-service.intakeCtx.Done()
	close(executionRelease)
	if err := <-drainResult; err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	<-executionExited
	assertChannelClosed(t, snapshot[0].done, "registry execution done")
	assertChannelClosed(t, server.workerDone, "worker done")
	assertChannelClosed(t, server.janitorDone, "janitor done")
	if err := <-service.Done(); err != nil {
		t.Fatalf("serve Done() error = %v", err)
	}
	if err := server.executions.Wait(t.Context()); err != nil {
		t.Fatalf("registry Wait() error = %v", err)
	}
	if err := service.Close(t.Context()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

type lifecycleForceOwner struct {
	server      *Server
	service     *hostAPIService
	workflowID  string
	approval    *workflowResponse
	done        <-chan struct{}
	releaseFunc func()
	releaseOnce sync.Once
}

func (o *lifecycleForceOwner) release() {
	o.releaseOnce.Do(func() {
		if o.releaseFunc != nil {
			o.releaseFunc()
		}
	})
}

func (o *lifecycleForceOwner) registerCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		o.release()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = o.service.ForceStop(ctx)
		_ = o.service.Close(ctx)
	})
}

func startSyncWorkflowOwner(t *testing.T) *lifecycleForceOwner {
	t.Helper()
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	step := newSingleSlotStep(make(chan struct{}), 1)
	server.executor = workflowkit.NewExecutor(server.workflows, []workflowkit.Step{step})
	service := startLifecycleService(t, server)
	done := startLifecycleHTTPRequest(t, service, http.MethodPost, "/workflows", map[string]any{
		"id":    "wf-force-sync-owner",
		"input": "force-stop the synchronous owner",
	}, "")
	owner := &lifecycleForceOwner{
		server:     server,
		service:    service,
		workflowID: "wf-force-sync-owner",
		done:       done,
	}
	owner.registerCleanup(t)
	waitLifecycleSignal(t, step.started, "sync workflow step")
	return owner
}

func startQueuedWorkflowOwner(t *testing.T) *lifecycleForceOwner {
	t.Helper()
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	step := newSingleSlotStep(make(chan struct{}), 1)
	server.executor = workflowkit.NewExecutor(server.workflows, []workflowkit.Step{step})
	savePendingWorkflow(t, server.workflows, "wf-force-queued-owner", time.Now().UTC())
	service := startLifecycleService(t, server)
	owner := &lifecycleForceOwner{
		server:     server,
		service:    service,
		workflowID: "wf-force-queued-owner",
		done:       server.workerDone,
	}
	owner.registerCleanup(t)
	waitLifecycleSignal(t, step.started, "queued workflow step")
	return owner
}

func startFinalApprovalOwner(t *testing.T) *lifecycleForceOwner {
	t.Helper()
	server, err := NewServer(Config{
		RuntimeHome: t.TempDir(),
		ApprovalAuthenticator: testApprovalAuthenticator{
			identity: ApprovalIdentity{Subject: "operator-force-final"},
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	createWaitingWorkflow(t, server, "wf-force-final-owner")
	step := newSingleSlotStep(make(chan struct{}), 1)
	server.executor = workflowkit.NewExecutor(server.workflows, []workflowkit.Step{step})
	service := startLifecycleService(t, server)
	done := startLifecycleHTTPRequest(
		t,
		service,
		http.MethodPost,
		"/workflows/wf-force-final-owner/approve",
		map[string]string{"note": "force-stop final approval"},
		"Bearer test-operator",
	)
	owner := &lifecycleForceOwner{
		server:     server,
		service:    service,
		workflowID: "wf-force-final-owner",
		done:       done,
	}
	owner.registerCleanup(t)
	waitLifecycleSignal(t, step.started, "final approval step")
	return owner
}

func startAgentApprovalOwner(t *testing.T) *lifecycleForceOwner {
	t.Helper()
	server, err := NewServer(Config{
		RuntimeHome:           t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-force-agent"}},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-force-agent-owner")
	barrier := &blockingFirstLeaseCheckpointStore{
		CheckpointStore: server.agentApprovals.checkpoints,
		acquired:        make(chan struct{}),
		release:         make(chan struct{}),
	}
	server.agentApprovals.checkpoints = barrier
	service := startLifecycleService(t, server)
	pending := created.AgentApproval.Tools[0]
	done := startLifecycleHTTPRequest(
		t,
		service,
		http.MethodPost,
		"/workflows/"+created.ID+"/agent-approve",
		map[string]any{
			"resolutions": []map[string]any{{
				"index":        pending.Index,
				"tool_call_id": pending.ToolCallID,
				"tool":         pending.Tool,
				"allowed":      true,
			}},
		},
		"Bearer test-operator",
	)
	owner := &lifecycleForceOwner{
		server:      server,
		service:     service,
		workflowID:  created.ID,
		approval:    &created,
		done:        done,
		releaseFunc: func() { close(barrier.release) },
	}
	owner.registerCleanup(t)
	waitLifecycleSignal(t, barrier.acquired, "agent approval checkpoint lease")
	return owner
}

func startLifecycleService(t *testing.T, server *Server) *hostAPIService {
	t.Helper()
	service := newHostAPIService(server, "127.0.0.1:0", io.Discard)
	if err := service.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return service
}

func startLifecycleHTTPRequest(
	t *testing.T,
	service *hostAPIService,
	method string,
	path string,
	body any,
	authorization string,
) <-chan struct{} {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	request, err := http.NewRequest(
		method,
		"http://"+service.listener.Addr().String()+path,
		bytes.NewReader(payload),
	)
	if err != nil {
		t.Fatalf("build lifecycle request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	done := make(chan struct{})
	go func() {
		response, _ := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		close(done)
	}()
	return done
}

func requireLifecycleOwnerSnapshot(
	t *testing.T,
	server *Server,
	wantKind executionKind,
) executionSnapshot {
	t.Helper()
	snapshots := server.executions.Snapshot()
	if len(snapshots) != 1 {
		t.Fatalf("Snapshot() length = %d, want 1", len(snapshots))
	}
	snapshot := snapshots[0]
	if snapshot.kind != wantKind {
		t.Fatalf("snapshot kind = %q, want %q", snapshot.kind, wantKind)
	}
	if snapshot.cleanup == nil {
		t.Fatal("production owner snapshot cleanup is nil")
	}
	return snapshot
}

func assertWorkflowForceStopped(t *testing.T, owner *lifecycleForceOwner) {
	t.Helper()
	run, err := owner.server.workflows.Get(t.Context(), owner.workflowID)
	if err != nil {
		t.Fatalf("Get workflow after ForceStop(): %v", err)
	}
	if run.Status != workflowkit.StatusFailed || run.Error != hostShutdownTimeoutCode {
		t.Fatalf("workflow after ForceStop() = %+v, want failed shutdown state", run)
	}
}

func assertQueuedWorkflowForceStopped(t *testing.T, owner *lifecycleForceOwner) {
	t.Helper()
	assertWorkflowForceStopped(t, owner)
	run, err := owner.server.workflows.Get(t.Context(), owner.workflowID)
	if err != nil {
		t.Fatalf("Get queued workflow after ForceStop(): %v", err)
	}
	if run.LeaseOwner != "" || !run.LeaseUntil.IsZero() {
		t.Fatalf("queued workflow lease after ForceStop() = (%q, %s), want cleared", run.LeaseOwner, run.LeaseUntil)
	}
}

func assertAgentApprovalForceStopped(t *testing.T, owner *lifecycleForceOwner) {
	t.Helper()
	if owner.approval == nil || owner.approval.AgentApproval == nil {
		t.Fatal("agent approval owner fixture is incomplete")
	}
	checkpoints := owner.server.runs.(runkit.CheckpointStore)
	checkpoint, err := checkpoints.GetCheckpoint(
		t.Context(),
		owner.approval.AgentApproval.CheckpointID,
		localApprovalTenant,
	)
	if err != nil {
		t.Fatalf("Get checkpoint after ForceStop(): %v", err)
	}
	if checkpoint.Status != runkit.CheckpointFailed ||
		checkpoint.FailureCode != hostShutdownTimeoutCode ||
		checkpoint.LeaseOwner != "" ||
		!checkpoint.LeaseUntil.IsZero() {
		t.Fatalf("checkpoint after ForceStop() = %+v, want failed shutdown state", checkpoint)
	}
	agentRun, err := owner.server.runs.Get(t.Context(), owner.approval.AgentRunID)
	if err != nil {
		t.Fatalf("Get agent run after ForceStop(): %v", err)
	}
	if agentRun.Status != runkit.StatusFailed ||
		agentRun.Summary.Status != runkit.StatusFailed ||
		agentRun.Summary.AbortReason != hostShutdownTimeoutCode {
		t.Fatalf("agent run after ForceStop() = %+v, want failed shutdown summary", agentRun)
	}
	workflow, err := owner.server.workflows.Get(t.Context(), owner.workflowID)
	if err != nil {
		t.Fatalf("Get agent workflow after ForceStop(): %v", err)
	}
	if workflow.Status != workflowkit.StatusFailed ||
		workflow.Error != hostShutdownTimeoutCode ||
		workflow.ApprovalRef != "" ||
		workflow.WaitingReason != "" ||
		agentApprovalFromMetadata(workflow.Metadata) != nil {
		t.Fatalf("agent workflow after ForceStop() = %+v, want failed without pending approval", workflow)
	}
}

func waitLifecycleSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-t.Context().Done():
		t.Fatalf("timed out waiting for %s", name)
	}
}

func newBareLifecycleServer() *Server {
	return &Server{
		executions:              newExecutionRegistry(),
		workerWake:              make(chan struct{}, 1),
		workerDone:              make(chan struct{}),
		janitorDone:             make(chan struct{}),
		agentApprovalJanitorCfg: agentApprovalJanitorConfig{interval: time.Hour},
	}
}

type blockingArtifactStore struct {
	artifactkit.Store
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingArtifactStore) Put(ctx context.Context, artifact artifactkit.Artifact) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return s.Store.Put(ctx, artifact)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type lifecycleShortRequest struct {
	method        string
	path          string
	body          any
	authorization string
}

type lifecycleStoreBarrier struct {
	entered          chan struct{}
	contextCancelled chan struct{}
	release          chan struct{}
	returned         chan struct{}
	enterOnce        sync.Once
	cancelOnce       sync.Once
	returnOnce       sync.Once
}

func newLifecycleStoreBarrier() *lifecycleStoreBarrier {
	return &lifecycleStoreBarrier{
		entered:          make(chan struct{}),
		contextCancelled: make(chan struct{}),
		release:          make(chan struct{}),
		returned:         make(chan struct{}),
	}
}

func (b *lifecycleStoreBarrier) wait(ctx context.Context) {
	b.enterOnce.Do(func() { close(b.entered) })
	<-ctx.Done()
	b.cancelOnce.Do(func() { close(b.contextCancelled) })
	<-b.release
	b.returnOnce.Do(func() { close(b.returned) })
}

type blockingShortWorkflowStore struct {
	workflowkit.Store
	barrier     *lifecycleStoreBarrier
	blockSave   bool
	blockUpdate bool
}

func (s *blockingShortWorkflowStore) Save(ctx context.Context, run workflowkit.WorkflowRun) error {
	if s.blockSave {
		s.barrier.wait(ctx)
	}
	return s.Store.Save(ctx, run)
}

func (s *blockingShortWorkflowStore) Update(
	ctx context.Context,
	id string,
	mutate func(workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error),
) (workflowkit.WorkflowRun, error) {
	if s.blockUpdate {
		s.barrier.wait(ctx)
	}
	return s.Store.Update(ctx, id, mutate)
}

type blockingRejectCheckpointStore struct {
	runkit.CheckpointStore
	barrier *lifecycleStoreBarrier
}

func (s *blockingRejectCheckpointStore) RejectCheckpoint(ctx context.Context, request runkit.ApprovalLeaseRequest) error {
	s.barrier.wait(ctx)
	return s.CheckpointStore.RejectCheckpoint(ctx, request)
}

func setupQueuedCreateShortRequest(t *testing.T) (*Server, lifecycleShortRequest, *lifecycleStoreBarrier) {
	t.Helper()
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	barrier := newLifecycleStoreBarrier()
	server.workflows = &blockingShortWorkflowStore{
		Store:     server.workflows,
		barrier:   barrier,
		blockSave: true,
	}
	return server, lifecycleShortRequest{
		method: http.MethodPost,
		path:   "/workflows",
		body: map[string]any{
			"id":       "wf-short-create",
			"input":    "queued create",
			"run_mode": string(RunModeQueued),
		},
	}, barrier
}

func setupRequeueShortRequest(t *testing.T) (*Server, lifecycleShortRequest, *lifecycleStoreBarrier) {
	t.Helper()
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	saveLifecycleTestRun(t, server.workflows, workflowkit.WorkflowRun{
		ID:     "wf-short-requeue",
		Status: workflowkit.StatusFailed,
		Error:  "previous_failure",
	})
	barrier := newLifecycleStoreBarrier()
	server.workflows = &blockingShortWorkflowStore{
		Store:       server.workflows,
		barrier:     barrier,
		blockUpdate: true,
	}
	return server, lifecycleShortRequest{
		method: http.MethodPost,
		path:   "/workflows/wf-short-requeue/requeue",
		body:   map[string]any{},
	}, barrier
}

func setupAgentRejectShortRequest(t *testing.T) (*Server, lifecycleShortRequest, *lifecycleStoreBarrier) {
	t.Helper()
	server, err := NewServer(Config{
		RuntimeHome:           t.TempDir(),
		AgentApprovalCipher:   &testApprovalCipher{},
		ApprovalAuthenticator: testApprovalAuthenticator{identity: ApprovalIdentity{Subject: "operator-short-reject"}},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	created := createToolApprovalWorkflow(t, server, "wf-short-reject")
	pending := created.AgentApproval.Tools[0]
	barrier := newLifecycleStoreBarrier()
	server.agentApprovals.checkpoints = &blockingRejectCheckpointStore{
		CheckpointStore: server.agentApprovals.checkpoints,
		barrier:         barrier,
	}
	return server, lifecycleShortRequest{
		method:        http.MethodPost,
		path:          "/workflows/" + created.ID + "/agent-approve",
		authorization: "Bearer test-operator",
		body: map[string]any{
			"resolutions": []map[string]any{{
				"index":        pending.Index,
				"tool_call_id": pending.ToolCallID,
				"tool":         pending.Tool,
				"allowed":      false,
			}},
		},
	}, barrier
}

type signallingWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	wrote chan struct{}
	once  sync.Once
}

func newSignallingWriter() *signallingWriter {
	return &signallingWriter{wrote: make(chan struct{})}
}

func (w *signallingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.mu.Unlock()
	w.once.Do(func() { close(w.wrote) })
	return n, err
}

func (w *signallingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type closeRecorder struct {
	mu    sync.Mutex
	order []string
}

func newCloseRecorder() *closeRecorder {
	return &closeRecorder{}
}

func (r *closeRecorder) closer(name string, err error) io.Closer {
	return closeFunc(func() error {
		r.record(name)
		return err
	})
}

func (r *closeRecorder) record(name string) {
	r.mu.Lock()
	r.order = append(r.order, name)
	r.mu.Unlock()
}

func (r *closeRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

type alwaysFailLifecycleWorkflowStore struct {
	workflowkit.Store
	errs  map[string]error
	calls map[string]chan struct{}
}

func (s *alwaysFailLifecycleWorkflowStore) Update(
	_ context.Context,
	id string,
	_ func(workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error),
) (workflowkit.WorkflowRun, error) {
	s.calls[id] <- struct{}{}
	return workflowkit.WorkflowRun{}, s.errs[id]
}

type controlledDeadlineContext struct {
	context.Context
	done     chan struct{}
	observed chan struct{}
	observe  sync.Once
	expireDo sync.Once
	expired  atomic.Bool
}

func newControlledDeadlineContext(parent context.Context) *controlledDeadlineContext {
	return &controlledDeadlineContext{
		Context:  parent,
		done:     make(chan struct{}),
		observed: make(chan struct{}),
	}
}

func (c *controlledDeadlineContext) Done() <-chan struct{} {
	c.observe.Do(func() { close(c.observed) })
	return c.done
}

func (c *controlledDeadlineContext) Err() error {
	if c.expired.Load() {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func (c *controlledDeadlineContext) expire() {
	c.expireDo.Do(func() {
		c.expired.Store(true)
		close(c.done)
	})
}

func assertChannelOpen(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s channel is closed", name)
	default:
	}
}

func assertChannelClosed(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatalf("%s channel is open", name)
	}
}
