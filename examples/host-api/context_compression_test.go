package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	contextagentadapter "github.com/eruca/goagents/contextkit/agentadapter"
	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/tools"
	"github.com/eruca/goagents/workflowkit"
)

func TestContextCompressionSettings(t *testing.T) {
	env := map[string]string{contextMaxCharsEnv: "48000", contextMaxToolCharsEnv: "2000", contextArchivePostgresDSNEnv: "postgres://archive"}
	settings, err := loadHostSettings(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if settings.contextMaxChars != 48000 || settings.contextMaxToolChars != 2000 || settings.contextArchivePostgresDSN != "postgres://archive" {
		t.Fatalf("settings = %+v", settings)
	}
	for _, bad := range []string{"-1", "lots", "1.5"} {
		env := map[string]string{contextMaxCharsEnv: bad}
		if _, err := loadHostSettings(func(key string) string { return env[key] }); err == nil || !strings.Contains(err.Error(), contextMaxCharsEnv) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
}

type recordingContextNext struct {
	requests []agentcore.ContextProjectionRequest
}

func (n *recordingContextNext) Project(_ context.Context, req agentcore.ContextProjectionRequest) (*agentcore.ContextProjectionResult, error) {
	n.requests = append(n.requests, req)
	return &agentcore.ContextProjectionResult{Messages: req.Messages, Metadata: req.Metadata}, nil
}

func TestContextProjectorCompressesBeforeTheMemoryProjector(t *testing.T) {
	next := &recordingContextNext{}
	off, offTools, err := newContextCompression(Config{}, &pgPools{}, next, nil)
	if err != nil || off != next || offTools != nil {
		t.Fatalf("compression off should return the next projector unchanged: %v", err)
	}
	projector, tools, err := newContextCompression(Config{ContextMaxChars: 300, ContextMaxToolChars: 50}, &pgPools{}, next, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := projector.(*contextagentadapter.Projector); !ok {
		t.Fatalf("projector = %T", projector)
	}
	if _, ok := tools.(*contextagentadapter.ContextTools); !ok {
		t.Fatalf("tools = %T", tools)
	}
	_, err = projector.Project(context.Background(), agentcore.ContextProjectionRequest{
		Messages: []agentcore.Message{
			{Role: "user", Content: strings.Repeat("earlier request ", 30)},
			{Role: "assistant", Content: "earlier answer"},
			{Role: "user", Content: "Review input artifact artifact:wf-1:input:abc"},
		},
		SystemPrompt: "system rules",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := next.requests[0]
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Content != "Review input artifact artifact:wf-1:input:abc" {
		t.Fatalf("memory projector got %+v", got.Messages)
	}
	if got.SystemPrompt != "system rules" || got.Metadata["contextkit.dropped"] != 2 {
		t.Fatalf("memory projector request = %+v", got)
	}
}

func TestHostRunsWorkflowsWithContextCompression(t *testing.T) {
	server, err := NewServer(Config{RuntimeHome: t.TempDir(), ContextMaxChars: 4000, ContextMaxToolChars: 500})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeStoreIfPossible(t, server.workflows)
		closeStoreIfPossible(t, server.runs)
	})
	runner, ok := server.agentApprovals.runner.(routingAgentRunner)
	if !ok {
		t.Fatalf("runner = %T", server.agentApprovals.runner)
	}
	if _, ok := runner.contextProjector.(*contextagentadapter.Projector); !ok {
		t.Fatalf("runner projector = %T", runner.contextProjector)
	}
	if _, ok := runner.contextTools.(*contextagentadapter.ContextTools); !ok {
		t.Fatalf("runner tools = %T", runner.contextTools)
	}
	provider := &skillEvalProvider{}
	server.providers["local-free"] = provider
	created := doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{
		"id": "wf-context", "input": "Review the change.",
	})
	if created.Status != string(workflowkit.StatusWaitingApproval) || len(provider.requests) != 1 {
		t.Fatalf("workflow = %+v after %d model calls", created, len(provider.requests))
	}
	// The local test model takes no tools, so check the provider itself.
	provided, err := runner.contextTools.Tools(context.Background(), agentcore.RunRequest{Metadata: map[string]any{"workflow_id": "wf-context"}})
	if err != nil {
		t.Fatal(err)
	}
	offered := map[string]bool{}
	for _, tool := range provided {
		offered[tool.Spec().Name] = true
	}
	if !offered[contextagentadapter.ContextReadTool] || !offered[contextagentadapter.ContextSearchTool] {
		t.Fatalf("tools provided = %v", offered)
	}
}

// TestHostContextArchivePostgres compresses on one replica and reads the
// removed content back through the context tools of another.
func TestHostContextArchivePostgres(t *testing.T) {
	dsn := hostPostgresDSN(t)
	config := Config{ContextMaxChars: 300, ContextArchivePostgresDSN: dsn}
	replica := func() (agentcore.ContextProjector, agentcore.ToolProvider) {
		pools := &pgPools{}
		t.Cleanup(func() { _ = pools.Close() })
		projector, tools, err := newContextCompression(config, pools, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return projector, tools
	}
	projectorA, _ := replica()
	_, toolsB := replica()
	workflow := map[string]any{"workflow_id": fmt.Sprintf("wf-archive-%d", time.Now().UnixNano())}
	_, err := projectorA.Project(context.Background(), agentcore.ContextProjectionRequest{
		Messages: []agentcore.Message{
			{Role: "user", Content: "The deployment key is ZEBRA-4417. " + strings.Repeat("filler ", 60)},
			{Role: "assistant", Content: "noted"},
			{Role: "user", Content: "what was the key?"},
		},
		Metadata: workflow,
	})
	if err != nil {
		t.Fatal(err)
	}
	provided, err := toolsB.Tools(context.Background(), agentcore.RunRequest{Metadata: workflow})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range provided {
		if tool.Spec().Name != contextagentadapter.ContextSearchTool {
			continue
		}
		result, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"deployment key"}`), tools.Env{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.ForLLM, "ZEBRA-4417") {
			t.Fatalf("search on the other replica = %s", result.ForLLM)
		}
		return
	}
	t.Fatal("context_search not provided")
}
