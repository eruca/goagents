package agentadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/window"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
	"github.com/eruca/goagents/goagent/tools"
)

func TestMappingKeepsToolCallsAndNamesReplies(t *testing.T) {
	input := []agentcore.Message{
		{Role: "user", Content: "find"},
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "c1", Name: "search", Input: json.RawMessage(`{"q": "x"}`)}}},
		{Role: "tool", ToolCallID: "c1", Content: "result"},
	}
	converted := ToContextKit(input)
	if converted[1].ToolCalls[0] != (contextkit.ToolCall{ID: "c1", Name: "search", Arguments: `{"q": "x"}`}) {
		t.Fatalf("tool call = %+v", converted[1].ToolCalls)
	}
	if converted[2].ToolName != "search" || converted[2].ToolCallID != "c1" {
		t.Fatalf("tool reply = %+v", converted[2])
	}
	back := FromContextKit(converted)
	if string(back[1].ToolCalls[0].Input) != `{"q": "x"}` || back[1].ToolCalls[0].Name != "search" || back[2].ToolCallID != "c1" {
		t.Fatalf("round trip = %+v", back)
	}
	if back[0].ToolCalls != nil || FromContextKit([]contextkit.Message{{Role: "assistant", ToolCalls: []contextkit.ToolCall{{Name: "n"}}}})[0].ToolCalls[0].Input != nil {
		t.Fatal("empty fields should stay empty")
	}
}

func TestNewProjectorRequiresCompressor(t *testing.T) {
	if _, err := NewProjector(Config{}); err == nil {
		t.Fatal("NewProjector accepted a nil compressor")
	}
}

// countingSummarizer records what each call is asked to fold in.
type countingSummarizer struct {
	mu        sync.Mutex
	collapsed [][]contextkit.Message
}

func (s *countingSummarizer) Summarize(_ context.Context, req contextkit.SummarizeRequest) (*contextkit.SummarizeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.collapsed = append(s.collapsed, req.Collapsed)
	return &contextkit.SummarizeResult{Summary: fmt.Sprintf("summary %d", len(s.collapsed))}, nil
}

type scriptedLLM struct {
	mu        sync.Mutex
	requests  []ports.ChatRequest
	responses []*ports.ChatResponse
}

func (l *scriptedLLM) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, req)
	response := l.responses[0]
	l.responses = l.responses[1:]
	return response, nil
}

type bigTool struct{ name string }

func (t bigTool) Spec() tools.Spec {
	return tools.Spec{Name: t.name, Permission: policy.PermissionRead}
}

func (t bigTool) Execute(context.Context, json.RawMessage, tools.Env) (*tools.Result, error) {
	return &tools.Result{ForLLM: strings.Repeat(t.name+" output ", 400)}, nil
}

