package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

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
	done       chan error
}

func newHostAPIService(server *Server, addr string, stdout io.Writer) *hostAPIService {
	if stdout == nil {
		stdout = io.Discard
	}
	return &hostAPIService{
		server: server,
		addr:   addr,
		stdout: stdout,
		done:   make(chan error, 1),
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
		Handler: s.server.Handler(),
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
	s.server.executions.BeginDrain()
	s.cancelIntake()

	operations := []func() error{
		func() error { return s.httpServer.Shutdown(ctx) },
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
	s.server.executions.BeginDrain()
	snapshots := s.server.executions.Snapshot()
	s.cancelIntake()
	s.cancelExecution()

	var joined error
	if err := s.httpServer.Close(); err != nil {
		joined = errors.Join(joined, err)
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
	return s.server.Close(ctx)
}

var _ hostkit.Service = (*hostAPIService)(nil)
