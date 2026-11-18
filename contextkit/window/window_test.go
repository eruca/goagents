package window

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/eruca/goagents/contextkit"
)

func ids(messages []contextkit.Message) string {
	out := make([]string, 0, len(messages))
	for _, msg := range messages {
		if msg.ID == "" {
			out = append(out, string(msg.Role)+":"+msg.Content)
			continue
		}
		out = append(out, msg.ID)
	}
	return strings.Join(out, ",")
}

func TestStandardCompressionKeepsNewestTurnsThatFitBudget(t *testing.T) {
	t.Parallel()

	c := New(Config{
		Budget:       contextkit.Budget{MaxChars: 60},
		Profile:      contextkit.ProfileStandard,
		MaxToolChars: 20,
	})
	got, err := c.Compress(context.Background(), contextkit.Request{
		Messages: []contextkit.Message{
			{ID: "sys", Role: contextkit.RoleSystem, Content: "be brief"},
			{ID: "u1", Role: contextkit.RoleUser, Content: strings.Repeat("old ", 10)},
			{ID: "a1", Role: contextkit.RoleAssistant, Content: strings.Repeat("answer ", 10)},
			{ID: "u2", Role: contextkit.RoleUser, Content: "second"},
			{ID: "a2", Role: contextkit.RoleAssistant, Content: "ok"},
			{ID: "u3", Role: contextkit.RoleUser, Content: "third"},
			{ID: "a3", Role: contextkit.RoleAssistant, Content: "done"},
		},
	})
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if got.Dropped != 2 || got.OverBudget {
		t.Fatalf("dropped/over budget = %d/%v, want 2/false", got.Dropped, got.OverBudget)
	}
	if want := "sys,system:" + got.Summary + ",u2,a2,u3,a3"; ids(got.Messages) != want {
		t.Fatalf("messages = %s, want %s", ids(got.Messages), want)
	}
	if summary := got.Messages[1]; summary.Role != contextkit.RoleSystem || summary.Metadata["contextkit.kind"] != "window_summary" {
		t.Fatalf("summary message = %#v, want system window summary", summary)
	}
	if !strings.Contains(got.Summary, "2 messages omitted") || !strings.Contains(got.Summary, "u1, a1") {
		t.Fatalf("summary = %q, want count and dropped ids", got.Summary)
	}
	if len(got.Collapses) != 0 || got.AutoCompact != nil {
		t.Fatalf("standard mode emitted collapse %#v / compact %#v", got.Collapses, got.AutoCompact)
	}
}

func TestCompressionNeverSeparatesToolRepliesFromTheirTurn(t *testing.T) {
	t.Parallel()

	c := New(Config{Budget: contextkit.Budget{MaxChars: 40}, Profile: contextkit.ProfileStandard, MinRecent: 2})
	got, err := c.Compress(context.Background(), contextkit.Request{
		Messages: []contextkit.Message{
			{ID: "u1", Role: contextkit.RoleUser, Content: strings.Repeat("old ", 10)},
			{ID: "u2", Role: contextkit.RoleUser, Content: "look it up"},
			{ID: "a2", Role: contextkit.RoleAssistant, Content: "calling"},
			{ID: "t2", Role: contextkit.RoleTool, ToolCallID: "call-1", Content: "result"},
			{ID: "a3", Role: contextkit.RoleAssistant, Content: "found"},
		},
	})
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	// MinRecent alone would start the window at t2; it must extend to u2.
	if want := "system:" + got.Summary + ",u2,a2,t2,a3"; ids(got.Messages) != want {
		t.Fatalf("messages = %s, want %s", ids(got.Messages), want)
	}
}

func TestCompressionWithoutBudgetKeepsEverything(t *testing.T) {
	t.Parallel()

	c := New(Config{Profile: contextkit.ProfileDeep})
	messages := []contextkit.Message{
		{ID: "u1", Role: contextkit.RoleUser, Content: strings.Repeat("old ", 100)},
		{ID: "a1", Role: contextkit.RoleAssistant, Content: "answer"},
		{ID: "u2", Role: contextkit.RoleUser, Content: "recent"},
	}
	got, err := c.Compress(context.Background(), contextkit.Request{Messages: messages})
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if got.Dropped != 0 || ids(got.Messages) != "u1,a1,u2" || len(got.Collapses) != 0 {
		t.Fatalf("result = %#v, want messages unchanged", got)
	}
}

