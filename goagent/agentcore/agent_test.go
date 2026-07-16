package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/prompt"
	"github.com/eruca/goagents/goagent/tools"
)

func TestNewAgentRequiresLLM(t *testing.T) {
	_, err := NewAgent(
		WithPromptCompiler(prompt.NewCompiler()),
		WithToolRegistry(tools.NewRegistry()),
	)
	if err == nil {
		t.Fatal("NewAgent returned nil error")
	}
}

func TestNewAgentWiresDefaultStages(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithPromptCompiler(prompt.NewCompiler()),
		WithToolRegistry(tools.NewRegistry()),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Content != "agent answer" {
		t.Fatalf("Content = %q", result.Content)
	}
}

func TestAgentSendsPromptBlocksOnce(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithPromptCompiler(prompt.NewCompiler()),
		WithPromptBlocks([]prompt.Block{
			{Name: "system", Mode: prompt.ModeCacheable, Content: "use tools carefully"},
		}),
		WithToolRegistry(tools.NewRegistry()),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("LLM requests = %d", len(llm.requests))
	}

	count := 0
	for _, message := range llm.requests[0].Messages {
		if message.Content == "use tools carefully" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("prompt block appeared %d times", count)
	}
}

func TestAgentRespectsMaxIterationsOption(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMaxIterations(1),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
}

