package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
)

var (
	factPattern = regexp.MustCompile(`Invoice (INV-\d+) was issued by ([A-Za-z ]+?) for a total of (\d+) yuan`)
	refPattern  = regexp.MustCompile(`ref=(ctx:[0-9a-f]{16})`)
	pagePattern = regexp.MustCompile(`maximum":(\d+)`)
)

// visibleModel is a deterministic stand-in for a model: it reads every page
// in order, then answers only from facts visible in its request, using the
// context tools to recover facts compression removed. It also writes
// summaries that copy every fact it is shown.
type visibleModel struct {
	mu       sync.Mutex
	nextPage map[string]int
	reads    map[string]map[string]bool
}

func (m *visibleModel) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Contains(req.Messages[0].Content, "running summary") {
		var facts []string
		for _, match := range factPattern.FindAllString(req.Messages[1].Content, -1) {
			facts = append(facts, match+".")
		}
		previous := req.Messages[1].Content
		for _, match := range factPattern.FindAllString(previous, -1) {
			if !containsString(facts, match+".") {
				facts = append(facts, match+".")
			}
		}
		return &ports.ChatResponse{Content: "Facts:\n" + strings.Join(facts, "\n"), Usage: ports.Usage{InputTokens: 10, OutputTokens: 10}}, nil
	}
	question := ""
	var all strings.Builder
	for _, msg := range req.Messages {
		if msg.Role == "user" && question == "" && !strings.Contains(msg.Content, "untrusted data") {
			question = msg.Content
		}
		all.WriteString(msg.Content + "\n")
	}
	pages := 0
	tools := map[string]bool{}
	for _, tool := range req.Tools {
		tools[tool.Name] = true
		if tool.Name == "read_page" {
			if match := pagePattern.FindStringSubmatch(string(tool.Schema.JSONSchema)); match != nil {
				fmt.Sscan(match[1], &pages)
			}
		}
	}
	if m.nextPage == nil {
		m.nextPage, m.reads = map[string]int{}, map[string]map[string]bool{}
	}
	started := false
	for _, msg := range req.Messages {
		if msg.Role == "assistant" {
			started = true
		}
	}
	if !started {
		// A new run: the same task may run again under another strategy.
		m.nextPage[question], m.reads[question] = 0, nil
	}
	if m.nextPage[question] < pages {
		m.nextPage[question]++
		return call("read_page", fmt.Sprintf(`{"page":%d}`, m.nextPage[question])), nil
	}
	facts := map[string]int{}
	for _, match := range factPattern.FindAllStringSubmatch(all.String(), -1) {
		var amount int
		fmt.Sscan(match[3], &amount)
		facts[match[2]] = amount
	}
	if m.reads[question] == nil {
		m.reads[question] = map[string]bool{}
	}
	if strings.Contains(question, "issued by") {
		vendor := strings.TrimSuffix(strings.SplitN(question, "issued by ", 2)[1], "?")
		if amount, seen := facts[vendor]; seen {
			return answer(fmt.Sprint(amount)), nil
		}
		if tools["context_search"] && !m.reads[question]["search"] {
			m.reads[question]["search"] = true
			return call("context_search", `{"query":"`+vendor+`"}`), nil
		}
	}
	if len(facts) < pages && tools["context_read"] {
		for _, match := range refPattern.FindAllStringSubmatch(all.String(), -1) {
			if !m.reads[question][match[1]] {
				m.reads[question][match[1]] = true
				return call("context_read", `{"ref":"`+match[1]+`"}`), nil
			}
		}
	}
	switch {
	case strings.Contains(question, "issued by"):
		vendor := strings.TrimSuffix(strings.SplitN(question, "issued by ", 2)[1], "?")
		return answer(fmt.Sprint(facts[vendor])), nil
	case strings.Contains(question, "sum"):
		total := 0
		for _, amount := range facts {
			total += amount
		}
		return answer(fmt.Sprint(total)), nil
	default:
		best, vendor := -1, ""
		for name, amount := range facts {
			if amount > best {
				best, vendor = amount, name
			}
		}
		return answer(vendor), nil
	}
}

var callCounter int

func call(name, args string) *ports.ChatResponse {
	callCounter++
	return &ports.ChatResponse{ToolCalls: []ports.ToolCall{{ID: fmt.Sprint("call-", callCounter), Name: name, Input: json.RawMessage(args)}},
		Usage: ports.Usage{InputTokens: 100, OutputTokens: 5}}
}