func TestCompressionKeepsOversizedNewestTurnAndReportsIt(t *testing.T) {
	t.Parallel()

	c := New(Config{Budget: contextkit.Budget{MaxChars: 10}, Profile: contextkit.ProfileStandard})
	got, err := c.Compress(context.Background(), contextkit.Request{
		Messages: []contextkit.Message{
			{ID: "u1", Role: contextkit.RoleUser, Content: "old"},
			{ID: "u2", Role: contextkit.RoleUser, Content: strings.Repeat("big ", 10)},
			{ID: "a2", Role: contextkit.RoleAssistant, Content: "ok"},
		},
	})
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if !got.OverBudget || got.Dropped != 1 || !strings.HasSuffix(ids(got.Messages), ",u2,a2") {
		t.Fatalf("result = %#v, want newest turn kept over budget", got)
	}
}

func TestRequestBudgetOverridesConfig(t *testing.T) {
	t.Parallel()

	c := New(Config{Budget: contextkit.Budget{MaxChars: 1000}, Profile: contextkit.ProfileStandard})
	got, err := c.Compress(context.Background(), contextkit.Request{
		Budget: contextkit.Budget{MaxChars: 5},
		Messages: []contextkit.Message{
			{ID: "u1", Role: contextkit.RoleUser, Content: "first question"},
			{ID: "u2", Role: contextkit.RoleUser, Content: "next"},
		},
	})
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if got.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1 under request budget", got.Dropped)
	}
}

type fakeSummarizer struct {
	requests []contextkit.SummarizeRequest
}

func (f *fakeSummarizer) Summarize(_ context.Context, req contextkit.SummarizeRequest) (*contextkit.SummarizeResult, error) {
	f.requests = append(f.requests, req)
	return &contextkit.SummarizeResult{Summary: "facts #" + string(rune('0'+len(f.requests)))}, nil
}

func TestDeepCompressionSummarizesIncrementally(t *testing.T) {
	t.Parallel()

	summarizer := &fakeSummarizer{}
	c := New(Config{
		Budget:     contextkit.Budget{MaxChars: 40},
		Profile:    contextkit.ProfileDeep,
		Summarizer: summarizer,
	})
	first, err := c.Compress(context.Background(), contextkit.Request{
		Messages: []contextkit.Message{
			{ID: "sys", Role: contextkit.RoleSystem, Content: "system"},
			{ID: "u1", Role: contextkit.RoleUser, Content: strings.Repeat("alpha ", 5)},
			{ID: "a1", Role: contextkit.RoleAssistant, Content: strings.Repeat("beta ", 5)},
			{ID: "u2", Role: contextkit.RoleUser, Content: "current"},
		},
	})
	if err != nil {
		t.Fatalf("first Compress() error = %v", err)
	}
	if len(summarizer.requests) != 1 || summarizer.requests[0].Previous != "" || ids(summarizer.requests[0].Collapsed) != "u1,a1" {
		t.Fatalf("first summarize requests = %#v", summarizer.requests)
	}
	if first.AutoCompact == nil || first.AutoCompact.Summary != "facts #1" || len(first.Collapses) != 1 {
		t.Fatalf("first result = %#v, want auto compact and collapse", first)
	}
	if first.Messages[1].Content != "summary of earlier context:\nfacts #1" {
		t.Fatalf("first summary message = %#v", first.Messages[1])
	}

	// The caller keeps the projection, JSON-round-tripped like a stored
	// transcript, and appends the next turn.
	var stored []contextkit.Message
	encoded, _ := json.Marshal(first.Messages)
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatal(err)
	}
	stored = append(stored,
		contextkit.Message{ID: "a2", Role: contextkit.RoleAssistant, Content: strings.Repeat("gamma ", 5)},
		contextkit.Message{ID: "u3", Role: contextkit.RoleUser, Content: "newest"},
	)
	second, err := c.Compress(context.Background(), contextkit.Request{Messages: stored})
	if err != nil {
		t.Fatalf("second Compress() error = %v", err)
	}
	if len(summarizer.requests) != 2 {
		t.Fatalf("summarizer calls = %d, want 2", len(summarizer.requests))
	}
	if got := summarizer.requests[1]; got.Previous != "summary of earlier context:\nfacts #1" || ids(got.Collapsed) != "u2,a2" {
		t.Fatalf("second summarize request = %#v, want previous summary plus only new messages", got)
	}
	if want := "sys,system:summary of earlier context:\nfacts #2,u3"; ids(second.Messages) != want {
		t.Fatalf("second messages = %s, want %s", ids(second.Messages), want)
	}
	if !strings.Contains(second.Summary, "facts #2") || second.Messages[1].Metadata["contextkit.dropped_total"] != 4 {
		t.Fatalf("second summary = %#v, want dropped_total 4", second.Messages[1])
	}

	restored := contextkit.RestoreProjection(second.Messages, append(first.Collapses, second.Collapses...))
	if got := ids(restored); got != "sys,u1,a1,u2,a2,u3" {
		t.Fatalf("restored = %s, want full history", got)
	}
}

