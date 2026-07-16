package workflowkit

import (
	"os"
	"strings"
	"testing"
)

func TestVerifyE2EScriptCoversWorkflowkitReleaseSlice(t *testing.T) {
	path := "scripts/verify-e2e.sh"
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("%s is not executable: mode %s", path, info.Mode())
	}

	contentBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	content := string(contentBytes)
	for _, want := range []string{
		"go test ./...",
		"go test -race ./...",
		"go run ./examples/basic",
		"go run ./examples/sqlite-resume",
		"go list -m all",
		"github.com/eruca/goagents/goagent",
		"github.com/eruca/goagents/workflowkit/agentstep",
		"agentstep",
		"examples/agent-approval",
		"examples/ocr-review",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("%s does not contain %q", path, want)
		}
	}
}
