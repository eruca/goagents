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
		"workflow=wf-ocr status=waiting_approval",
		"ocr=artifact:ocr-result",
		"context=artifact:context-projection",
		"approval=approval:wf-ocr",
		"agent_run=",
		"workflow=wf-ocr status=succeeded",
		"output=artifact:ocr-review-final",
		"audit=audit:ocr-review-approved",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}
