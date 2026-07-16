package window

import (
	"context"
	"strings"
	"testing"

	"github.com/eruca/goagents/contextkit"
)

func TestStandardCompressionPreservesSystemAndRecentMessages(t *testing.T) {
	t.Parallel()

	c := New(Config{
		Budget:       contextkit.Budget{MaxChars: 70},
		Profile:      contextkit.ProfileStandard,
		MinRecent:    2,
		MaxToolChars: 20,
	})
	req := contextkit.Request{
		Messages: []contextkit.Message{
			{ID: "sys", Role: contextkit.RoleSystem, Content: "system instructions"},
			{ID: "old-user", Role: contextkit.RoleUser, Content: strings.Repeat("old ", 20)},
			{ID: "old-assistant", Role: contextkit.RoleAssistant, Content: strings.Repeat("answer ", 20)},
			{ID: "recent-user", Role: contextkit.RoleUser, Content: "current question"},
			{ID: "recent-tool", Role: contextkit.RoleTool, ToolName: "ocr_document", ToolCallID: "call-1", Status: "success", Ref: "ocr:1", Content: strings.Repeat("tool-output-", 8)},
		},
	}

	got, err := c.Compress(context.Background(), req)
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}

	if got.Profile != contextkit.ProfileStandard {
		t.Fatalf("unexpected profile: %s", got.Profile)
	}
	if len(got.Messages) < 3 {
		t.Fatalf("expected system, summary, and recent messages, got=%#v", got.Messages)
	}
	if got.Messages[0].Role != contextkit.RoleSystem {
		t.Fatalf("system message should be first: %#v", got.Messages)
	}
	if got.Messages[1].Role != contextkit.RoleAssistant || !strings.Contains(got.Messages[1].Content, "compressed earlier context") {
		t.Fatalf("expected summary placeholder, got=%#v", got.Messages[1])
	}
	if got.Dropped != 2 {
		t.Fatalf("unexpected dropped count: %d", got.Dropped)
	}
	if len(got.Collapses) != 0 {
		t.Fatalf("standard mode should not emit collapse metadata: %#v", got.Collapses)
	}
	if !strings.Contains(got.Messages[len(got.Messages)-1].Content, "[truncated") {
		t.Fatalf("recent tool message should be budgeted: %#v", got.Messages[len(got.Messages)-1])
	}
	if !strings.Contains(got.Messages[len(got.Messages)-1].Content, "tool=ocr_document") {
		t.Fatalf("recent tool message should be projected: %#v", got.Messages[len(got.Messages)-1])
	}
}

func TestDeepCompressionEmitsCollapseAndAutoCompactMetadata(t *testing.T) {
	t.Parallel()

	c := New(Config{
		Budget:       contextkit.Budget{MaxChars: 50},
		Profile:      contextkit.ProfileDeep,
		MinRecent:    1,
		MaxToolChars: 15,
	})
	req := contextkit.Request{
		Messages: []contextkit.Message{
			{ID: "sys", Role: contextkit.RoleSystem, Content: "system instructions"},
			{ID: "m1", Role: contextkit.RoleUser, Content: strings.Repeat("alpha ", 10)},
			{ID: "m2", Role: contextkit.RoleAssistant, Content: strings.Repeat("beta ", 10)},
			{ID: "m3", Role: contextkit.RoleTool, Content: strings.Repeat("gamma ", 10)},
		},
	}

	got, err := c.Compress(context.Background(), req)
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}

	if got.Profile != contextkit.ProfileDeep {
		t.Fatalf("unexpected profile: %s", got.Profile)
	}
	if len(got.Collapses) != 1 {
		t.Fatalf("expected one collapse, got=%#v", got.Collapses)
	}
	if got.Collapses[0].ID == "" || len(got.Collapses[0].OriginalIDs) != 2 {
		t.Fatalf("unexpected collapse metadata: %#v", got.Collapses[0])
	}
	if len(got.Collapses[0].Originals) != 2 {
		t.Fatalf("collapse should keep reversible originals: %#v", got.Collapses[0])
	}
	if got.AutoCompact == nil {
		t.Fatalf("expected auto compact metadata")
	}
	if !strings.Contains(got.AutoCompact.Summary, "deep compact") {
		t.Fatalf("unexpected auto compact summary: %#v", got.AutoCompact)
	}
}