func TestAgentUsesRequestRunID(t *testing.T) {
	runID := NewRunID()
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(WithLLM(llm))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{RunID: runID, Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.RunID != runID {
		t.Fatalf("RunID = %s, want %s", result.RunID, runID)
	}
}

func TestAgentGeneratesRunIDWhenMissing(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(WithLLM(llm))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.RunID.IsZero() {
		t.Fatal("RunID is zero")
	}
}

func TestAgentIncludesSkillsInPrompt(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithSkillProvider(staticSkillProvider{
			skills: []Skill{
				{
					Name:      "tool-use-guide",
					Content:   "Use lookup before answering factual questions.",
					Priority:  10,
					Cacheable: true,
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("LLM requests = %d", len(llm.requests))
	}

	messages := llm.requests[0].Messages
	if len(messages) < 2 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Role != "system" || messages[0].Content != "Skill: tool-use-guide\nUse lookup before answering factual questions." {
		t.Fatalf("first message = %#v", messages[0])
	}
	if messages[1].Content != "hello" {
		t.Fatalf("second message = %#v", messages[1])
	}
}

func TestAgentSortsSkillsInPrompt(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithSkillProvider(staticSkillProvider{
			skills: []Skill{
				{Name: "zeta", Content: "zeta skill", Priority: 20, Cacheable: true},
				{Name: "alpha", Content: "alpha skill", Priority: 10, Cacheable: true},
				{Name: "beta", Content: "beta skill", Priority: 10, Cacheable: true},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	content := llm.requests[0].Messages[0].Content
	want := "Skill: alpha\nalpha skill\nSkill: beta\nbeta skill\nSkill: zeta\nzeta skill"
	if content != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
}

func TestAgentRendersSkillNameDescriptionAndContent(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithSkillProvider(staticSkillProvider{
			skills: []Skill{
				{
					Name:        "tool-use-guide",
					Description: "Guidance for using tools.",
					Content:     "Use lookup before answering factual questions.",
					Cacheable:   true,
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	content := llm.requests[0].Messages[0].Content
	want := "Skill: tool-use-guide\nDescription: Guidance for using tools.\nUse lookup before answering factual questions."
	if content != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
}

func TestAgentReturnsSkillProviderError(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithSkillProvider(staticSkillProvider{err: fmt.Errorf("skills failed")}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err == nil || err.Error() != "skills failed" {
		t.Fatalf("err = %v", err)
	}
	if len(llm.requests) != 0 {
		t.Fatalf("LLM requests = %d", len(llm.requests))
	}
}

func TestAgentRunsWithoutSkillProvider(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(WithLLM(llm))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Content != "agent answer" {
		t.Fatalf("Content = %q", result.Content)
	}
}

func TestAgentIncludesSystemPromptProviderBlocks(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithSystemPromptProvider(staticSystemPromptProvider{
			blocks: []prompt.Block{
				{Name: "identity", Mode: prompt.ModeCacheable, Content: "You are a domain assistant."},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	messages := llm.requests[0].Messages
	if len(messages) < 2 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Role != "system" || messages[0].Content != "You are a domain assistant." {
		t.Fatalf("first message = %#v", messages[0])
	}
	if messages[1].Content != "hello" {
		t.Fatalf("second message = %#v", messages[1])
	}
}

func TestAgentCombinesStaticSystemAndSkillPromptBlocks(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithPromptBlocks([]prompt.Block{
			{Name: "static", Mode: prompt.ModeCacheable, Priority: 1, Content: "Static instruction."},
		}),
		WithSystemPromptProvider(staticSystemPromptProvider{
			blocks: []prompt.Block{{Name: "system", Mode: prompt.ModeCacheable, Priority: 2, Content: "System instruction."}},
		}),
		WithSkillProvider(staticSkillProvider{
			skills: []Skill{{Name: "skill", Content: "Skill instruction.", Priority: 3, Cacheable: true}},
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := llm.requests[0].Messages[0].Content
	want := "Static instruction.\nSystem instruction.\nSkill: skill\nSkill instruction."
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestAgentUsesToolProviderTools(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "provided", Input: json.RawMessage(`{}`)}}},
		{Content: "agent answer"},
	}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolProvider(staticToolProvider{
			tools: []tools.Tool{
				testAgentTool{
					spec: tools.Spec{Name: "provided", Permission: policy.PermissionRead},
					run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
						return &tools.Result{ForLLM: "provided observation"}, nil
					},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Content != "agent answer" {
		t.Fatalf("Content = %q", result.Content)
	}
}

func TestAgentUsesCustomToolRegistry(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "custom", Input: json.RawMessage(`{}`)}}},
		{Content: "agent answer"},
	}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(customToolRegistry{
			tools: map[string]tools.Tool{
				"custom": testAgentTool{
					spec: tools.Spec{Name: "custom", Permission: policy.PermissionRead},
					run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
						return &tools.Result{ForLLM: "custom observation"}, nil
					},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Content != "agent answer" {
		t.Fatalf("Content = %q", result.Content)
	}
	if len(llm.requests) == 0 || len(llm.requests[0].Tools) != 1 || llm.requests[0].Tools[0].Name != "custom" {
		t.Fatalf("first request tools = %#v", llm.requests[0].Tools)
	}
}

func TestAgentDoesNotPolluteBaseRegistryWithProvidedTools(t *testing.T) {
	base := tools.NewRegistry()
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(base),
		WithToolProvider(staticToolProvider{
			tools: []tools.Tool{
				testAgentTool{spec: tools.Spec{Name: "request_only", Permission: policy.PermissionRead}},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if _, ok := base.Get("request_only"); ok {
		t.Fatal("request-scoped tool polluted base registry")
	}
}

func TestAgentReturnsToolProviderError(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolProvider(staticToolProvider{err: fmt.Errorf("tools failed")}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err == nil || err.Error() != "tools failed" {
		t.Fatalf("err = %v", err)
	}
	if len(llm.requests) != 0 {
		t.Fatalf("LLM requests = %d", len(llm.requests))
	}
}

func TestAgentWithModuleWiresPromptSkillsAndTools(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "module_tool", Input: json.RawMessage(`{}`)}}},
		{Content: "agent answer"},
	}}
	module := staticModule{
		staticSystemPromptProvider: staticSystemPromptProvider{
			blocks: []prompt.Block{{Name: "identity", Mode: prompt.ModeCacheable, Priority: 1, Content: "You are a module assistant."}},
		},
		staticSkillProvider: staticSkillProvider{
			skills: []Skill{{Name: "module-skill", Content: "Use module tools carefully.", Priority: 2, Cacheable: true}},
		},
		staticToolProvider: staticToolProvider{
			tools: []tools.Tool{
				testAgentTool{
					spec: tools.Spec{Name: "module_tool", Permission: policy.PermissionRead},
					run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
						return &tools.Result{ForLLM: "module observation"}, nil
					},
				},
			},
		},
	}
	agent, err := NewAgent(
		WithLLM(llm),
		WithModule(module),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Content != "agent answer" {
		t.Fatalf("Content = %q", result.Content)
	}
	content := llm.requests[0].Messages[0].Content
	want := "You are a module assistant.\nSkill: module-skill\nUse module tools carefully."
	if content != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
}

func TestAgentEmitsFinalizedEvent(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithEventSink(sink),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !sink.hasEvent(EventFinalized, result.RunID) {
		t.Fatalf("events = %#v", sink.events)
	}
}

func TestAgentEmitsMemoryEvents(t *testing.T) {
	sink := &recordingEventSink{}
	memory := &mockMemoryProvider{loaded: []ports.MemoryMessage{{Role: "assistant", Content: "remembered"}}}
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
		WithEventSink(sink),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{SessionID: "session_1", Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !sink.hasEvent(EventMemoryLoaded, result.RunID) {
		t.Fatalf("events = %#v", sink.events)
	}
	if !sink.hasEvent(EventMemorySaved, result.RunID) {
		t.Fatalf("events = %#v", sink.events)
	}
}

func TestAgentEmitsToolEvents(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}}},
		{Content: "agent answer"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "observation"}, nil
		},
	})
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
		WithEventSink(sink),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !sink.hasEvent(EventToolStarted, result.RunID) {
		t.Fatalf("events = %#v", sink.events)
	}
	if !sink.hasEvent(EventToolCompleted, result.RunID) {
		t.Fatalf("events = %#v", sink.events)
	}
}

func TestAgentToolCompletedEventIncludesResultReference(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}}},
		{Content: "agent answer"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "observation", Ref: "artifact:lookup-1"}, nil
		},
	})
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
		WithEventSink(sink),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	event, ok := sink.findEvent(EventToolCompleted, result.RunID)
	if !ok {
		t.Fatalf("events = %#v", sink.events)
	}
	if event.Metadata["ref"] != "artifact:lookup-1" {
		t.Fatalf("metadata = %#v", event.Metadata)
	}
}

func TestAgentToolEventsExcludeRawToolInputAndResult(t *testing.T) {
	const secretInput = `{"account":"secret-account"}`
	const secretResult = "secret-account balance"

	sink := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(secretInput)}}},
		{Content: "agent answer"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: secretResult, ForUser: secretResult, Ref: "artifact:lookup-1"}, nil
		},
	})
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry), WithEventSink(sink))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	if _, err := agent.Run(context.Background(), RunRequest{Input: "hello"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	for _, event := range sink.events {
		if event.Type != EventToolStarted && event.Type != EventToolCompleted {
			continue
		}
		if event.Message == secretInput || event.Message == secretResult {
			t.Fatalf("event message leaked tool data: %#v", event)
		}
		for _, value := range event.Metadata {
			if value == secretInput || value == secretResult {
				t.Fatalf("event metadata leaked tool data: %#v", event)
			}
		}
	}
}

func TestAgentFailedEventsExcludeRawToolInputAndError(t *testing.T) {
	const secretInput = `{"account":"secret-account"}`
	const secretError = "secret-account backend failure"

	sink := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(secretInput)}},
	}}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return nil, errors.New(secretError)
		},
	})
	agent, err := NewAgent(WithLLM(llm), WithToolRegistry(registry), WithEventSink(sink))
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	if _, err := agent.Run(context.Background(), RunRequest{Input: "hello"}); err == nil {
		t.Fatal("Run returned nil error")
	}
	for _, event := range sink.events {
		if event.Type != EventToolFailed && event.Type != EventStageFailed {
			continue
		}
		if event.Message == secretError || event.Message == secretInput {
			t.Fatalf("event message leaked tool data: %#v", event)
		}
		for _, value := range event.Metadata {
			if value == secretInput || value == secretError {
				t.Fatalf("event metadata leaked tool data: %#v", event)
			}
		}
	}
}

