package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunExample(t *testing.T) {
	var out bytes.Buffer
	dbPath := filepath.Join(t.TempDir(), "workflow.db")
	if err := run(context.Background(), dbPath, &out); err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"workflow=wf-sqlite status=waiting_approval",
		"workflow=wf-sqlite status=succeeded",
		"output=artifact:sqlite-final",
		"audit=audit:sqlite-approval-recorded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}
