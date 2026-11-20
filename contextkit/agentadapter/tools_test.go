package agentadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/archive"
	"github.com/eruca/goagents/contextkit/window"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

var refPattern = regexp.MustCompile(`ref=(ctx:[0-9a-f]{16})`)

// recallingLLM reads documents with a tool until its context has been
// compressed, then reads the oldest archived result back by its ref.
type recallingLLM struct {
	mu       sync.Mutex
	steps    int
	readRef  string
	readBack string
	requests []ports.ChatRequest
}

func (l *recallingLLM) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, req)
	last := req.Messages[len(req.Messages)-1]
	switch {
	case l.readRef != "" && last.Role == "tool":
		l.readBack = last.Content
		return &ports.ChatResponse{Content: "done"}, nil
	case l.steps < 8:
		l.steps++
		return &ports.ChatResponse{ToolCalls: []ports.ToolCall{{ID: fmt.Sprint("c", l.steps), Name: "ocr", Input: json.RawMessage(fmt.Sprintf(`{"page":%d}`, l.steps))}}}, nil
	default:
		for _, msg := range req.Messages {
			if match := refPattern.FindStringSubmatch(msg.Content); match != nil {
				l.readRef = match[1]
				return &ports.ChatResponse{ToolCalls: []ports.ToolCall{{ID: "read", Name: ContextReadTool, Input: json.RawMessage(`{"ref":"` + l.readRef + `"}`)}}}, nil
			}
		}
		return &ports.ChatResponse{Content: "no ref found"}, nil
	}
}

type pageTool struct{}

func (pageTool) Spec() tools.Spec { return bigTool{name: "ocr"}.Spec() }

func (pageTool) Execute(_ context.Context, input json.RawMessage, _ tools.Env) (*tools.Result, error) {
	var args struct{ Page int }
	_ = json.Unmarshal(input, &args)
	return &tools.Result{ForLLM: fmt.Sprintf("PAGE %d FACT: invoice total is %d00 yuan. ", args.Page, args.Page) + strings.Repeat("filler ", 300)}, nil
}

func scopeFromWorkflow(metadata map[string]any) string {
	id, _ := metadata["workflow_id"].(string)
	return id
}

