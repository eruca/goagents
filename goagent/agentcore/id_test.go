package agentcore

import "testing"

func TestNewRunIDIsNonZero(t *testing.T) {
	runID := NewRunID()
	if runID.IsZero() {
		t.Fatal("RunID is zero")
	}
}

func TestRunIDFromStringRoundTrips(t *testing.T) {
	runID := NewRunID()
	parsed, err := RunIDFromString(runID.String())
	if err != nil {
		t.Fatalf("RunIDFromString returned error: %v", err)
	}
	if parsed != runID {
		t.Fatalf("parsed = %s, want %s", parsed, runID)
	}
}
