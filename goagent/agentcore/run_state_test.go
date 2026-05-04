package agentcore

import "testing"

func TestNewRunStateInitializesState(t *testing.T) {
	req := RunRequest{Input: "hello", UserID: "u1"}
	runID := NewRunID()
	state := NewRunState(runID, req)

	if state.RunID != runID {
		t.Fatalf("RunID = %s", state.RunID)
	}
	if state.Input.Input != "hello" {
		t.Fatalf("input = %q", state.Input.Input)
	}
	if state.Metadata == nil {
		t.Fatal("Metadata is nil")
	}
	if state.Messages == nil {
		t.Fatal("Messages is nil")
	}
}