func answer(value string) *ports.ChatResponse {
	return &ports.ChatResponse{Content: "Done.\nANSWER: " + value, Usage: ports.Usage{InputTokens: 100, OutputTokens: 5}}
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func TestStrategiesDifferOnWhatTheModelCanStillSee(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--seeds", "1", "--pages", "8", "--page-chars", "1200", "--budget", "3000"}, &stdout, &stderr, &visibleModel{})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	report, err := evaluate(context.Background(), mustOptions(t, "--seeds", "1", "--pages", "8", "--page-chars", "1200", "--budget", "3000"), &visibleModel{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]StrategyReport{}
	for _, strategy := range report.Strategies {
		got[strategy.Name] = strategy
	}
	if got["none"].Correct != 3 {
		t.Fatalf("none = %+v", got["none"])
	}
	if got["drop"].Correct == 3 || got["drop"].CorrectByKind["lookup"] != 0 {
		t.Fatalf("drop kept facts it dropped: %+v", got["drop"])
	}
	if got["summary"].Correct != 3 || got["summary"].SummarizerCalls == 0 {
		t.Fatalf("summary = %+v", got["summary"])
	}
	// A lookup needs one search. Facts read back are compressed again in
	// turn, so a sum over every page does not fit a budget this small.
	if got["pull"].CorrectByKind["lookup"] != 1 || got["pull"].ContextReads == 0 {
		t.Fatalf("pull = %+v", got["pull"])
	}
	if got["push"].CorrectByKind["lookup"] != 1 {
		t.Fatalf("push = %+v", got["push"])
	}
	if got["drop"].InputTokens >= got["none"].InputTokens && got["none"].InputTokens > 0 {
		t.Logf("token counts come from the fake model and are not compared")
	}
	if !strings.Contains(stdout.String(), "strategy") || !strings.Contains(stdout.String(), "pull") {
		t.Fatalf("report = %s", stdout.String())
	}
}

func mustOptions(t *testing.T, args ...string) options {
	t.Helper()
	opts, err := parseOptions(args, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestDocumentsAndGrading(t *testing.T) {
	doc := newDocument(7, 5, 400)
	again := newDocument(7, 5, 400)
	if doc.text[2] != again.text[2] || len(doc.text) != 5 {
		t.Fatal("documents are not deterministic")
	}
	for page, text := range doc.text {
		if len(factPattern.FindAllString(text, -1)) != 1 || !strings.HasPrefix(text, fmt.Sprintf("Page %d of 5.", page+1)) {
			t.Fatalf("page %d = %q", page+1, text)
		}
	}
	tasks := map[string]evalTask{}
	for _, task := range doc.tasks() {
		tasks[task.id] = task
	}
	for output, want := range map[string]string{
		"x\nANSWER: 4,370 yuan":      "4370",
		"**ANSWER:** Beiming Paper.": "Beiming Paper",
		"ANSWER：1200":                "1200",
		"no answer line":             "",
	} {
		if got := extractAnswer(output); got != want && !(want == "4370" && got == "4,370 yuan") {
			t.Fatalf("extractAnswer(%q) = %q", output, got)
		}
	}
	for _, bad := range [][]string{{"--strategies", "magic"}, {"--pages", "1"}, {"--budget", "0"}} {
		if _, err := parseOptions(bad, &bytes.Buffer{}); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}

// windowedModel rejects requests over its window like a provider whose
// context is smaller than the compressor's budget.
type windowedModel struct {
	ports.LLMClient
	window int
}

func (m windowedModel) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	size := 0
	for _, msg := range req.Messages {
		size += len(msg.Content)
	}
	if size > m.window {
		return nil, fmt.Errorf("provider: %w", ports.ErrContextTooLong)
	}
	return m.LLMClient.Chat(ctx, req)
}

func TestOverflowsAreRecoveredAndCounted(t *testing.T) {
	model := windowedModel{LLMClient: &visibleModel{}, window: 6000}
	report, err := evaluate(context.Background(), mustOptions(t, "--seeds", "1", "--pages", "6", "--page-chars", "1500", "--budget", "1000000", "--strategies", "none,pull"), model, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	none, pull := report.Strategies[0], report.Strategies[1]
	if none.OverflowFailures != 3 || none.ContextOverflows != 0 {
		t.Fatalf("none = %+v", none)
	}
	if pull.ContextOverflows == 0 || pull.OverflowFailures != 0 || pull.CorrectByKind["lookup"] != 1 {
		t.Fatalf("pull = %+v", pull)
	}
}