func TestAgentEmitsToolFailedEvent(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}}},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return nil, fmt.Errorf("tool failed")
		},
	})
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
		WithEventSink(sink),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if !sink.hasEvent(EventToolFailed, sink.events[0].RunID) {
		t.Fatalf("events = %#v", sink.events)
	}
}

func TestAgentLoadsAndSavesSessionMemory(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	memory := &mockMemoryProvider{
		loaded: []ports.MemoryMessage{{Role: "assistant", Content: "remembered"}},
	}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{SessionID: "session_1", Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if memory.loadedSessionID != "session_1" {
		t.Fatalf("loaded session = %q", memory.loadedSessionID)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("LLM requests = %d", len(llm.requests))
	}
	messages := llm.requests[0].Messages
	if len(messages) < 2 || messages[0].Content != "remembered" || messages[1].Content != "hello" {
		t.Fatalf("messages = %#v", messages)
	}
	if memory.savedSessionID != "session_1" {
		t.Fatalf("saved session = %q", memory.savedSessionID)
	}
	wantSaved := []ports.MemoryMessage{
		{Role: "assistant", Content: "remembered"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "agent answer"},
	}
	if len(memory.saved) != len(wantSaved) {
		t.Fatalf("saved = %#v", memory.saved)
	}
	for i := range wantSaved {
		if memory.saved[i] != wantSaved[i] {
			t.Fatalf("saved = %#v", memory.saved)
		}
	}
}

func TestAgentDoesNotSaveAssistantToolCallMessagesToMemory(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{
			Content: "I will look it up.",
			ToolCalls: []ports.ToolCall{{
				ID:    "call_lookup_1",
				Name:  "lookup",
				Input: json.RawMessage(`{}`),
			}},
		},
		{Content: "agent answer"},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "observation"}, nil
		},
	})
	memory := &mockMemoryProvider{}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
		WithMemoryProvider(memory),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{SessionID: "session_1", Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	for _, message := range memory.saved {
		if message.Role == "assistant" && message.Content == "I will look it up." {
			t.Fatalf("saved provider transcript message: %#v", memory.saved)
		}
	}
}

