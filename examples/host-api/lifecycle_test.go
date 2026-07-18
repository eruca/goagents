package main

import (
	"bytes"
	"context"
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

func TestHostAPIServiceForceStopCancelsExecutionsAndRunsCleanup(t *testing.T) {
	tests := []struct {
		name string
		kind executionKind
	}{
		{name: "sync workflow", kind: executionSyncWorkflow},
		{name: "queued workflow", kind: executionQueuedWorkflow},
		{name: "final approval", kind: executionFinalApproval},
		{name: "agent approval", kind: executionAgentApproval},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newBareLifecycleServer()
			service := newHostAPIService(server, "127.0.0.1:0", io.Discard)
			if err := service.Start(t.Context()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}

			cleanupCalled := make(chan context.Context, 1)
			cleanupErr := error(nil)
			if test.kind == executionFinalApproval {
				cleanupErr = errors.New("final approval cleanup failed")
			}
			handle, accepted := server.executions.Begin("wf-force", test.kind, func(ctx context.Context) error {
				cleanupCalled <- ctx
				return cleanupErr
			})
			if !accepted {
				t.Fatal("execution registry rejected active execution")
			}
			executionStarted := make(chan struct{})
			executionExited := make(chan struct{})
			go func() {
				close(executionStarted)
				<-service.executionCtx.Done()
				handle.Done()
				close(executionExited)
			}()
			<-executionStarted

			cleanupCtx := context.WithValue(t.Context(), lifecycleContextKey{}, test.kind)
			err := service.ForceStop(cleanupCtx)
			if cleanupErr == nil && err != nil {
				t.Fatalf("ForceStop() error = %v", err)
			}
			if cleanupErr != nil && !errors.Is(err, cleanupErr) {
				t.Fatalf("ForceStop() error = %v, want cleanup sentinel", err)
			}
			if got := <-cleanupCalled; got != cleanupCtx {
				t.Fatalf("cleanup context = %p, want original %p", got, cleanupCtx)
			}
			<-executionExited
			if !errors.Is(service.intakeCtx.Err(), context.Canceled) {
				t.Fatalf("intake context error = %v, want context.Canceled", service.intakeCtx.Err())
			}
			if !errors.Is(service.executionCtx.Err(), context.Canceled) {
				t.Fatalf("execution context error = %v, want context.Canceled", service.executionCtx.Err())
			}
			if doneErr := <-service.Done(); doneErr != nil {
				t.Fatalf("Done() error after ForceStop() = %v", doneErr)
			}
			if err := service.Close(t.Context()); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		})
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
		r.mu.Lock()
		r.order = append(r.order, name)
		r.mu.Unlock()
		return err
	})
}

func (r *closeRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

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

type lifecycleContextKey struct{}

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
