package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eruca/goagents/workflowkit"
)

type queuedWorkerConfig struct {
	leaseDuration time.Duration
}

type queuedWorkerResponse struct {
	Started                 bool   `json:"started"`
	WorkerID                string `json:"worker_id"`
	ClaimAttempts           int    `json:"claim_attempts"`
	Claimed                 int    `json:"claimed"`
	Completed               int    `json:"completed"`
	Idle                    int    `json:"idle"`
	Errors                  int    `json:"errors"`
	LeaseExtensions         int    `json:"lease_extensions"`
	HeartbeatErrors         int    `json:"heartbeat_errors"`
	LastWorkflowID          string `json:"last_workflow_id,omitempty"`
	LastError               string `json:"last_error,omitempty"`
	LastErrorWorkflowID     string `json:"last_error_workflow_id,omitempty"`
	LastHeartbeatWorkflowID string `json:"last_heartbeat_workflow_id,omitempty"`
	LastHeartbeatError      string `json:"last_heartbeat_error,omitempty"`
}

type queuedWorkerStatus struct {
	mu                      sync.RWMutex
	started                 bool
	workerID                string
	claimAttempts           int
	claimed                 int
	completed               int
	idle                    int
	errors                  int
	leaseExtensions         int
	heartbeatErrors         int
	lastWorkflowID          string
	lastError               string
	lastErrorWorkflowID     string
	lastHeartbeatWorkflowID string
	lastHeartbeatError      string
}

func (s *queuedWorkerStatus) markStarted(workerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	s.workerID = workerID
}

func (s *queuedWorkerStatus) recordClaimAttempt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimAttempts++
}

func (s *queuedWorkerStatus) recordClaimed(workflowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimed++
	s.lastWorkflowID = workflowID
}

func (s *queuedWorkerStatus) recordCompleted(workflowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed++
	s.lastWorkflowID = workflowID
}

func (s *queuedWorkerStatus) recordIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idle++
}

func (s *queuedWorkerStatus) recordError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors++
	s.lastError = err.Error()
}

func (s *queuedWorkerStatus) recordWorkflowError(workflowID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors++
	s.lastWorkflowID = workflowID
	s.lastErrorWorkflowID = workflowID
	s.lastError = err.Error()
}

func (s *queuedWorkerStatus) recordLeaseExtended(workflowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaseExtensions++
	s.lastHeartbeatWorkflowID = workflowID
	s.lastHeartbeatError = ""
}

func (s *queuedWorkerStatus) recordHeartbeatError(workflowID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeatErrors++
	s.lastHeartbeatWorkflowID = workflowID
	s.lastHeartbeatError = err.Error()
}

func (s *queuedWorkerStatus) snapshot() queuedWorkerResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return queuedWorkerResponse{
		Started:                 s.started,
		WorkerID:                s.workerID,
		ClaimAttempts:           s.claimAttempts,
		Claimed:                 s.claimed,
		Completed:               s.completed,
		Idle:                    s.idle,
		Errors:                  s.errors,
		LeaseExtensions:         s.leaseExtensions,
		HeartbeatErrors:         s.heartbeatErrors,
		LastWorkflowID:          s.lastWorkflowID,
		LastError:               s.lastError,
		LastErrorWorkflowID:     s.lastErrorWorkflowID,
		LastHeartbeatWorkflowID: s.lastHeartbeatWorkflowID,
		LastHeartbeatError:      s.lastHeartbeatError,
	}
}

func loadQueuedWorkerConfig(getenv func(string) string) (queuedWorkerConfig, error) {
	config := queuedWorkerConfig{leaseDuration: defaultQueuedLeaseDuration}
	rawLease := strings.TrimSpace(getenv(queuedLeaseDurationEnv))
	if rawLease == "" {
		return config, nil
	}
	lease, err := time.ParseDuration(rawLease)
	if err != nil {
		return queuedWorkerConfig{}, fmt.Errorf("%s must be a Go duration such as 1m or 500ms: %w", queuedLeaseDurationEnv, err)
	}
	if lease <= 0 {
		return queuedWorkerConfig{}, fmt.Errorf("%s must be greater than zero", queuedLeaseDurationEnv)
	}
	config.leaseDuration = lease
	return config, nil
}

func queuedHeartbeatInterval(lease time.Duration) time.Duration {
	interval := lease / 2
	if interval < minQueuedHeartbeatInterval {
		return minQueuedHeartbeatInterval
	}
	return interval
}

func (s *Server) queuedLeaseDuration() time.Duration {
	if s.workerCfg.leaseDuration <= 0 {
		return defaultQueuedLeaseDuration
	}
	return s.workerCfg.leaseDuration
}

func (s *Server) signalQueuedWorker() {
	select {
	case s.workerWake <- struct{}{}:
	default:
	}
}

// StartQueuedWorker starts the host-owned in-process worker loop for pending workflows.
func (s *Server) StartQueuedWorker(ctx context.Context) {
	s.StartQueuedWorkerWithContexts(ctx, ctx)
}