func TestAgentSkipsMemoryWithoutSessionID(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	memory := &mockMemoryProvider{}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if memory.loadCalls != 0 || memory.saveCalls != 0 {
		t.Fatalf("load calls = %d, save calls = %d", memory.loadCalls, memory.saveCalls)
	}
}

func TestAgentLoadsMemoryOnceAcrossToolIterations(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}}},
		{Content: "agent answer"},
	}}
	memory := &mockMemoryProvider{loaded: []ports.MemoryMessage{{Role: "assistant", Content: "remembered"}}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "observation"}, nil
		},
	})
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
		WithToolRegistry(registry),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{SessionID: "session_1", Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if memory.loadCalls != 1 {
		t.Fatalf("load calls = %d", memory.loadCalls)
	}
}

func TestAgentDoesNotSaveMemoryOnMaxIterations(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{}}}
	memory := &mockMemoryProvider{}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
		WithMaxIterations(1),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{SessionID: "session_1", Input: "hello"})
	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
	if memory.saveCalls != 0 {
		t.Fatalf("save calls = %d", memory.saveCalls)
	}
}

func TestAgentDoesNotSaveMemoryOnPolicyDeny(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "write_file", Input: json.RawMessage(`{}`)}}},
	}}
	memory := &mockMemoryProvider{}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write_file", Permission: policy.PermissionWrite},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "wrote"}, nil
		},
	})
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
		WithToolRegistry(registry),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{SessionID: "session_1", Input: "hello"})
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
	if memory.saveCalls != 0 {
		t.Fatalf("save calls = %d", memory.saveCalls)
	}
}

func TestAgentRunDetailedReturnsExecutionSummaryForToolRun(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			return &tools.Result{ForLLM: "found it"}, nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{
			ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}},
			Usage:     ports.Usage{InputTokens: 3, OutputTokens: 2},
		},
		{
			Content: "agent answer",
			Usage:   ports.Usage{InputTokens: 5, OutputTokens: 4},
		},
	}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.RunDetailed(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("RunDetailed returned error: %v", err)
	}
	if result.ExecutionSummary.LLMCalls != 2 {
		t.Fatalf("LLMCalls = %d, want 2", result.ExecutionSummary.LLMCalls)
	}
	if result.ExecutionSummary.ToolCalls != 1 {
		t.Fatalf("ToolCalls = %d, want 1", result.ExecutionSummary.ToolCalls)
	}
	if len(result.ExecutionSummary.UsedTools) != 1 || result.ExecutionSummary.UsedTools[0] != "lookup" {
		t.Fatalf("UsedTools = %#v", result.ExecutionSummary.UsedTools)
	}
	if result.ExecutionSummary.AbortReason != "" {
		t.Fatalf("AbortReason = %q, want empty", result.ExecutionSummary.AbortReason)
	}
	if result.Usage.InputTokens != 8 || result.Usage.OutputTokens != 6 {
		t.Fatalf("usage = %#v", result.Usage)
	}
	if result.ExecutionSummary.Duration <= 0 {
		t.Fatalf("Duration = %s, want positive", result.ExecutionSummary.Duration)
	}
}

