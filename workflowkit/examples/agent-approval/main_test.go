package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunExample(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), &out); err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"workflow=wf-agent status=waiting_approval",
		"approval=approval:wf-agent",
		"agent_run=",
		"workflow=wf-agent status=succeeded",
		"output=artifact:agent-final",
		"audit=audit:agent-approval-recorded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}