func (s *Server) StartQueuedWorkerWithContexts(intakeCtx context.Context, executionCtx context.Context) {
	s.workerStart.Do(func() {
		s.worker.markStarted(s.workerID)
		go s.runQueuedWorkerLoop(intakeCtx, executionCtx)
	})
}

func (s *Server) WaitQueuedWorker(ctx context.Context) error {
	select {
	case <-s.workerDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) runQueuedWorkerLoop(intakeCtx context.Context, executionCtx context.Context) {
	defer close(s.workerDone)
	for {
		if intakeCtx.Err() != nil {
			return
		}
		ran, keepRunning, _ := s.runOneQueuedWorkflow(intakeCtx, executionCtx)
		if !keepRunning {
			return
		}
		if ran {
			continue
		}
		timer := time.NewTimer(queuedPollInterval)
		select {
		case <-intakeCtx.Done():
			timer.Stop()
			return
		case <-s.workerWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

func (s *Server) runOneQueuedWorkflow(intakeCtx context.Context, executionCtx context.Context) (bool, bool, error) {
	if s.queue == nil {
		return false, true, nil
	}
	s.failExpiredQueuedWorkflows(intakeCtx)
	s.worker.recordClaimAttempt()
	claimed, err := s.queue.ClaimRunnable(intakeCtx, s.workerID, s.queuedLeaseDuration())
	if err != nil {
		if errors.Is(err, workflowkit.ErrNoRunnableWorkflow) {
			s.worker.recordIdle()
		} else {
			s.worker.recordError(err)
		}
		return false, intakeCtx.Err() == nil, err
	}
	s.worker.recordClaimed(claimed.ID)
	if intakeCtx.Err() != nil {
		s.releaseQueuedWorkflowLease(claimed.ID)
		return false, false, nil
	}
	pendingShutdown := newPendingShutdownTracker()
	handle, ok := s.executions.Begin(claimed.ID, executionQueuedWorkflow, func(ctx context.Context) error {
		return s.finalizeWorkflowShutdownTracked(ctx, claimed.ID, pendingShutdown)
	})
	if !ok {
		s.releaseQueuedWorkflowLease(claimed.ID)
		return false, false, nil
	}
	defer handle.Done()
	if intakeCtx.Err() != nil {
		s.releaseQueuedWorkflowLease(claimed.ID)
		return false, false, nil
	}

	stopHeartbeat := s.startQueuedLeaseHeartbeat(executionCtx, claimed.ID)
	defer func() {
		stopHeartbeat()
		s.releaseQueuedWorkflowLease(claimed.ID)
	}()
	operationCtx := withPendingShutdownTracker(executionCtx, pendingShutdown)
	_, err = s.executor.Run(operationCtx, claimed)
	if err != nil {
		s.worker.recordWorkflowError(claimed.ID, err)
		return true, true, err
	}
	if identity, ok := pendingShutdown.Snapshot(); ok {
		pendingShutdown.Clear(identity.CheckpointID)
	}
	s.worker.recordCompleted(claimed.ID)
	return true, true, nil
}

// failExpiredQueuedWorkflows fails queued runs whose worker stopped
// heartbeating, for example after a host crash. The interrupted step is not
// replayed because it may already have had side effects; an operator can
// requeue the failed run, which resumes after its completed steps.
func (s *Server) failExpiredQueuedWorkflows(ctx context.Context) {
	recovery, ok := s.queue.(workflowkit.QueueRecoveryStore)
	if !ok {
		return
	}
	failed, err := recovery.FailExpiredRunning(ctx, queuedLeaseExpiredCode)
	if err != nil {
		s.worker.recordError(fmt.Errorf("fail expired queued workflows: %w", err))
		return
	}
	for _, run := range failed {
		if err := s.failRunningAgentRun(ctx, run.AgentRunID, queuedLeaseExpiredCode); err != nil {
			s.worker.recordWorkflowError(run.ID, err)
		}
	}
}

func (s *Server) releaseQueuedWorkflowLease(workflowID string) {
	ctx, cancel := context.WithTimeout(context.Background(), hostCleanupTimeout)
	defer cancel()
	if _, err := s.queue.ReleaseLease(ctx, workflowID, s.workerID); err != nil {
		s.worker.recordError(fmt.Errorf("release lease for workflow %s: %w", workflowID, err))
	}
}

func (s *Server) startQueuedLeaseHeartbeat(ctx context.Context, workflowID string) func() {
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		leaseDuration := s.queuedLeaseDuration()
		ticker := time.NewTicker(queuedHeartbeatInterval(leaseDuration))
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if _, err := s.queue.ExtendLease(heartbeatCtx, workflowID, s.workerID, leaseDuration); err != nil {
					if heartbeatCtx.Err() != nil {
						return
					}
					s.worker.recordHeartbeatError(workflowID, err)
					continue
				}
				s.worker.recordLeaseExtended(workflowID)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (s *Server) handleQueuedWorker(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.worker.snapshot())
}