func TestModelReadsBackArchivedContentByRef(t *testing.T) {
	store := archive.New(archive.Config{})
	projector, err := NewProjector(Config{
		Compressor: window.New(window.Config{Budget: contextkit.Budget{MaxChars: 4000}, MaxToolChars: 600, Offload: store}),
		Scope:      scopeFromWorkflow,
	})
	if err != nil {
		t.Fatal(err)
	}
	contextTools, err := NewContextTools(ContextToolsConfig{Archive: store, Scope: scopeFromWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(pageTool{})
	llm := &recallingLLM{}
	agent, err := agentcore.NewAgent(agentcore.WithLLM(llm), agentcore.WithToolRegistry(registry),
		agentcore.WithContextProjector(projector), agentcore.WithToolProvider(contextTools), agentcore.WithMaxIterations(12))
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), agentcore.RunRequest{Input: "total the invoices", Metadata: map[string]any{"workflow_id": "wf-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if llm.readRef == "" {
		t.Fatalf("the model found no ref; result %q", result.Content)
	}
	// The read returns the full original page, which the compressed context
	// no longer held, untruncated and not archived again.
	if !strings.Contains(llm.readBack, "FACT: invoice total is") || !strings.Contains(llm.readBack, strings.Repeat("filler ", 300)) ||
		strings.Contains(llm.readBack, "[truncated") || strings.Contains(llm.readBack, "\nref=") {
		t.Fatalf("read back = %.300q", llm.readBack)
	}
	if result.Content != "done" {
		t.Fatalf("result = %q", result.Content)
	}
	tools := map[string]bool{}
	for _, spec := range llm.requests[0].Tools {
		tools[spec.Name] = true
	}
	if !tools[ContextReadTool] || !tools[ContextSearchTool] || !tools["ocr"] {
		t.Fatalf("tools offered = %v", tools)
	}
}

func TestContextToolsAreScopedAndChained(t *testing.T) {
	store := archive.New(archive.Config{})
	ref, _ := store.Offload(contextkit.WithArchiveScope(context.Background(), "wf-a"), contextkit.Message{Role: contextkit.RoleTool, ToolName: "ocr", Content: "secret of workflow a"})
	next := toolProviderFunc(func(context.Context, agentcore.RunRequest) ([]tools.Tool, error) {
		return []tools.Tool{pageTool{}}, nil
	})
	provider, err := NewContextTools(ContextToolsConfig{Archive: store, Scope: scopeFromWorkflow, Next: next})
	if err != nil {
		t.Fatal(err)
	}
	run := func(workflow, tool, input string) string {
		t.Helper()
		list, err := provider.Tools(context.Background(), agentcore.RunRequest{Metadata: map[string]any{"workflow_id": workflow}})
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range list {
			if candidate.Spec().Name == tool {
				result, err := candidate.Execute(context.Background(), json.RawMessage(input), tools.Env{})
				if err != nil {
					t.Fatal(err)
				}
				return result.ForLLM
			}
		}
		t.Fatalf("tool %s missing", tool)
		return ""
	}
	if got := run("wf-a", ContextReadTool, `{"ref":"`+ref+`"}`); !strings.Contains(got, "secret of workflow a") || !strings.Contains(got, "untrusted data") {
		t.Fatalf("read in scope = %q", got)
	}
	if got := run("wf-b", ContextReadTool, `{"ref":"`+ref+`"}`); strings.Contains(got, "secret") || !strings.Contains(got, "no earlier content has this ref") {
		t.Fatalf("read across scopes = %q", got)
	}
	if got := run("wf-a", ContextSearchTool, `{"query":"secret workflow"}`); !strings.Contains(got, ref) {
		t.Fatalf("search = %q", got)
	}
	if got := run("wf-b", ContextSearchTool, `{"query":"secret workflow"}`); !strings.Contains(got, "no earlier content matches") {
		t.Fatalf("search across scopes = %q", got)
	}
	unscoped, _ := provider.Tools(context.Background(), agentcore.RunRequest{})
	if len(unscoped) != 1 || unscoped[0].Spec().Name != "ocr" {
		t.Fatalf("a run without a scope got %d tools", len(unscoped))
	}
	if _, err := NewContextTools(ContextToolsConfig{Archive: store}); err == nil {
		t.Fatal("missing scope accepted")
	}
}

type toolProviderFunc func(context.Context, agentcore.RunRequest) ([]tools.Tool, error)

func (f toolProviderFunc) Tools(ctx context.Context, req agentcore.RunRequest) ([]tools.Tool, error) {
	return f(ctx, req)
}

type promptRecorder struct {
	requests []ports.ChatRequest
	reply    string
}

func (p *promptRecorder) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	p.requests = append(p.requests, req)
	return &ports.ChatResponse{Content: p.reply, Reasoning: "thinking that must not leak", Usage: ports.Usage{InputTokens: 10, OutputTokens: 4}}, nil
}

func TestLLMSummarizerFoldsMessagesIntoTheSummary(t *testing.T) {
	llm := &promptRecorder{reply: "  Request: total invoices\nFacts: INV-2 = 4300  "}
	summarizer, err := NewLLMSummarizer(LLMSummarizerConfig{LLM: llm, MaxMessageChars: 20})
	if err != nil {
		t.Fatal(err)
	}
	got, err := summarizer.Summarize(context.Background(), contextkit.SummarizeRequest{
		Previous: "Request: total invoices",
		Collapsed: []contextkit.Message{
			{Role: contextkit.RoleAssistant, ToolCalls: []contextkit.ToolCall{{Name: "ocr", Arguments: `{"page":2}`}}},
			{Role: contextkit.RoleTool, ToolName: "ocr", Ref: "ctx:abc", Content: "INV-2 total 4300 " + strings.Repeat("filler ", 20)},
		},
		Recent:        []contextkit.Message{{Role: contextkit.RoleUser, Content: "total the invoices"}},
		SummaryBudget: contextkit.Budget{MaxChars: 500},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "Request: total invoices\nFacts: INV-2 = 4300" {
		t.Fatalf("summary = %q", got.Summary)
	}
	prompt := llm.requests[0].Messages[1].Content
	for _, want := range []string{"current request (kept in context, for reference): total the invoices", "under 500 characters", "Previous summary:\nRequest: total invoices", `calls ocr {"page":2}`, "[tool ocr] INV-2 total 4300", "…[cut]", "(ref=ctx:abc)"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if llm.requests[0].Messages[0].Role != "system" || llm.requests[0].MaxOutputTokens != 4096 {
		t.Fatalf("request = %+v", llm.requests[0])
	}
	llm.reply = "  "
	if _, err := summarizer.Summarize(context.Background(), contextkit.SummarizeRequest{}); err == nil {
		t.Fatal("empty summary accepted")
	}
	if _, err := NewLLMSummarizer(LLMSummarizerConfig{}); err == nil {
		t.Fatal("missing LLM accepted")
	}
}

func TestSearchShowsWhereTheQueryMatches(t *testing.T) {
	long := strings.Repeat("filler text ", 300) + "Invoice INV-102 was issued by Beiming Paper for a total of 5900 yuan. " + strings.Repeat("more filler ", 300)
	got := matchSnippets(long, "Beiming Paper total", 600)
	if !strings.Contains(got, "Beiming Paper for a total of 5900 yuan") || len([]rune(got)) > 620 {
		t.Fatalf("snippet = %q", got)
	}
	if matchSnippets("short", "x", 100) != "short" {
		t.Fatal("short text changed")
	}
	if got := matchSnippets(long, "kubernetes", 50); got != long[:50] {
		t.Fatalf("no match = %q", got)
	}
	if got := matchSnippets(strings.Repeat("无关内容", 200)+"数据库端口是55432"+strings.Repeat("无关内容", 200), "数据库端口", 300); !strings.Contains(got, "数据库端口是55432") {
		t.Fatalf("chinese snippet = %q", got)
	}
}

func TestNotesArePinnedAndNeverCompressed(t *testing.T) {
	notes := NewNotes(NotesConfig{MaxChars: 60})
	store := archive.New(archive.Config{})
	projector, err := NewProjector(Config{
		Compressor: window.New(window.Config{Budget: contextkit.Budget{MaxChars: 400}, Offload: store}),
		Scope:      scopeFromWorkflow, Notes: notes,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, _ := NewContextTools(ContextToolsConfig{Archive: store, Scope: scopeFromWorkflow, Notes: notes})
	runTools, _ := provider.Tools(context.Background(), agentcore.RunRequest{Metadata: map[string]any{"workflow_id": "wf"}})
	var note tools.Tool
	for _, tool := range runTools {
		if tool.Spec().Name == ContextNoteTool {
			note = tool
		}
	}
	if note == nil {
		t.Fatal("context_note not offered")
	}
	messages := []agentcore.Message{{Role: "user", Content: "audit"}}
	project := func() *agentcore.ContextProjectionResult {
		t.Helper()
		got, err := projector.Project(context.Background(), agentcore.ContextProjectionRequest{Messages: messages, Metadata: map[string]any{"workflow_id": "wf"}})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := project(); strings.Contains(got.Messages[0].Content, "Your notes") {
		t.Fatal("notes shown before any were written")
	}
	if _, err := note.Execute(context.Background(), json.RawMessage(`{"note":"INV-102: Beiming Paper, 5900 yuan"}`), tools.Env{}); err != nil {
		t.Fatal(err)
	}
	// Many large steps later, the note is still at the top.
	for i := 0; i < 6; i++ {
		messages = append(messages,
			agentcore.Message{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: fmt.Sprint("c", i), Name: "ocr", Input: json.RawMessage(`{}`)}}},
			agentcore.Message{Role: "tool", ToolCallID: fmt.Sprint("c", i), Content: strings.Repeat("page text ", 30)})
	}
	got := project()
	if got.Messages[0].Role != "system" || !strings.Contains(got.Messages[0].Content, "INV-102: Beiming Paper, 5900 yuan") || got.Metadata["contextkit.dropped"] == 0 {
		t.Fatalf("projection = %+v", got.Messages[0])
	}
	// A newer note pushes out the oldest beyond MaxChars; the next
	// projection shows it, not a cached copy.
	_, _ = note.Execute(context.Background(), json.RawMessage(`{"note":"INV-103: Qinghe Foods, 7200 yuan, the largest so far"}`), tools.Env{})
	got = project()
	if strings.Contains(got.Messages[0].Content, "INV-102") || !strings.Contains(got.Messages[0].Content, "INV-103") {
		t.Fatalf("notes after eviction = %q", got.Messages[0].Content)
	}
	if notes.List("other") != nil {
		t.Fatal("notes leaked across scopes")
	}
	notes.Delete("wf")
	if len(notes.List("wf")) != 0 {
		t.Fatal("deleted notes remain")
	}
}
