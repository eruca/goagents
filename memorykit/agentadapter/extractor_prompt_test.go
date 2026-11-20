package agentadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
)

func TestExtractorPromptStatesTheFormatAndLimits(t *testing.T) {
	llm := &extractorLLM{response: &ports.ChatResponse{Content: `{"candidates":[]}`}}
	extractor, err := NewJSONCandidateExtractor(llm, adapterTestLimits(), 3)
	if err != nil {
		t.Fatal(err)
	}
	drafts, err := extractor.Extract(context.Background(), validExtractionRequest(adapterNow, "nothing durable"))
	if err != nil || len(drafts) != 0 {
		t.Fatalf("empty answer = %+v, %v", drafts, err)
	}
	system := llm.request.Messages[0].Content
	for _, want := range []string{`{"candidates":[`, "fact", "decision", "constraint", "lesson", "At most 3 candidates", "at most 80 characters", "at most 256 characters", "untrusted data"} {
		if !strings.Contains(system, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, system)
		}
	}
	user := llm.request.Messages[1].Content
	if !strings.HasSuffix(user, extractionReminder) || strings.Index(user, "</source_text_json>") > strings.Index(user, extractionReminder) {
		t.Fatalf("the reminder must follow the source: %q", user)
	}
	if llm.request.MaxOutputTokens != defaultExtractorOutputTokens {
		t.Fatalf("max output tokens = %d", llm.request.MaxOutputTokens)
	}
	extractor.MaxOutputTokens = 900
	_, _ = extractor.Extract(context.Background(), validExtractionRequest(adapterNow, "x"))
	if llm.request.MaxOutputTokens != 900 {
		t.Fatalf("max output tokens = %d", llm.request.MaxOutputTokens)
	}
}

func TestExtractorAcceptsOneCodeFenceOnly(t *testing.T) {
	body := `{"candidates":[{"kind":"fact","key":"db.port","content":"The staging database listens on 55432."}]}`
	for name, reply := range map[string]string{
		"plain":         body,
		"json fence":    "```json\n" + body + "\n```",
		"bare fence":    "```\n" + body + "\n```",
		"spaced fence":  "\n  ```json\n" + body + "\n```  \n",
		"prose":         "Here you go:\n" + body,
		"other fence":   "```yaml\n" + body + "\n```",
		"prose + fence": "Result:\n```json\n" + body + "\n```",
	} {
		t.Run(name, func(t *testing.T) {
			extractor, _ := NewJSONCandidateExtractor(&extractorLLM{response: &ports.ChatResponse{Content: reply}}, adapterTestLimits(), 3)
			drafts, err := extractor.Extract(context.Background(), validExtractionRequest(adapterNow, "source"))
			accepted := err == nil && len(drafts) == 1 && drafts[0].Key == "db.port"
			if want := !strings.Contains(name, "prose") && name != "other fence"; accepted != want {
				t.Fatalf("accepted = %v (%+v, %v), want %v", accepted, drafts, err, want)
			}
		})
	}
}