func TestAgentRunDetailedReturnsExecutionSummaryOnPolicyDeny(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{
		{ToolCalls: []ports.ToolCall{{Name: "write_file", Input: json.RawMessage(`{}`)}}},
	}}
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "write_file", Permission: policy.PermissionWrite},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			t.Fatal("tool should not run after policy denial")
			return nil, nil
		},
	})
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.RunDetailed(context.Background(), RunRequest{Input: "hello"})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
	if result == nil {
		t.Fatal("RunDetailed returned nil result")
	}
	if result.ExecutionSummary.LLMCalls != 1 {
		t.Fatalf("LLMCalls = %d, want 1", result.ExecutionSummary.LLMCalls)
	}
	if result.ExecutionSummary.ToolCalls != 0 {
		t.Fatalf("ToolCalls = %d, want 0", result.ExecutionSummary.ToolCalls)
	}
	if result.ExecutionSummary.AbortReason == "" {
		t.Fatal("AbortReason is empty")
	}
}

func TestAgentAbortsWhenBudgetExceeded(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		Content: "agent answer",
		Usage:   ports.Usage{InputTokens: 7, OutputTokens: 5},
	}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithBudget(Budget{MaxTotalTokens: 10}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
}

func TestAgentRunDetailedReturnsExecutionSummaryOnBudgetExceeded(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		Content: "agent answer",
		Usage:   ports.Usage{InputTokens: 7, OutputTokens: 5},
	}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithBudget(Budget{MaxTotalTokens: 10}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.RunDetailed(context.Background(), RunRequest{Input: "hello"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if result == nil {
		t.Fatal("RunDetailed returned nil result")
	}
	if result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %#v", result.Usage)
	}
	if result.ExecutionSummary.LLMCalls != 1 {
		t.Fatalf("LLMCalls = %d, want 1", result.ExecutionSummary.LLMCalls)
	}
	if result.ExecutionSummary.AbortReason == "" {
		t.Fatal("AbortReason is empty")
	}
}

func TestAgentBudgetDenialStopsBeforeToolExecution(t *testing.T) {
	toolRan := false
	registry := tools.NewRegistry()
	registry.Register(testAgentTool{
		spec: tools.Spec{Name: "lookup", Permission: policy.PermissionRead},
		run: func(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
			toolRan = true
			return &tools.Result{ForLLM: "result"}, nil
		},
	})
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		ToolCalls: []ports.ToolCall{{Name: "lookup", Input: json.RawMessage(`{}`)}},
		Usage:     ports.Usage{InputTokens: 6, OutputTokens: 5},
	}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithToolRegistry(registry),
		WithBudget(Budget{MaxTotalTokens: 10}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if toolRan {
		t.Fatalf("tool ran after budget denial")
	}
}

func TestAgentBudgetDenialDoesNotSaveMemory(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		Content: "agent answer",
		Usage:   ports.Usage{InputTokens: 6, OutputTokens: 5},
	}}}
	memory := &mockMemoryProvider{}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
		WithBudget(Budget{MaxTotalTokens: 10}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello", SessionID: "session_1"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if memory.saveCalls != 0 {
		t.Fatalf("save calls = %d", memory.saveCalls)
	}
}

