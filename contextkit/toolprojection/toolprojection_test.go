package toolprojection

import (
	"strings"
	"testing"

	"github.com/eruca/contextkit"
)

func TestProjectToolMessageUsesStructuredObservation(t *testing.T) {
	t.Parallel()

	msg := contextkit.Message{
		ID:         "obs-1",
		Role:       contextkit.RoleTool,
		Content:    "raw OCR output with important content",
		ToolName:   "ocr_document",
		ToolCallID: "call-1",
		Status:     "success",
		Ref:        "ocr:run-1",
	}

	got := Project(msg, Config{MaxResultChars: 12})

	if got.Role != contextkit.RoleTool {
		t.Fatalf("unexpected role: %s", got.Role)
	}
	for _, want := range []string{
		"tool=ocr_document",
		"tool_call_id=call-1",
		"status=success",
		"ref=ocr:run-1",
		"result=raw OCR outp",
		"[truncated",
	} {
		if !strings.Contains(got.Content, want) {
			t.Fatalf("projected content missing %q: %q", want, got.Content)
		}
	}
	if got.Metadata["contextkit.tool_projected"] != true {
		t.Fatalf("expected projection metadata: %#v", got.Metadata)
	}
}

func TestProjectNonToolMessageIsClonedUnchanged(t *testing.T) {
	t.Parallel()

	msg := contextkit.Message{Role: contextkit.RoleUser, Content: "hello"}

	got := Project(msg, Config{MaxResultChars: 3})

	if got.Content != "hello" || got.Role != contextkit.RoleUser {
		t.Fatalf("unexpected non-tool projection: %#v", got)
	}
}
