package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

type mockLLM struct {
	responses []*ports.ChatResponse
}

func (m *mockLLM) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}

type artifact struct {
	Kind     string
	Content  string
	Metadata map[string]any
}

type artifactStore struct {
	mu     sync.Mutex
	next   int
	values map[string]artifact
}

func newArtifactStore() *artifactStore {
	return &artifactStore{values: make(map[string]artifact)}
}

func (s *artifactStore) put(kind string, content string, metadata map[string]any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	ref := fmt.Sprintf("artifact:%s-%d", kind, s.next)
	s.values[ref] = artifact{Kind: kind, Content: content, Metadata: metadata}
	return ref
}

func (s *artifactStore) get(ref string) (artifact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[ref]
	return value, ok
}

type queryAccountsTool struct {
	store *artifactStore
}

func (t queryAccountsTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "query_accounts",
		Description: "Stores a deterministic account query result and returns a compact artifact reference.",
		Permission:  policy.PermissionRead,
		Timeout:     100 * time.Millisecond,
		Schema: tools.Schema{
			JSONSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}},"required":["status"],"additionalProperties":false}`),
		},
	}
}

func (t queryAccountsTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	var req struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}
	content := "id,status\nacct_1,active\nacct_2,active\nacct_3,active\n"
	metadata := map[string]any{"row_count": 3, "status": req.Status, "mime_type": "text/csv"}
	ref := t.store.put("query", content, metadata)
	return &tools.Result{
		ForLLM:   fmt.Sprintf("stored account query result ref=%s rows=3 preview=acct_1,acct_2,acct_3", ref),
		ForUser:  content,
		Ref:      ref,
		Metadata: metadata,
	}, nil
}

type readArtifactTool struct {
	store *artifactStore
}

func (t readArtifactTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        "read_artifact",
		Description: "Reads a bounded artifact preview by ref.",
		Permission:  policy.PermissionRead,
		Timeout:     100 * time.Millisecond,
		Schema: tools.Schema{
			JSONSchema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string"},"max_chars":{"type":"integer","minimum":1,"maximum":200}},"required":["ref"],"additionalProperties":false}`),
		},
	}
}

func (t readArtifactTool) Execute(ctx context.Context, input json.RawMessage, env tools.Env) (*tools.Result, error) {
	var req struct {
		Ref      string `json:"ref"`
		MaxChars int    `json:"max_chars"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}
	if req.MaxChars == 0 {
		req.MaxChars = 80
	}
	value, ok := t.store.get(req.Ref)
	if !ok {
		return &tools.Result{ForLLM: "artifact not found", IsError: true}, nil
	}
	preview := value.Content
	if len(preview) > req.MaxChars {
		preview = preview[:req.MaxChars]
	}
	return &tools.Result{
		ForLLM:   fmt.Sprintf("artifact ref=%s kind=%s preview:\n%s", req.Ref, value.Kind, preview),
		ForUser:  preview,
		Ref:      req.Ref,
		Metadata: value.Metadata,
	}, nil
}

type printSink struct{}

func (s printSink) Emit(ctx context.Context, event agentcore.Event) error {
	if event.Type != agentcore.EventToolCompleted {
		return nil
	}
	fmt.Printf("event=%s metadata=%s\n", event.Type, formatMetadata(event.Metadata))
	return nil
}

func formatMetadata(metadata map[string]any) string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, metadata[key]))
	}
	return strings.Join(parts, ",")
}

func main() {
	ctx := context.Background()
	store := newArtifactStore()
	registry := tools.NewRegistry()
	registry.Register(queryAccountsTool{store: store})
	registry.Register(readArtifactTool{store: store})

	agent, err := agentcore.NewAgent(
		agentcore.WithLLM(&mockLLM{responses: []*ports.ChatResponse{
			{ToolCalls: []ports.ToolCall{{Name: "query_accounts", Input: json.RawMessage(`{"status":"active"}`)}}},
			{ToolCalls: []ports.ToolCall{{Name: "read_artifact", Input: json.RawMessage(`{"ref":"artifact:query-1","max_chars":80}`)}}},
			{Content: "Final answer: active accounts were found."},
		}}),
		agentcore.WithToolRegistry(registry),
		agentcore.WithEventSink(printSink{}),
	)
	if err != nil {
		panic(err)
	}

	result, err := agent.Run(ctx, agentcore.RunRequest{Input: "Find active accounts and inspect the stored result."})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content)
	value, _ := store.get("artifact:query-1")
	fmt.Printf("artifact artifact:query-1 rows=%v\n", value.Metadata["row_count"])
}
