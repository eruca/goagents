package window

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/contextkit"
)

func agentRun(steps int, resultChars int) []contextkit.Message {
	messages := []contextkit.Message{{ID: "u", Role: contextkit.RoleUser, Content: "do the task"}}
	for i := 0; i < steps; i++ {
		id := fmt.Sprintf("c%d", i)
		messages = append(messages,
			contextkit.Message{ID: "s" + id, Role: contextkit.RoleAssistant, Reasoning: strings.Repeat("r", 200),
				ToolCalls: []contextkit.ToolCall{{ID: id, Name: "read"}}},
			contextkit.Message{ID: "t" + id, Role: contextkit.RoleTool, ToolName: "read", ToolCallID: id, Content: strings.Repeat("x", resultChars)},
		)
	}
	return messages
}

// charCost is what the messages cost once projected, with nothing removed.
func charCost(messages []contextkit.Message) int {
	got, err := New(Config{}).Compress(context.Background(), contextkit.Request{Messages: messages})
	if err != nil {
		panic(err)
	}
	return charsOf(got.Messages)
}

func TestNothingIsAppliedWhenItFits(t *testing.T) {
	got, err := New(Config{Budget: contextkit.Budget{MaxChars: 100000}}).Compress(context.Background(), contextkit.Request{Messages: agentRun(4, 100)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Applied) != 0 || len(got.Cleared) != 0 || got.Dropped != 0 {
		t.Fatalf("applied %v cleared %d dropped %d", got.Applied, len(got.Cleared), got.Dropped)
	}
}

func TestClearingReasoningComesFirst(t *testing.T) {
	messages := agentRun(4, 100)
	full := charCost(messages)
	// Room for everything but three of the four reasonings.
	got, err := New(Config{Budget: contextkit.Budget{MaxChars: full - 500}}).Compress(context.Background(), contextkit.Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got.Applied) != "[clear_reasoning]" || got.Dropped != 0 || len(got.Cleared) != 3 {
		t.Fatalf("applied %v dropped %d cleared %d", got.Applied, got.Dropped, len(got.Cleared))
	}
	var kept []string
	for _, msg := range got.Messages {
		if msg.Reasoning != "" {
			kept = append(kept, msg.ID)
		}
	}
	if strings.Join(kept, ",") != "sc3" || got.Cleared[0].Reasoning == "" {
		t.Fatalf("reasoning kept on %v", kept)
	}
}

func TestToolResultsAreClearedOldestFirstUntilItFits(t *testing.T) {
	messages := agentRun(6, 1000)
	full := charCost(messages)
	budget := full - 1000 - 1800 // five reasonings and about two results
	// ClearTarget 1 clears only until it fits.
	got, err := New(Config{Budget: contextkit.Budget{MaxChars: budget}, ClearTarget: 1}).Compress(context.Background(), contextkit.Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got.Applied) != "[clear_reasoning clear_tool_results]" || got.Dropped != 0 || got.OverBudget {
		t.Fatalf("applied %v dropped %d over %v", got.Applied, got.Dropped, got.OverBudget)
	}
	var cleared []string
	for _, msg := range got.Messages {
		if msg.Metadata[clearedKey] == true {
			cleared = append(cleared, msg.ID)
			if msg.ToolCallID == "" || !strings.Contains(msg.Content, "tool_call_id="+msg.ToolCallID) || !strings.Contains(msg.Content, "[older result cleared: 1000 chars]") {
				t.Fatalf("cleared message = %+v", msg)
			}
		}
	}
	if strings.Join(cleared, ",") != "tc0,tc1" {
		t.Fatalf("cleared %v, want the two oldest", cleared)
	}
	// By default clearing aims for 70% of the budget, so the next calls keep
	// the same cleared results: here it clears every result it may.
	got, err = New(Config{Budget: contextkit.Budget{MaxChars: budget}}).Compress(context.Background(), contextkit.Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	batch := 0
	for _, msg := range got.Messages {
		if msg.Metadata[clearedKey] == true {
			batch++
		}
	}
	if batch != 3 || got.Dropped != 0 {
		t.Fatalf("batch clearing cleared %d, dropped %d", batch, got.Dropped)
	}
	// The three newest results are never cleared, whatever the budget.
	got, err = New(Config{Budget: contextkit.Budget{MaxChars: 500}}).Compress(context.Background(), contextkit.Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	// (The last-resort step may still shorten the newest one.)
	for _, msg := range got.Messages {
		if msg.Metadata[clearedKey] == true && (msg.ID == "tc3" || msg.ID == "tc4" || msg.ID == "tc5") {
			t.Fatalf("recent result %s cleared", msg.ID)
		}
	}
}

type recordingOffload struct{ stored map[string]string }

func (o *recordingOffload) Offload(_ context.Context, msg contextkit.Message) (string, error) {
	if o.stored == nil {
		o.stored = map[string]string{}
	}
	ref := "offload:" + msg.ToolCallID
	if msg.ToolCallID == "" || msg.Role != contextkit.RoleTool {
		ref = "offload:" + string(msg.Role) + ":" + msg.ID
	}
	o.stored[ref] = msg.Content
	return ref, nil
}

func TestOffloadKeepsFullToolResults(t *testing.T) {
	offload := &recordingOffload{}
	messages := agentRun(5, 2000)
	got, err := New(Config{Budget: contextkit.Budget{MaxChars: 3000}, MaxToolChars: 600, Offload: offload}).Compress(context.Background(), contextkit.Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	tools := 0
	for ref := range offload.stored {
		if strings.HasPrefix(ref, "offload:c") {
			tools++
		}
	}
	if got.Applied[0] != contextkit.StepToolBudget || tools != 5 {
		t.Fatalf("applied %v, offloaded %d tool results", got.Applied, tools)
	}
	for _, msg := range got.Messages {
		if msg.Role == contextkit.RoleTool && (!strings.Contains(msg.Content, "ref=offload:") || msg.Ref == "") {
			t.Fatalf("tool message without its reference: %q", msg.Content)
		}
	}
	if offload.stored["offload:c0"] != strings.Repeat("x", 2000) {
		t.Fatal("offloaded content is not the full result")
	}
}

type flakySummarizer struct {
	calls int
	err   error
}

func (s *flakySummarizer) Summarize(context.Context, contextkit.SummarizeRequest) (*contextkit.SummarizeResult, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &contextkit.SummarizeResult{Summary: "summary"}, nil
}

func TestFailingSummarizerFallsBackAndPauses(t *testing.T) {
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	summarizer := &flakySummarizer{err: errors.New("provider down")}
	c := New(Config{
		Budget: contextkit.Budget{MaxChars: 1000}, Profile: contextkit.ProfileDeep, Summarizer: summarizer,
		Now: func() time.Time { return now },
	})
	compress := func() *contextkit.Result {
		t.Helper()
		got, err := c.Compress(context.Background(), contextkit.Request{Messages: turns(6)})
		if err != nil {
			t.Fatalf("a failing summarizer failed the compression: %v", err)
		}
		return got
	}
	for i := 0; i < 3; i++ {
		if got := compress(); !got.SummaryFallback || !strings.HasPrefix(got.Summary, placeholderPrefix) {
			t.Fatalf("call %d: fallback %v summary %q", i, got.SummaryFallback, got.Summary)
		}
	}
	// Three failures pause it: no call, still a fallback.
	compress()
	if summarizer.calls != 3 {
		t.Fatalf("summarizer calls = %d, want paused at 3", summarizer.calls)
	}
	now = now.Add(6 * time.Minute)
	summarizer.err = nil
	if got := compress(); got.SummaryFallback || summarizer.calls != 4 || got.AutoCompact == nil {
		t.Fatalf("after cooldown: fallback %v calls %d", got.SummaryFallback, summarizer.calls)
	}
}

func TestCanceledSummarizerFailsTheCompression(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	summarizer := &cancelingSummarizer{cancel: cancel}
	c := New(Config{Budget: contextkit.Budget{MaxChars: 1000}, Profile: contextkit.ProfileDeep, Summarizer: summarizer})
	if _, err := c.Compress(ctx, contextkit.Request{Messages: turns(6)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

type cancelingSummarizer struct{ cancel context.CancelFunc }

func (s *cancelingSummarizer) Summarize(ctx context.Context, _ contextkit.SummarizeRequest) (*contextkit.SummarizeResult, error) {
	s.cancel()
	return nil, ctx.Err()
}

func TestNewestStepIsTruncatedAsALastResort(t *testing.T) {
	messages := []contextkit.Message{
		{ID: "u", Role: contextkit.RoleUser, Content: strings.Repeat("q", 300)},
		{ID: "a", Role: contextkit.RoleAssistant, Reasoning: strings.Repeat("r", 3000), ToolCalls: []contextkit.ToolCall{{ID: "c", Name: "read"}}},
		{ID: "t", Role: contextkit.RoleTool, ToolCallID: "c", ToolName: "read", Content: strings.Repeat("x", 20000)},
	}
	got, err := New(Config{Budget: contextkit.Budget{MaxChars: 2000}}).Compress(context.Background(), contextkit.Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if got.OverBudget || charsOf(got.Messages) > 2000 || got.Applied[len(got.Applied)-1] != contextkit.StepTruncate {
		t.Fatalf("%d chars, over %v, applied %v", charsOf(got.Messages), got.OverBudget, got.Applied)
	}
	if got.Messages[0].Content != strings.Repeat("q", 300) {
		t.Fatal("the user message was cut")
	}
	if len(got.Cleared) != 2 || len(got.Cleared[0].Content)+len(got.Cleared[0].Reasoning) < 3000 {
		t.Fatalf("originals = %d", len(got.Cleared))
	}
	// A user message that alone exceeds the budget is reported, not cut.
	got, err = New(Config{Budget: contextkit.Budget{MaxChars: 100}}).Compress(context.Background(), contextkit.Request{Messages: messages[:1]})
	if err != nil || !got.OverBudget || got.Messages[0].Content != messages[0].Content {
		t.Fatalf("oversized user message: over %v, %v", got.OverBudget, err)
	}
}

// TestCompactingBelowTheBudgetSpacesOutSummaries feeds a growing session one
// turn at a time and counts summaries: compacting to half the window leaves
// room for several turns before the next one.
func TestCompactingBelowTheBudgetSpacesOutSummaries(t *testing.T) {
	calls := func(target float64) int {
		summarizer := &flakySummarizer{}
		c := New(Config{Budget: contextkit.Budget{MaxChars: 6000}, Profile: contextkit.ProfileDeep, Summarizer: summarizer, CompactTarget: target})
		var transcript []contextkit.Message
		for i := 0; i < 30; i++ {
			transcript = append(transcript, turns(1)...)
			got, err := c.Compress(context.Background(), contextkit.Request{Messages: transcript})
			if err != nil {
				t.Fatal(err)
			}
			if got.OverBudget {
				t.Fatalf("target %v: over budget", target)
			}
			transcript = got.Messages
		}
		return summarizer.calls
	}
	full, half := calls(1), calls(0)
	if half*2 > full {
		t.Fatalf("summaries: %d filling the window, %d compacting to half", full, half)
	}
	t.Logf("summaries over 30 turns: %d filling the window, %d compacting to half", full, half)
}
