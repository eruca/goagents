package main

import (
	"context"
	"errors"
	"time"

	"github.com/eruca/goagents/workflowkit"
)

const hostShutdownTimeoutCode = "host_shutdown_timeout"
const hostCleanupTimeout = 5 * time.Second

var errWorkflowShutdownUnchanged = errors.New("workflow shutdown state unchanged")

func (s *Server) finalizeWorkflowShutdown(ctx context.Context, workflowID string) error {
	run, err := s.workflows.Get(ctx, workflowID)
	if err != nil {
		return err
	}
	if run.Status != workflowkit.StatusPending && run.Status != workflowkit.StatusRunning {
		return nil
	}

	_, err = s.workflows.Update(ctx, workflowID, func(current workflowkit.WorkflowRun) (workflowkit.WorkflowRun, error) {
		if current.Status != workflowkit.StatusPending && current.Status != workflowkit.StatusRunning {
			return current, errWorkflowShutdownUnchanged
		}
		current.Status = workflowkit.StatusFailed
		current.Error = hostShutdownTimeoutCode
		current.LeaseOwner = ""
		current.LeaseUntil = time.Time{}
		return current, nil
	})
	if errors.Is(err, errWorkflowShutdownUnchanged) {
		return nil
	}
	return err
}

func waitAndCleanupExecutions(ctx context.Context, snapshots []executionSnapshot) error {
	for _, snapshot := range snapshots {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-snapshot.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if snapshot.cleanup != nil {
			if err := snapshot.cleanup(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
