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
