package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/eruca/goagents/hostkit"
)

type hostAPIService struct {
	server *Server
	addr   string
	stdout io.Writer

	intakeCtx       context.Context
	cancelIntake    context.CancelFunc
	executionCtx    context.Context
	cancelExecution context.CancelFunc

	listener   net.Listener
	httpServer *http.Server
	requests   *requestActivity
	done       chan error
}

func newHostAPIService(server *Server, addr string, stdout io.Writer) *hostAPIService {
	if stdout == nil {
		stdout = io.Discard
	}
	return &hostAPIService{
		server:   server,
		addr:     addr,
		stdout:   stdout,
		requests: newRequestActivity(),
		done:     make(chan error, 1),
	}
}

func (s *hostAPIService) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return hostkit.Fail(hostkit.CodeListenFailed, "host API listen failed", err)
	}

	baseCtx := context.WithoutCancel(ctx)
	s.intakeCtx, s.cancelIntake = context.WithCancel(baseCtx)
	s.executionCtx, s.cancelExecution = context.WithCancel(baseCtx)
	s.listener = listener
	s.httpServer = &http.Server{
		Handler: s.requests.Wrap(s.server.Handler()),
		BaseContext: func(net.Listener) context.Context {
			return s.executionCtx
		},
	}

	s.server.StartQueuedWorkerWithContexts(s.intakeCtx, s.executionCtx)
	s.server.StartAgentApprovalJanitor(s.intakeCtx)
	go s.serve()

	_, _ = fmt.Fprintf(s.stdout, "host_api_addr=%s\n", listener.Addr().String())
	return nil
}

func (s *hostAPIService) serve() {
	err := s.httpServer.Serve(s.listener)
	switch {
	case errors.Is(err, http.ErrServerClosed):
		err = nil
	case err != nil:
		err = hostkit.Fail(hostkit.CodeServeFailed, "host API serve failed", err)
	}
	s.done <- err
}

func (s *hostAPIService) Done() <-chan error {
	return s.done
}

func (s *hostAPIService) Drain(ctx context.Context) error {
	s.requests.BeginDrain()
	s.server.executions.BeginDrain()
	s.cancelIntake()

	operations := []func() error{
		func() error { return s.httpServer.Shutdown(ctx) },
		func() error { return s.requests.Wait(ctx) },
		func() error { return s.server.WaitQueuedWorker(ctx) },
		func() error { return s.server.WaitAgentApprovalJanitor(ctx) },
		func() error { return s.server.executions.Wait(ctx) },
	}
	results := make(chan error, len(operations))
	for _, operation := range operations {
		go func() {
			results <- operation()
		}()
	}

	var joined error
	for range operations {
		joined = errors.Join(joined, <-results)
	}
	return joined
}

func (s *hostAPIService) ForceStop(ctx context.Context) error {
	s.requests.BeginDrain()
	s.server.executions.BeginDrain()
	snapshots := s.server.executions.Snapshot()
	s.cancelIntake()
	s.cancelExecution()

	var joined error
	if err := s.httpServer.Close(); err != nil {
		joined = errors.Join(joined, err)
	}
	if err := s.requests.Wait(ctx); err != nil {
		return errors.Join(joined, err)
	}
	if err := waitAndCleanupExecutions(ctx, snapshots); err != nil {
		joined = errors.Join(joined, err)
	}
	if err := s.server.WaitQueuedWorker(ctx); err != nil {
		joined = errors.Join(joined, err)
	}
	if err := s.server.WaitAgentApprovalJanitor(ctx); err != nil {
		joined = errors.Join(joined, err)
	}
	return joined
}

func (s *hostAPIService) Close(ctx context.Context) error {
	if s.cancelIntake != nil {
		s.cancelIntake()
	}
	if s.cancelExecution != nil {
		s.cancelExecution()
	}
	if s.requests != nil {
		s.requests.BeginDrain()
		if err := s.requests.Wait(ctx); err != nil {
			return err
		}
	}
	return s.server.Close(ctx)
}

var _ hostkit.Service = (*hostAPIService)(nil)

// requestActivity is a transport-only idle barrier. Short handlers stay out of
// the workflow execution registry but still own the stores until they return.
type requestActivity struct {
	mu        sync.Mutex
	accepting bool
	active    int
	idle      chan struct{}
}

func newRequestActivity() *requestActivity {
	idle := make(chan struct{})
	close(idle)
	return &requestActivity{
		accepting: true,
		idle:      idle,
	}
}

func (a *requestActivity) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done, accepted := a.begin()
		if !accepted {
			writeHostDraining(w)
			return
		}
		defer done()
		next.ServeHTTP(w, r)
	})
}

func (a *requestActivity) begin() (func(), bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.accepting {
		return nil, false
	}
	if a.active == 0 {
		a.idle = make(chan struct{})
	}
	a.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.active--
			if a.active == 0 {
				close(a.idle)
			}
		})
	}, true
}

func (a *requestActivity) BeginDrain() {
	a.mu.Lock()
	a.accepting = false
	a.mu.Unlock()
}

func (a *requestActivity) Wait(ctx context.Context) error {
	a.mu.Lock()
	idle := a.idle
	a.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
