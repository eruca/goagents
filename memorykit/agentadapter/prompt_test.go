package agentadapter

import (
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/prompt"
)

func TestGuardPromptBlockIsFixedCacheableAndContentFree(t *testing.T) {
	t.Parallel()
	block := GuardPromptBlock()
	if block.Name != "memory.untrusted_context" || block.Mode != prompt.ModeCacheable {
		t.Fatalf("guard block = %#v", block)
	}
	if strings.TrimSpace(block.Content) == "" || !strings.Contains(block.Content, "untrusted") {
		t.Fatalf("guard content = %q", block.Content)
	}
	for _, forbidden := range []string{"<memory_records>", testMemoryID1, "build.test_command", "Run go test"} {
		if strings.Contains(block.Content, forbidden) {
			t.Fatalf("guard contains dynamic memory %q: %q", forbidden, block.Content)
		}
	}
	if again := GuardPromptBlock(); again != block {
		t.Fatalf("guard block is not fixed: %#v != %#v", again, block)
	}
}
