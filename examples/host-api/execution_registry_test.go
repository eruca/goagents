package main

import (
	"context"
	"testing"
)

func TestExecutionRegistryBeginAndWait(t *testing.T) {
	registry := newExecutionRegistry()
	handle, ok := registry.Begin("wf-1", executionSyncWorkflow, nil)
	if !ok {
		t.Fatal("Begin() rejected while registry was accepting")
	}

	waitResult := make(chan error, 1)
	go func() {
		waitResult <- registry.Wait(context.Background())
	}()

	select {
	case err := <-waitResult:
		t.Fatalf("Wait() returned before the execution completed: %v", err)
	default:
	}

	handle.Done()
	if err := <-waitResult; err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
}

func TestExecutionRegistryRejectsBeginAfterDrain(t *testing.T) {
	registry := newExecutionRegistry()
	registry.BeginDrain()

	if handle, ok := registry.Begin("wf-1", executionQueuedWorkflow, nil); ok || handle != nil {
		t.Fatalf("Begin() after BeginDrain() = (%v, %v), want (nil, false)", handle, ok)
	}
}

func TestExecutionRegistrySnapshotSurvivesConcurrentDone(t *testing.T) {
	registry := newExecutionRegistry()
	cleanup := func(context.Context) error { return nil }
	handle, ok := registry.Begin("wf-1", executionFinalApproval, cleanup)
	if !ok {
		t.Fatal("Begin() rejected while registry was accepting")
	}

	snapshots := registry.Snapshot()
	if len(snapshots) != 1 {
		t.Fatalf("Snapshot() length = %d, want 1", len(snapshots))
	}

	doneReturned := make(chan struct{})
	go func() {
		handle.Done()
		close(doneReturned)
	}()

	<-snapshots[0].done
	<-doneReturned
	if snapshots[0].workflowID != "wf-1" {
		t.Fatalf("snapshot workflowID = %q, want wf-1", snapshots[0].workflowID)
	}
	if snapshots[0].kind != executionFinalApproval {
		t.Fatalf("snapshot kind = %q, want %q", snapshots[0].kind, executionFinalApproval)
	}
	if snapshots[0].cleanup == nil {
		t.Fatal("snapshot cleanup is nil")
	}
}

func TestExecutionHandleDoneIsIdempotent(t *testing.T) {
	registry := newExecutionRegistry()
	handle, ok := registry.Begin("wf-1", executionAgentApproval, nil)
	if !ok {
		t.Fatal("Begin() rejected while registry was accepting")
	}

	firstReturned := make(chan struct{})
	secondReturned := make(chan struct{})
	go func() {
		handle.Done()
		close(firstReturned)
	}()
	go func() {
		handle.Done()
		close(secondReturned)
	}()

	<-firstReturned
	<-secondReturned
	if got := len(registry.Snapshot()); got != 0 {
		t.Fatalf("Snapshot() length after Done() = %d, want 0", got)
	}
	if err := registry.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
}

func TestExecutionRegistryBeginDrainAndBeginRace(t *testing.T) {
	for range 1000 {
		registry := newExecutionRegistry()
		start := make(chan struct{})
		beginResult := make(chan *executionHandle, 1)
		drainReturned := make(chan struct{})

		go func() {
			<-start
			handle, _ := registry.Begin("wf-race", executionSyncWorkflow, nil)
			beginResult <- handle
		}()
		go func() {
			<-start
			registry.BeginDrain()
			close(drainReturned)
		}()

		close(start)
		handle := <-beginResult
		<-drainReturned

		if lateHandle, ok := registry.Begin("wf-late", executionSyncWorkflow, nil); ok || lateHandle != nil {
			t.Fatal("Begin() accepted an execution after BeginDrain() returned")
		}
		if handle != nil {
			handle.Done()
		}
		if err := registry.Wait(context.Background()); err != nil {
			t.Fatalf("Wait() error = %v, want nil", err)
		}
	}
}

func TestExecutionRegistryNewServerInitializesRegistry(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server.executions == nil {
		t.Fatal("NewServer() executions is nil")
	}
}