type fakeSummarizer struct {
	calls int
	got   contextkit.SummarizeRequest
}

func (f *fakeSummarizer) Summarize(ctx context.Context, req contextkit.SummarizeRequest) (*contextkit.SummarizeResult, error) {
	f.calls++
	f.got = req
	return &contextkit.SummarizeResult{Summary: "primary request: keep the medical policy facts"}, nil
}

func TestDeepCompressionUsesSummarizerForAutoCompactProjection(t *testing.T) {
	t.Parallel()

	summarizer := &fakeSummarizer{}
	c := New(Config{
		Budget:       contextkit.Budget{MaxChars: 80},
		Profile:      contextkit.ProfileDeep,
		MinRecent:    1,
		MaxToolChars: 20,
		Summarizer:   summarizer,
	})
	req := contextkit.Request{
		Messages: []contextkit.Message{
			{ID: "sys", Role: contextkit.RoleSystem, Content: "system instructions"},
			{ID: "m1", Role: contextkit.RoleUser, Content: "old question"},
			{ID: "m2", Role: contextkit.RoleAssistant, Content: "old answer"},
			{ID: "m3", Role: contextkit.RoleUser, Content: "current question"},
		},
	}

	got, err := c.Compress(context.Background(), req)
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}

	if summarizer.calls != 1 {
		t.Fatalf("expected summarizer call, got %d", summarizer.calls)
	}
	if len(summarizer.got.Collapsed) != 2 || len(summarizer.got.Recent) != 1 {
		t.Fatalf("unexpected summarize request: %#v", summarizer.got)
	}
	if got.AutoCompact == nil || got.AutoCompact.Summary != "primary request: keep the medical policy facts" {
		t.Fatalf("unexpected auto compact: %#v", got.AutoCompact)
	}
	if got.Messages[1].Content != "auto compacted earlier context:\nprimary request: keep the medical policy facts" {
		t.Fatalf("summarized projection not used: %#v", got.Messages)
	}
}

func TestRestoreProjectionExpandsCollapseSummaries(t *testing.T) {
	t.Parallel()

	c := New(Config{
		Profile:   contextkit.ProfileDeep,
		MinRecent: 1,
	})
	req := contextkit.Request{
		Messages: []contextkit.Message{
			{ID: "sys", Role: contextkit.RoleSystem, Content: "system"},
			{ID: "m1", Role: contextkit.RoleUser, Content: "old"},
			{ID: "m2", Role: contextkit.RoleAssistant, Content: "answer"},
			{ID: "m3", Role: contextkit.RoleUser, Content: "recent"},
		},
	}

	got, err := c.Compress(context.Background(), req)
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}

	restored := contextkit.RestoreProjection(got.Messages, got.Collapses)
	if len(restored) != 4 {
		t.Fatalf("unexpected restored len: got=%d messages=%#v", len(restored), restored)
	}
	for i, want := range []string{"sys", "m1", "m2", "m3"} {
		if restored[i].ID != want {
			t.Fatalf("unexpected restored message at %d: got=%q want=%q", i, restored[i].ID, want)
		}
	}
}

func TestConfigCanUseProfileFromEnvironment(t *testing.T) {
	t.Setenv(contextkit.EnvDeepCompression, "1")

	c := New(Config{Budget: contextkit.Budget{MaxChars: 10}})
	got, err := c.Compress(context.Background(), contextkit.Request{
		Messages: []contextkit.Message{{Role: contextkit.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if got.Profile != contextkit.ProfileDeep {
		t.Fatalf("unexpected profile: %s", got.Profile)
	}
}