func TestDeepCompressionWithoutSummarizerStillRecordsCollapse(t *testing.T) {
	t.Parallel()

	c := New(Config{Budget: contextkit.Budget{MaxChars: 10}, Profile: contextkit.ProfileDeep})
	got, err := c.Compress(context.Background(), contextkit.Request{
		Messages: []contextkit.Message{
			{ID: "u1", Role: contextkit.RoleUser, Content: strings.Repeat("old ", 10)},
			{ID: "u2", Role: contextkit.RoleUser, Content: "new"},
		},
	})
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if got.AutoCompact != nil || len(got.Collapses) != 1 || ids(got.Collapses[0].Originals) != "u1" {
		t.Fatalf("result = %#v, want collapse without auto compact", got)
	}
	if got.Messages[0].Metadata["contextkit.collapse_id"] != got.Collapses[0].ID {
		t.Fatalf("summary metadata = %#v, want collapse id", got.Messages[0].Metadata)
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

// words prices a message as one token per word and ten per image.
func words(msg contextkit.Message) int {
	return len(strings.Fields(msg.Content)) + 10*len(msg.Images)
}

func TestEachBudgetLimitMustHold(t *testing.T) {
	t.Parallel()

	// Words: u1+a1 = 6, u2+a2 = 4, u3 = 1. Characters: u1+a1 = 26, u2+a2 = 7, u3 = 1.
	messages := []contextkit.Message{
		{ID: "u1", Role: contextkit.RoleUser, Content: "a b c d e"},
		{ID: "a1", Role: contextkit.RoleAssistant, Content: "longanswerhere"},
		{ID: "u2", Role: contextkit.RoleUser, Content: "g h"},
		{ID: "a2", Role: contextkit.RoleAssistant, Content: "i j"},
		{ID: "u3", Role: contextkit.RoleUser, Content: "k"},
	}
	cases := []struct {
		name   string
		budget contextkit.Budget
		want   string
	}{
		{"tokens bind", contextkit.Budget{MaxChars: 1000, MaxTokens: 6}, "u2,a2,u3"},
		{"characters bind", contextkit.Budget{MaxChars: 8, MaxTokens: 1000}, "u2,a2,u3"},
		{"tokens alone", contextkit.Budget{MaxTokens: 11}, "u1,a1,u2,a2,u3"},
		{"tokens alone, one short", contextkit.Budget{MaxTokens: 10}, "u2,a2,u3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New(Config{Budget: tc.budget, Profile: contextkit.ProfileStandard, CountTokens: words})
			got, err := c.Compress(context.Background(), contextkit.Request{Messages: messages})
			if err != nil {
				t.Fatalf("Compress() error = %v", err)
			}
			kept := got.Messages
			if got.Dropped > 0 {
				kept = kept[1:]
			}
			if ids(kept) != tc.want || got.OverBudget {
				t.Fatalf("kept = %s (over budget %v), want %s", ids(kept), got.OverBudget, tc.want)
			}
		})
	}
}

func TestTokenBudgetNeedsCounter(t *testing.T) {
	t.Parallel()

	messages := []contextkit.Message{{ID: "u1", Role: contextkit.RoleUser, Content: "hello"}}
	if _, err := New(Config{Budget: contextkit.Budget{MaxTokens: 10}}).Compress(context.Background(), contextkit.Request{Messages: messages}); !errors.Is(err, ErrTokenCounterRequired) {
		t.Fatalf("config token budget error = %v, want ErrTokenCounterRequired", err)
	}
	c := New(Config{Budget: contextkit.Budget{MaxChars: 100}})
	if _, err := c.Compress(context.Background(), contextkit.Request{Messages: messages, Budget: contextkit.Budget{MaxTokens: 10}}); !errors.Is(err, ErrTokenCounterRequired) {
		t.Fatalf("request token budget error = %v, want ErrTokenCounterRequired", err)
	}
}

func TestCharacterBudgetNeverCountsTokens(t *testing.T) {
	t.Parallel()

	calls := 0
	c := New(Config{
		Budget:      contextkit.Budget{MaxChars: 5},
		Profile:     contextkit.ProfileStandard,
		CountTokens: func(contextkit.Message) int { calls++; return 0 },
	})
	if _, err := c.Compress(context.Background(), contextkit.Request{Messages: []contextkit.Message{
		{ID: "u1", Role: contextkit.RoleUser, Content: "first question"},
		{ID: "u2", Role: contextkit.RoleUser, Content: "next"},
	}}); err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("CountTokens calls = %d, want 0 without a token limit", calls)
	}
}