func TestAgentCustomBudgetGuardCanAllowRun(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithBudgetGuard(allowBudgetGuard{}),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	result, err := agent.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Content != "agent answer" {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestAgentBudgetStageEmitsFailureEvent(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &mockLLM{responses: []*ports.ChatResponse{{
		Content: "agent answer",
		Usage:   ports.Usage{InputTokens: 11},
	}}}
	agent, err := NewAgent(
		WithLLM(llm),
		WithBudget(Budget{MaxInputTokens: 10}),
		WithEventSink(sink),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{Input: "hello"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if !sink.hasStageEvent(EventStageFailed, "budget") {
		t.Fatalf("missing budget failure event: %#v", sink.events)
	}
}

func TestAgentReturnsMemorySaveError(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	memory := &mockMemoryProvider{saveErr: fmt.Errorf("save failed")}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{SessionID: "session_1", Input: "hello"})
	if err == nil || err.Error() != "save failed" {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentReturnsMemoryLoadError(t *testing.T) {
	llm := &mockLLM{responses: []*ports.ChatResponse{{Content: "agent answer"}}}
	memory := &mockMemoryProvider{loadErr: fmt.Errorf("load failed")}
	agent, err := NewAgent(
		WithLLM(llm),
		WithMemoryProvider(memory),
	)
	if err != nil {
		t.Fatalf("NewAgent returned error: %v", err)
	}

	_, err = agent.Run(context.Background(), RunRequest{SessionID: "session_1", Input: "hello"})
	if err == nil || err.Error() != "load failed" {
		t.Fatalf("err = %v", err)
	}
	if len(llm.requests) != 0 {
		t.Fatalf("LLM requests = %d", len(llm.requests))
	}
}

type mockMemoryProvider struct {
	loaded          []ports.MemoryMessage
	saved           []ports.MemoryMessage
	loadErr         error
	saveErr         error
	loadCalls       int
	saveCalls       int
	loadedSessionID string
	savedSessionID  string
}

func (m *mockMemoryProvider) Load(ctx context.Context, sessionID string) ([]ports.MemoryMessage, error) {
	m.loadCalls++
	m.loadedSessionID = sessionID
	return append([]ports.MemoryMessage(nil), m.loaded...), m.loadErr
}

func (m *mockMemoryProvider) Save(ctx context.Context, sessionID string, messages []ports.MemoryMessage) error {
	m.saveCalls++
	m.savedSessionID = sessionID
	m.saved = append([]ports.MemoryMessage(nil), messages...)
	return m.saveErr
}

type staticSkillProvider struct {
	skills []Skill
	err    error
}

func (p staticSkillProvider) Skills(ctx context.Context, req RunRequest) ([]Skill, error) {
	return append([]Skill(nil), p.skills...), p.err
}

type staticSystemPromptProvider struct {
	blocks []prompt.Block
	err    error
}

func (p staticSystemPromptProvider) SystemPrompt(ctx context.Context, req RunRequest) ([]prompt.Block, error) {
	return append([]prompt.Block(nil), p.blocks...), p.err
}

type staticToolProvider struct {
	tools []tools.Tool
	err   error
}

func (p staticToolProvider) Tools(ctx context.Context, req RunRequest) ([]tools.Tool, error) {
	return append([]tools.Tool(nil), p.tools...), p.err
}

type customToolRegistry struct {
	tools map[string]tools.Tool
}

func (r customToolRegistry) Get(name string) (tools.Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

func (r customToolRegistry) MustGet(name string) (tools.Tool, error) {
	tool, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("tool %q not registered", name)
	}
	return tool, nil
}

func (r customToolRegistry) Specs() []tools.Spec {
	specs := make([]tools.Spec, 0, len(r.tools))
	for _, tool := range r.tools {
		specs = append(specs, tool.Spec())
	}
	return specs
}

type staticModule struct {
	staticSystemPromptProvider
	staticSkillProvider
	staticToolProvider
}

type recordingEventSink struct {
	events []Event
}

func (s *recordingEventSink) Emit(ctx context.Context, event Event) error {
	s.events = append(s.events, event)
	return nil
}

func (s *recordingEventSink) hasEvent(eventType EventType, runID RunID) bool {
	_, ok := s.findEvent(eventType, runID)
	return ok
}

func (s *recordingEventSink) findEvent(eventType EventType, runID RunID) (Event, bool) {
	for _, event := range s.events {
		if event.Type == eventType && event.RunID == runID {
			return event, true
		}
	}
	return Event{}, false
}

func (s *recordingEventSink) hasStageEvent(eventType EventType, stage string) bool {
	for _, event := range s.events {
		if event.Type == eventType && event.Stage == stage {
			return true
		}
	}
	return false
}