// TestRealRunStaysInBudgetAndSummarizesEachMessageOnce runs a goagent agent
// whose tools return large outputs through many iterations.
func TestRealRunStaysInBudgetAndSummarizesEachMessageOnce(t *testing.T) {
	const iterations = 8
	var responses []*ports.ChatResponse
	for i := 0; i < iterations; i++ {
		// Each step asks for two tools at once; their replies must stay
		// with the assistant message that called them.
		responses = append(responses, &ports.ChatResponse{ToolCalls: []ports.ToolCall{
			{ID: fmt.Sprintf("a%d", i), Name: "ocr", Input: json.RawMessage(`{}`)},
			{ID: fmt.Sprintf("b%d", i), Name: "search", Input: json.RawMessage(`{}`)},
		}})
	}
	responses = append(responses, &ports.ChatResponse{Content: "final"})
	llm := &scriptedLLM{responses: responses}
	registry := tools.NewRegistry()
	registry.Register(bigTool{name: "ocr"})
	registry.Register(bigTool{name: "search"})

	summarizer := &countingSummarizer{}
	const budget = 3000
	var results []*contextkit.Result
	projector, err := NewProjector(Config{
		Compressor: window.New(window.Config{
			Budget: contextkit.Budget{MaxChars: budget}, Profile: contextkit.ProfileDeep,
			MaxToolChars: 500, Summarizer: summarizer,
		}),
		OnCompress: func(_ context.Context, result *contextkit.Result) { results = append(results, result) },
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(llm), agentcore.WithToolRegistry(registry),
		agentcore.WithContextProjector(projector), agentcore.WithMaxIterations(iterations+2),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), agentcore.RunRequest{Input: "read every document"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "final" || len(llm.requests) != iterations+1 {
		t.Fatalf("content %q after %d model calls", result.Content, len(llm.requests))
	}

	for index, request := range llm.requests {
		chars := 0
		calls := map[string]bool{}
		for _, msg := range request.Messages {
			chars += len([]rune(msg.Content))
			for _, call := range msg.ToolCalls {
				calls[call.ID] = true
				chars += len([]rune(call.Name)) + len(call.Input)
			}
			if msg.Role == "tool" && !calls[msg.ToolCallID] {
				t.Fatalf("request %d: tool reply %s without its call", index, msg.ToolCallID)
			}
		}
		if chars > budget {
			t.Fatalf("request %d: %d chars, budget %d", index, chars, budget)
		}
	}
	// Without the cache every model call would summarize the run's whole
	// dropped history again; with it each message is summarized once.
	seen := map[string]int{}
	total := 0
	for _, batch := range summarizer.collapsed {
		for _, msg := range batch {
			key := msg.ToolCallID + "|" + string(msg.Role) + "|" + msg.Content[:min(40, len(msg.Content))]
			for _, call := range msg.ToolCalls {
				key += call.ID
			}
			seen[key]++
			total++
		}
	}
	for key, count := range seen {
		if count > 1 {
			t.Fatalf("message %q summarized %d times", key, count)
		}
	}
	if len(summarizer.collapsed) == 0 || total == 0 {
		t.Fatal("nothing was summarized")
	}
	// The same run without the projector, for comparison.
	plainLLM := &scriptedLLM{responses: append([]*ports.ChatResponse(nil), responses...)}
	plain, err := agentcore.NewAgent(agentcore.WithLLM(plainLLM), agentcore.WithToolRegistry(registry), agentcore.WithMaxIterations(iterations+2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Run(context.Background(), agentcore.RunRequest{Input: "read every document"}); err != nil {
		t.Fatal(err)
	}
	requestChars := func(request ports.ChatRequest) int {
		chars := 0
		for _, msg := range request.Messages {
			chars += len([]rune(msg.Content))
		}
		return chars
	}
	t.Logf("last model request: %d chars uncompressed, %d chars compressed (budget %d); %d summarizer calls",
		requestChars(plainLLM.requests[len(plainLLM.requests)-1]), requestChars(llm.requests[len(llm.requests)-1]), budget, len(summarizer.collapsed))

	last := results[len(results)-1]
	if last.OverBudget || len(last.Collapses) == 0 {
		t.Fatalf("last result over budget %v, collapses %d", last.OverBudget, len(last.Collapses))
	}
}

type recordingNext struct {
	got agentcore.ContextProjectionRequest
}

func (n *recordingNext) Project(_ context.Context, req agentcore.ContextProjectionRequest) (*agentcore.ContextProjectionResult, error) {
	n.got = req
	messages := append([]agentcore.Message{{Role: "user", Content: "memory"}}, req.Messages...)
	return &agentcore.ContextProjectionResult{Messages: messages, Metadata: map[string]any{"memory.injected": true}}, nil
}

func TestNextReceivesCompressedMessagesAndMetadataMerges(t *testing.T) {
	next := &recordingNext{}
	projector, err := NewProjector(Config{
		Compressor: window.New(window.Config{Budget: contextkit.Budget{MaxChars: 50}}),
		Next:       next,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := projector.Project(context.Background(), agentcore.ContextProjectionRequest{
		Messages: []agentcore.Message{
			{Role: "user", Content: strings.Repeat("old ", 30)},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "now"},
		},
		Metadata: map[string]any{"run": "r1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.got.Messages) != 2 || next.got.Messages[0].Role != "system" || next.got.Metadata["run"] != "r1" || next.got.Metadata["contextkit.dropped"] != 2 {
		t.Fatalf("next got %+v", next.got)
	}
	if result.Messages[0].Content != "memory" || result.Metadata["memory.injected"] != true || result.Metadata["contextkit.dropped"] != 2 {
		t.Fatalf("result = %+v", result)
	}
}

func TestAgentBudgetNeedsATokenCounter(t *testing.T) {
	request := agentcore.ContextProjectionRequest{
		Messages: []agentcore.Message{{Role: "user", Content: "hi"}},
		Budget:   agentcore.Budget{MaxInputTokens: 100},
	}
	plain, _ := NewProjector(Config{Compressor: window.New(window.Config{}), UseAgentBudget: true})
	if _, err := plain.Project(context.Background(), request); err == nil {
		t.Fatal("token budget without a counter was accepted")
	}
	counted, _ := NewProjector(Config{
		Compressor:     window.New(window.Config{CountTokens: func(m contextkit.Message) int { return len(strings.Fields(m.Content)) }}),
		UseAgentBudget: true,
	})
	if _, err := counted.Project(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	ignored, _ := NewProjector(Config{Compressor: window.New(window.Config{})})
	if _, err := ignored.Project(context.Background(), request); err != nil {
		t.Fatalf("agent budget used without UseAgentBudget: %v", err)
	}
}

func TestCacheIsBoundedAndSafeForConcurrentUse(t *testing.T) {
	projector, _ := NewProjector(Config{Compressor: window.New(window.Config{Budget: contextkit.Budget{MaxChars: 100}}), CacheSize: 4})
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			var messages []agentcore.Message
			for turn := 0; turn < 10; turn++ {
				messages = append(messages,
					agentcore.Message{Role: "user", Content: fmt.Sprintf("worker %d turn %d %s", worker, turn, strings.Repeat("x", 30))},
					agentcore.Message{Role: "assistant", Content: "ok"})
				if _, err := projector.Project(context.Background(), agentcore.ContextProjectionRequest{Messages: messages}); err != nil {
					t.Error(err)
				}
			}
		}(worker)
	}
	wait.Wait()
	if projector.order.Len() > 4 || len(projector.entries) != projector.order.Len() {
		t.Fatalf("cache holds %d entries (%d indexed), limit 4", projector.order.Len(), len(projector.entries))
	}
}

func TestReusesTheLongestRememberedPrefix(t *testing.T) {
	projector, _ := NewProjector(Config{Compressor: window.New(window.Config{Budget: contextkit.Budget{MaxChars: 1000}})})
	messages := []agentcore.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}
	first, _ := projector.Project(context.Background(), agentcore.ContextProjectionRequest{Messages: messages})
	if first.Metadata["contextkit.reused_messages"] != 0 {
		t.Fatalf("first call reused %v", first.Metadata["contextkit.reused_messages"])
	}
	messages = append(messages, agentcore.Message{Role: "user", Content: "c"})
	second, _ := projector.Project(context.Background(), agentcore.ContextProjectionRequest{Messages: messages})
	if second.Metadata["contextkit.reused_messages"] != 2 || len(second.Messages) != 3 {
		t.Fatalf("second = %+v", second)
	}
	// A different budget does not reuse a compression made for another.
	other, _ := NewProjector(Config{Compressor: window.New(window.Config{CountTokens: func(contextkit.Message) int { return 1 }}), UseAgentBudget: true})
	other.Project(context.Background(), agentcore.ContextProjectionRequest{Messages: messages[:2], Budget: agentcore.Budget{MaxInputTokens: 10}})
	again, _ := other.Project(context.Background(), agentcore.ContextProjectionRequest{Messages: messages, Budget: agentcore.Budget{MaxInputTokens: 20}})
	if again.Metadata["contextkit.reused_messages"] != 0 {
		t.Fatalf("reused across budgets: %v", again.Metadata["contextkit.reused_messages"])
	}
}

func TestSystemPromptCountsAgainstTheBudgetButIsNotProjected(t *testing.T) {
	messages := []agentcore.Message{
		{Role: "user", Content: strings.Repeat("old ", 50)},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "now"},
	}
	projector, _ := NewProjector(Config{Compressor: window.New(window.Config{Budget: contextkit.Budget{MaxChars: 260}})})
	without, err := projector.Project(context.Background(), agentcore.ContextProjectionRequest{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if without.Metadata["contextkit.dropped"] != 0 {
		t.Fatalf("without a prompt everything fits: %+v", without.Metadata)
	}
	// A 200-character prompt leaves too little room for the old turn.
	next := &recordingNext{}
	withPrompt, _ := NewProjector(Config{Compressor: window.New(window.Config{Budget: contextkit.Budget{MaxChars: 260}}), Next: next})
	got, err := withPrompt.Project(context.Background(), agentcore.ContextProjectionRequest{Messages: messages, SystemPrompt: strings.Repeat("p", 200)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["contextkit.dropped"] != 2 {
		t.Fatalf("with a prompt: %+v", got.Metadata)
	}
	for _, msg := range next.got.Messages {
		if strings.Contains(msg.Content, "ppp") {
			t.Fatalf("system prompt leaked into the projection: %+v", next.got.Messages)
		}
	}
	if next.got.SystemPrompt != strings.Repeat("p", 200) {
		t.Fatal("Next did not receive the system prompt")
	}
}

func TestAgentPassesCompiledPromptToTheProjector(t *testing.T) {
	var seen string
	projector := projectorFunc(func(_ context.Context, req agentcore.ContextProjectionRequest) (*agentcore.ContextProjectionResult, error) {
		seen = req.SystemPrompt
		return &agentcore.ContextProjectionResult{Messages: req.Messages}, nil
	})
	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(&scriptedLLM{responses: []*ports.ChatResponse{{Content: "done"}}}),
		agentcore.WithContextProjector(projector),
		agentcore.WithPromptBlocks([]prompt.Block{{Name: "system", Mode: prompt.ModeCacheable, Content: "You are careful."}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), agentcore.RunRequest{Input: "hi"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "You are careful.") {
		t.Fatalf("projector saw system prompt %q", seen)
	}
}

type projectorFunc func(context.Context, agentcore.ContextProjectionRequest) (*agentcore.ContextProjectionResult, error)

func (f projectorFunc) Project(ctx context.Context, req agentcore.ContextProjectionRequest) (*agentcore.ContextProjectionResult, error) {
	return f(ctx, req)
}