func TestRequestBudgetReplacesConfigWhole(t *testing.T) {
	t.Parallel()

	// The request sets only a token limit, so the config's character limit,
	// which would drop u1, no longer applies.
	c := New(Config{Budget: contextkit.Budget{MaxChars: 5}, Profile: contextkit.ProfileStandard, CountTokens: words})
	got, err := c.Compress(context.Background(), contextkit.Request{
		Budget: contextkit.Budget{MaxTokens: 100},
		Messages: []contextkit.Message{
			{ID: "u1", Role: contextkit.RoleUser, Content: "first question"},
			{ID: "u2", Role: contextkit.RoleUser, Content: "next"},
		},
	})
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if got.Dropped != 0 {
		t.Fatalf("dropped = %d, want 0 under the request budget", got.Dropped)
	}
}

func TestTokenBudgetReportsOversizedNewestTurn(t *testing.T) {
	t.Parallel()

	c := New(Config{Budget: contextkit.Budget{MaxTokens: 15}, Profile: contextkit.ProfileStandard, CountTokens: words})
	got, err := c.Compress(context.Background(), contextkit.Request{Messages: []contextkit.Message{
		{ID: "u1", Role: contextkit.RoleUser, Content: "old"},
		{ID: "u2", Role: contextkit.RoleUser, Content: "look", Images: []contextkit.Image{{ID: "img-1"}, {ID: "img-2"}}},
	}})
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if !got.OverBudget || got.Dropped != 1 || !strings.HasSuffix(ids(got.Messages), ",u2") {
		t.Fatalf("result = %#v, want image turn kept over budget", got)
	}
}

func TestCountTokensSeesProjectedToolMessages(t *testing.T) {
	t.Parallel()

	var tool string
	c := New(Config{
		Budget:       contextkit.Budget{MaxTokens: 1000},
		Profile:      contextkit.ProfileStandard,
		MaxToolChars: 10,
		CountTokens: func(msg contextkit.Message) int {
			if msg.Role == contextkit.RoleTool {
				tool = msg.Content
			}
			return words(msg)
		},
	})
	if _, err := c.Compress(context.Background(), contextkit.Request{Messages: []contextkit.Message{
		{ID: "u1", Role: contextkit.RoleUser, Content: "look it up"},
		{ID: "a1", Role: contextkit.RoleAssistant, Content: "calling"},
		{ID: "t1", Role: contextkit.RoleTool, ToolName: "search", Content: strings.Repeat("raw ", 100)},
	}}); err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if !strings.HasPrefix(tool, "tool=search\n") || strings.Count(tool, "raw") > 3 {
		t.Fatalf("counted tool message = %q, want the bounded projection", tool)
	}
}

func TestDroppedImagesReachSummarizerAndRestore(t *testing.T) {
	t.Parallel()

	summarizer := &fakeSummarizer{}
	c := New(Config{
		Budget:      contextkit.Budget{MaxTokens: 15},
		Profile:     contextkit.ProfileDeep,
		CountTokens: words,
		Summarizer:  summarizer,
	})
	input := []contextkit.Message{
		{ID: "u1", Role: contextkit.RoleUser, Content: "what is this", Images: []contextkit.Image{{ID: "img-1"}, {ID: "img-2"}}},
		{ID: "a1", Role: contextkit.RoleAssistant, Content: "a rash"},
		{ID: "u2", Role: contextkit.RoleUser, Content: "and now"},
	}
	got, err := c.Compress(context.Background(), contextkit.Request{Messages: input})
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	if len(summarizer.requests) != 1 || ids(summarizer.requests[0].Collapsed) != "u1,a1" {
		t.Fatalf("summarize requests = %#v, want u1,a1 collapsed", summarizer.requests)
	}
	if images := summarizer.requests[0].Collapsed[0].Images; len(images) != 2 || images[0].ID != "img-1" || images[1].ID != "img-2" {
		t.Fatalf("collapsed images = %#v, want img-1, img-2", images)
	}

	// Copies, not aliases: a host editing what it was handed changes nothing else.
	summarizer.requests[0].Collapsed[0].Images[0].ID = "edited"
	if input[0].Images[0].ID != "img-1" || got.Collapses[0].Originals[0].Images[0].ID != "img-1" {
		t.Fatalf("images aliased: input %#v, collapse %#v", input[0].Images, got.Collapses[0].Originals[0].Images)
	}

	restored := contextkit.RestoreProjection(got.Messages, got.Collapses)
	if ids(restored) != "u1,a1,u2" || len(restored[0].Images) != 2 {
		t.Fatalf("restored = %#v, want full history with images", restored)
	}
}

func TestPlaceholderCarriesDroppedImageCount(t *testing.T) {
	t.Parallel()

	c := New(Config{Budget: contextkit.Budget{MaxTokens: 12}, Profile: contextkit.ProfileStandard, CountTokens: words})
	first, err := c.Compress(context.Background(), contextkit.Request{Messages: []contextkit.Message{
		{ID: "u1", Role: contextkit.RoleUser, Content: "look", Images: []contextkit.Image{{ID: "img-1"}, {ID: "img-2"}}},
		{ID: "u2", Role: contextkit.RoleUser, Content: "next"},
	}})
	if err != nil {
		t.Fatalf("first Compress() error = %v", err)
	}
	if !strings.Contains(first.Summary, "1 messages omitted (2 images)") {
		t.Fatalf("first summary = %q, want the image count", first.Summary)
	}

	var stored []contextkit.Message
	encoded, _ := json.Marshal(first.Messages)
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatal(err)
	}
	stored = append(stored,
		// 12 tokens: with u4 it overflows, so u2 and u3 both go.
		contextkit.Message{ID: "u3", Role: contextkit.RoleUser, Content: "look again", Images: []contextkit.Image{{ID: "img-3"}}},
		contextkit.Message{ID: "u4", Role: contextkit.RoleUser, Content: "last"},
	)
	second, err := c.Compress(context.Background(), contextkit.Request{Messages: stored})
	if err != nil {
		t.Fatalf("second Compress() error = %v", err)
	}
	if !strings.Contains(second.Summary, "3 messages omitted (3 images)") {
		t.Fatalf("second summary = %q, want counts carried across the JSON round trip", second.Summary)
	}
}
