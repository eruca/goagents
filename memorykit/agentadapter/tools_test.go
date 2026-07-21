package agentadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/memorystore"
)

const (
	toolMemoryID1 = "11111111-1111-4111-8111-111111111111"
	toolMemoryID2 = "22222222-2222-4222-8222-222222222222"
	toolMemoryID3 = "33333333-3333-4333-8333-333333333333"
)

const (
	wantSearchSchema = `{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string","minLength":1},"key":{"type":"string"},"kinds":{"type":"array","items":{"enum":["fact","decision","constraint","lesson"]},"uniqueItems":true}}}`
	wantReadSchema   = `{"type":"object","additionalProperties":false,"required":["memory_id"],"properties":{"memory_id":{"type":"string","format":"uuid"}}}`
	wantWriteSchema  = `{"type":"object","additionalProperties":false,"required":["kind","key","content","reason"],"properties":{"kind":{"enum":["fact","decision","constraint","lesson"]},"key":{"type":"string","minLength":1},"content":{"type":"string","minLength":1},"valid_until":{"type":"string","format":"date-time"},"reason":{"type":"string","minLength":1}}}`
)

func TestToolProviderRegistersRequestScopedToolsAndExactSchemas(t *testing.T) {
	store := newToolMemoryStore(t)
	provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})

	metadata := trustedMetadata("tenant-1", "project-1", false)
	metadata["nested"] = map[string]any{"value": "original"}
	readOnly, err := provider.Tools(context.Background(), agentcore.RunRequest{UserID: "user-1", Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	assertToolNames(t, readOnly, "read_memory", "search_memory")
	if metadata["nested"].(map[string]any)["value"] != "original" {
		t.Fatal("Tools mutated caller metadata")
	}

	runID := agentcore.NewRunID()
	writeEnabled, err := provider.Tools(context.Background(), agentcore.RunRequest{
		RunID: runID, UserID: "user-1", Metadata: trustedMetadata("tenant-1", "project-1", true),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertToolNames(t, writeEnabled, "read_memory", "remember_project_memory", "search_memory")

	wantSchemas := map[string]string{
		"search_memory": wantSearchSchema, "read_memory": wantReadSchema, "remember_project_memory": wantWriteSchema,
	}
	for _, tool := range writeEnabled {
		schema := string(tool.Spec().Schema.JSONSchema)
		if schema != wantSchemas[tool.Spec().Name] {
			t.Fatalf("%s schema = %s", tool.Spec().Name, schema)
		}
		lower := strings.ToLower(schema)
		for _, forbidden := range []string{"tenant", "project", "subject", "user_id"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s schema exposes %q: %s", tool.Spec().Name, forbidden, schema)
			}
		}
	}
	if findTool(t, writeEnabled, "remember_project_memory").Spec().Permission != policy.PermissionWrite {
		t.Fatal("remember_project_memory is not PermissionWrite")
	}
}

func TestToolProviderRejectsNilAndMalformedTrustedDependencies(t *testing.T) {
	validStore := newToolMemoryStore(t)
	validRecall := newToolRecaller(t, &toolRecallStore{})
	validValidator := &acceptContentValidator{}
	valid := ToolProviderConfig{
		Store: validStore, DeepRecall: validRecall, ResolveScope: resolveToolScope,
		AllowExplicitWrite: allowToolWrite, ValidateContent: validValidator,
		NewID: func() string { return toolMemoryID1 }, Now: func() time.Time { return adapterNow },
	}
	var typedStore *toolLifecycleStore
	var typedValidator *acceptContentValidator
	tests := []struct {
		name   string
		mutate func(*ToolProviderConfig)
	}{
		{name: "nil store", mutate: func(c *ToolProviderConfig) { c.Store = nil }},
		{name: "typed nil store", mutate: func(c *ToolProviderConfig) { c.Store = typedStore }},
		{name: "recall", mutate: func(c *ToolProviderConfig) { c.DeepRecall = nil }},
		{name: "resolver", mutate: func(c *ToolProviderConfig) { c.ResolveScope = nil }},
		{name: "predicate", mutate: func(c *ToolProviderConfig) { c.AllowExplicitWrite = nil }},
		{name: "validator", mutate: func(c *ToolProviderConfig) { c.ValidateContent = nil }},
		{name: "typed nil validator", mutate: func(c *ToolProviderConfig) { c.ValidateContent = typedValidator }},
		{name: "id", mutate: func(c *ToolProviderConfig) { c.NewID = nil }},
		{name: "clock", mutate: func(c *ToolProviderConfig) { c.Now = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := NewToolProvider(config); err == nil {
				t.Fatal("NewToolProvider accepted incomplete config")
			}
		})
	}

	provider, err := NewToolProvider(valid)
	if err != nil {
		t.Fatal(err)
	}
	malformed := []agentcore.RunRequest{
		{Metadata: trustedMetadata("", "project-1", false)},
		{Metadata: trustedMetadata("tenant-1", "project-1", true), UserID: "user-1"},
		{Metadata: trustedMetadata("tenant-1", "project-1", true), RunID: agentcore.NewRunID(), UserID: "   "},
	}
	for index, request := range malformed {
		if got, err := provider.Tools(context.Background(), request); err == nil || len(got) != 0 {
			t.Fatalf("malformed request %d: tools=%v err=%v", index, got, err)
		}
	}
}

func TestReadMemoryClosesScopeIgnoresExecutionEnvAndFiltersEffectiveness(t *testing.T) {
	store := newToolMemoryStore(t)
	createToolMemory(t, store, toolMemoryID1, toolScope("tenant-1", "project-1"), memorykit.StatusActive, adapterNow.Add(-time.Hour), adapterNow.Add(time.Hour), "own")
	createToolMemory(t, store, toolMemoryID2, toolScope("tenant-1", "project-2"), memorykit.StatusActive, adapterNow.Add(-time.Hour), time.Time{}, "foreign-secret")
	provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
	requestTools, err := provider.Tools(context.Background(), agentcore.RunRequest{Metadata: trustedMetadata("tenant-1", "project-1", false)})
	if err != nil {
		t.Fatal(err)
	}
	read := findTool(t, requestTools, "read_memory")

	foreign, err := read.Execute(context.Background(), json.RawMessage(`{"memory_id":"`+toolMemoryID2+`"}`), ports.ToolEnv{
		UserID: "attacker", Metadata: trustedMetadata("tenant-1", "project-2", true),
	})
	if err != nil || foreign.IsError || foreign.ForLLM != `{"found":false}` || strings.Contains(foreign.ForLLM, "foreign-secret") {
		t.Fatalf("foreign result=%#v err=%v", foreign, err)
	}
	own, err := read.Execute(context.Background(), json.RawMessage(`{"memory_id":"`+toolMemoryID1+`"}`), ports.ToolEnv{
		UserID: "attacker", Metadata: trustedMetadata("other", "other", true),
	})
	if err != nil || own.IsError || !strings.Contains(own.ForLLM, "own") {
		t.Fatalf("own result=%#v err=%v", own, err)
	}

	for _, memory := range []memorykit.Memory{
		{ID: toolMemoryID1, Scope: toolScope("tenant-1", "project-1"), Kind: memorykit.KindFact, Key: "k", Content: "candidate", Status: memorykit.StatusCandidate, ValidFrom: adapterNow.Add(-time.Hour), Version: 1},
		{ID: toolMemoryID1, Scope: toolScope("tenant-1", "project-1"), Kind: memorykit.KindFact, Key: "k", Content: "inactive", Status: memorykit.StatusInactive, ValidFrom: adapterNow.Add(-time.Hour), Version: 1},
		{ID: toolMemoryID1, Scope: toolScope("tenant-1", "project-1"), Kind: memorykit.KindFact, Key: "k", Content: "expired", Status: memorykit.StatusActive, ValidFrom: adapterNow.Add(-2 * time.Hour), ValidUntil: adapterNow, Version: 1},
		{ID: toolMemoryID1, Scope: toolScope("tenant-1", "project-1"), Kind: memorykit.KindFact, Key: "k", Content: "future", Status: memorykit.StatusActive, ValidFrom: adapterNow.Add(time.Hour), Version: 1},
	} {
		scripted := &toolLifecycleStore{getMemory: memory}
		p := newToolProviderForTest(t, scripted, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
		registered, toolsErr := p.Tools(context.Background(), agentcore.RunRequest{Metadata: trustedMetadata("tenant-1", "project-1", false)})
		if toolsErr != nil {
			t.Fatal(toolsErr)
		}
		result, executeErr := findTool(t, registered, "read_memory").Execute(context.Background(), json.RawMessage(`{"memory_id":"`+toolMemoryID1+`"}`), ports.ToolEnv{})
		if executeErr != nil || result.ForLLM != `{"found":false}` || scripted.sourcesCalls != 0 {
			t.Fatalf("status=%s result=%#v err=%v sourcesCalls=%d", memory.Status, result, executeErr, scripted.sourcesCalls)
		}
	}
}

func TestReadMemoryReturnsOnlySortedUniqueSourceRefsAndClassifiesErrors(t *testing.T) {
	memory := memorykit.Memory{
		ID: toolMemoryID1, Scope: toolScope("tenant-1", "project-1"), Kind: memorykit.KindLesson,
		Key: "testing", Content: "bounded content", Status: memorykit.StatusActive,
		ValidFrom: adapterNow.Add(-time.Hour), Version: 2,
	}
	store := &toolLifecycleStore{getMemory: memory, sources: []memorykit.Source{
		{Kind: "git", Ref: "z-ref", EvidenceHash: "secret-a"},
		{Kind: "artifact", Ref: "a-ref", EvidenceHash: "secret-b"},
		{Kind: "run", Ref: "z-ref", EvidenceHash: "secret-c"},
	}}
	provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
	registered, err := provider.Tools(context.Background(), agentcore.RunRequest{Metadata: trustedMetadata("tenant-1", "project-1", false)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := findTool(t, registered, "read_memory").Execute(context.Background(), json.RawMessage(`{"memory_id":"`+toolMemoryID1+`"}`), ports.ToolEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ForLLM != `{"found":true,"id":"11111111-1111-4111-8111-111111111111","kind":"lesson","key":"testing","content":"bounded content","version":2,"source_refs":["a-ref","z-ref"]}` || strings.Contains(result.ForLLM, "EvidenceHash") || strings.Contains(result.ForLLM, "secret") {
		t.Fatalf("ForLLM = %s", result.ForLLM)
	}

	for _, test := range []struct {
		name      string
		err       error
		wantError bool
	}{
		{name: "recoverable", err: &memorykit.BackendError{Op: "get", Recoverable: true, Err: errors.New("secret backend")}},
		{name: "integrity", err: memorykit.ErrInvalidMemory, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			failing := &toolLifecycleStore{getErr: test.err}
			p := newToolProviderForTest(t, failing, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
			registered, toolsErr := p.Tools(context.Background(), agentcore.RunRequest{Metadata: trustedMetadata("tenant-1", "project-1", false)})
			if toolsErr != nil {
				t.Fatal(toolsErr)
			}
			got, executeErr := findTool(t, registered, "read_memory").Execute(context.Background(), json.RawMessage(`{"memory_id":"`+toolMemoryID1+`"}`), ports.ToolEnv{})
			if test.wantError {
				if executeErr == nil || got != nil {
					t.Fatalf("result=%#v err=%v", got, executeErr)
				}
			} else if executeErr != nil || got == nil || !got.IsError || strings.Contains(got.ForLLM+got.ForUser, "secret") {
				t.Fatalf("result=%#v err=%v", got, executeErr)
			}
		})
	}
}

func TestSearchMemoryUsesClosedScopeStrictKindsAndClassifiesErrors(t *testing.T) {
	recallStore := &toolRecallStore{set: memorykit.CandidateSet{FullText: []memorykit.Candidate{{
		Memory: memorykit.Memory{
			ID: toolMemoryID1, Scope: toolScope("tenant-1", "project-1"), Kind: memorykit.KindLesson,
			Key: "testing.pgvector", Content: "run integration", Status: memorykit.StatusActive,
			ValidFrom: adapterNow.Add(-time.Hour), Importance: 1, Confidence: 0.8, Version: 1,
		}, Sources: []memorykit.Source{{Kind: "git", Ref: "z"}, {Kind: "run", Ref: "a"}},
		Channel: memorykit.ChannelFullText, Rank: 1,
	}}}}
	provider := newToolProviderForTest(t, newToolMemoryStore(t), newToolRecaller(t, recallStore), &acceptContentValidator{})
	registered, err := provider.Tools(context.Background(), agentcore.RunRequest{Metadata: trustedMetadata("tenant-1", "project-1", false)})
	if err != nil {
		t.Fatal(err)
	}
	search := findTool(t, registered, "search_memory")
	result, err := search.Execute(context.Background(), json.RawMessage(`{"query":"pgvector","key":"testing.pgvector","kinds":["lesson"]}`), ports.ToolEnv{Metadata: trustedMetadata("foreign", "foreign", true)})
	if err != nil || result.IsError {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if !strings.Contains(result.ForLLM, toolMemoryID1) || strings.Contains(result.ForLLM, "EvidenceHash") {
		t.Fatalf("ForLLM = %s", result.ForLLM)
	}
	if len(recallStore.queries) != 1 || recallStore.queries[0].Scope != toolScope("tenant-1", "project-1") ||
		!reflect.DeepEqual(recallStore.queries[0].Keys, []string{"testing.pgvector"}) ||
		!reflect.DeepEqual(recallStore.queries[0].Kinds, []memorykit.Kind{memorykit.KindLesson}) {
		t.Fatalf("query = %#v", recallStore.queries)
	}
	if got, executeErr := search.Execute(context.Background(), json.RawMessage(`{"query":"x","kinds":["other"]}`), ports.ToolEnv{}); executeErr == nil || got != nil {
		t.Fatalf("invalid kind result=%#v err=%v", got, executeErr)
	}

	for _, test := range []struct {
		name      string
		err       error
		wantError bool
	}{
		{name: "recoverable", err: &memorykit.BackendError{Op: "search", Recoverable: true, Err: errors.New("private outage")}},
		{name: "integrity", err: memorykit.ErrInvalidRecallResult, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newToolProviderForTest(t, newToolMemoryStore(t), newToolRecaller(t, &toolRecallStore{err: test.err}), &acceptContentValidator{})
			registered, toolsErr := p.Tools(context.Background(), agentcore.RunRequest{Metadata: trustedMetadata("tenant-1", "project-1", false)})
			if toolsErr != nil {
				t.Fatal(toolsErr)
			}
			got, executeErr := findTool(t, registered, "search_memory").Execute(context.Background(), json.RawMessage(`{"query":"x"}`), ports.ToolEnv{})
			if test.wantError {
				if executeErr == nil || got != nil {
					t.Fatalf("result=%#v err=%v", got, executeErr)
				}
			} else if executeErr != nil || got == nil || !got.IsError || strings.Contains(got.ForLLM+got.ForUser, "private") {
				t.Fatalf("result=%#v err=%v", got, executeErr)
			}
		})
	}
}

func TestRememberMemoryCreatesActiveScopedRecordAndRetriesIdempotently(t *testing.T) {
	base := newToolMemoryStore(t)
	store := &toolLifecycleStore{delegate: base}
	validator := &acceptContentValidator{}
	ids := []string{toolMemoryID1, toolMemoryID2}
	var idMu sync.Mutex
	newIDCalls := 0
	nowCalls := 0
	provider, err := NewToolProvider(ToolProviderConfig{
		Store: store, DeepRecall: newToolRecaller(t, &toolRecallStore{}), ResolveScope: resolveToolScope,
		AllowExplicitWrite: allowToolWrite, ValidateContent: validator,
		NewID: func() string {
			idMu.Lock()
			defer idMu.Unlock()
			id := ids[newIDCalls]
			newIDCalls++
			return id
		},
		Now: func() time.Time {
			nowCalls++
			return adapterNow.Add(time.Duration(nowCalls-1) * time.Minute)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runID := agentcore.NewRunID()
	registered, err := provider.Tools(context.Background(), agentcore.RunRequest{
		RunID: runID, UserID: "user-1", Metadata: trustedMetadata("tenant-1", "project-1", true),
	})
	if err != nil {
		t.Fatal(err)
	}
	write := findTool(t, registered, "remember_project_memory")
	input := json.RawMessage(`{"kind":"lesson","key":"testing.pgvector","content":"run real gate","valid_until":"2026-07-22T12:00:00Z","reason":"user explicitly requested"}`)
	maliciousEnv := ports.ToolEnv{UserID: "attacker", Metadata: trustedMetadata("foreign", "foreign", false)}
	first, err := write.Execute(context.Background(), input, maliciousEnv)
	if err != nil || first.IsError || first.ForUser != "项目记忆已保存" || !strings.Contains(first.ForLLM, toolMemoryID1) || strings.Contains(first.ForLLM, "run real gate") {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := write.Execute(context.Background(), input, maliciousEnv)
	if err != nil || second.IsError || second.ForLLM != first.ForLLM {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if newIDCalls != 1 {
		t.Fatalf("NewID calls = %d, want 1", newIDCalls)
	}
	if len(store.createRequests) != 2 || !reflect.DeepEqual(store.createRequests[0], store.createRequests[1]) {
		t.Fatalf("retry requests differ: %#v", store.createRequests)
	}
	request := store.createRequests[0]
	if request.ID != toolMemoryID1 || request.Scope != toolScope("tenant-1", "project-1") || request.Status != memorykit.StatusActive ||
		request.Actor != "user-1" || request.SourceAgentID != runID.String() || request.ValidFrom != adapterNow ||
		request.Importance != 0 || request.Confidence != 0 || request.Now != adapterNow ||
		len(request.Sources) != 1 || request.Sources[0].Kind != "agent_run" || request.Sources[0].Ref != "agent-run:"+runID.String() ||
		!strings.HasPrefix(request.IdempotencyKey, "agent-write:"+runID.String()+":") || len(strings.TrimPrefix(request.IdempotencyKey, "agent-write:"+runID.String()+":")) != 64 {
		t.Fatalf("create request = %#v", request)
	}
	if validator.calls != 2 {
		t.Fatalf("validator calls = %d, want 2", validator.calls)
	}

	different, err := write.Execute(context.Background(), json.RawMessage(`{"kind":"lesson","key":"testing.other","content":"different","reason":"explicit"}`), maliciousEnv)
	if err != nil || different.IsError || !strings.Contains(different.ForLLM, toolMemoryID2) || newIDCalls != 2 {
		t.Fatalf("different=%#v err=%v NewID=%d", different, err, newIDCalls)
	}
}

func TestRememberMemoryFailsTruthfullyAndDoesNotLeakContent(t *testing.T) {
	secret := "top-secret-content"
	for _, test := range []struct {
		name      string
		validator *acceptContentValidator
		storeErr  error
		wantCalls int
	}{
		{name: "validator", validator: &acceptContentValidator{err: errors.New("secret rejected")}, wantCalls: 0},
		{name: "store", validator: &acceptContentValidator{}, storeErr: errors.New("secret storage failure"), wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &toolLifecycleStore{delegate: newToolMemoryStore(t), createErr: test.storeErr}
			provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), test.validator)
			registered, err := provider.Tools(context.Background(), agentcore.RunRequest{
				RunID: agentcore.NewRunID(), UserID: "user-1", Metadata: trustedMetadata("tenant-1", "project-1", true),
			})
			if err != nil {
				t.Fatal(err)
			}
			result, executeErr := findTool(t, registered, "remember_project_memory").Execute(context.Background(), json.RawMessage(`{"kind":"fact","key":"secret","content":"`+secret+`","reason":"explicit"}`), ports.ToolEnv{})
			if executeErr != nil || result == nil || !result.IsError || result.ForUser != "项目记忆未保存" ||
				strings.Contains(result.ForUser+result.ForLLM, "已保存") || strings.Contains(result.ForUser+result.ForLLM, secret) || len(store.createRequests) != test.wantCalls {
				t.Fatalf("result=%#v err=%v createCalls=%d", result, executeErr, len(store.createRequests))
			}
		})
	}
}

func TestRememberMemoryRejectsInvalidClockIDAndValidityAsIntegrityErrors(t *testing.T) {
	tests := []struct {
		name  string
		newID func() string
		now   func() time.Time
		input json.RawMessage
	}{
		{name: "id", newID: func() string { return "not-a-uuid" }, now: func() time.Time { return adapterNow }, input: json.RawMessage(`{"kind":"fact","key":"k","content":"c","reason":"r"}`)},
		{name: "clock", newID: func() string { return toolMemoryID1 }, now: func() time.Time { return time.Time{} }, input: json.RawMessage(`{"kind":"fact","key":"k","content":"c","reason":"r"}`)},
		{name: "valid until", newID: func() string { return toolMemoryID1 }, now: func() time.Time { return adapterNow }, input: json.RawMessage(`{"kind":"fact","key":"k","content":"c","valid_until":"2026-07-21T12:00:00Z","reason":"r"}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &toolLifecycleStore{delegate: newToolMemoryStore(t)}
			provider, err := NewToolProvider(ToolProviderConfig{
				Store: store, DeepRecall: newToolRecaller(t, &toolRecallStore{}), ResolveScope: resolveToolScope,
				AllowExplicitWrite: allowToolWrite, ValidateContent: &acceptContentValidator{}, NewID: test.newID, Now: test.now,
			})
			if err != nil {
				t.Fatal(err)
			}
			registered, err := provider.Tools(context.Background(), agentcore.RunRequest{RunID: agentcore.NewRunID(), UserID: "user", Metadata: trustedMetadata("tenant-1", "project-1", true)})
			if err != nil {
				t.Fatal(err)
			}
			got, executeErr := findTool(t, registered, "remember_project_memory").Execute(context.Background(), test.input, ports.ToolEnv{})
			if executeErr == nil || got != nil || len(store.createRequests) != 0 {
				t.Fatalf("result=%#v err=%v createCalls=%d", got, executeErr, len(store.createRequests))
			}
		})
	}
}

func TestToolClosuresValidateInputsWhenCalledDirectly(t *testing.T) {
	provider := newToolProviderForTest(t, newToolMemoryStore(t), newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
	registered, err := provider.Tools(context.Background(), agentcore.RunRequest{RunID: agentcore.NewRunID(), UserID: "user", Metadata: trustedMetadata("tenant-1", "project-1", true)})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		input json.RawMessage
	}{
		{name: "search_memory", input: json.RawMessage(`{"query":"x","tenant":"foreign"}`)},
		{name: "read_memory", input: json.RawMessage(`{"memory_id":"not-a-uuid"}`)},
		{name: "remember_project_memory", input: json.RawMessage(`{"kind":"fact","key":"k","content":"c","reason":"r","user_id":"attacker"}`)},
	} {
		got, executeErr := findTool(t, registered, test.name).Execute(context.Background(), test.input, ports.ToolEnv{})
		if executeErr == nil || got != nil {
			t.Fatalf("%s result=%#v err=%v", test.name, got, executeErr)
		}
	}
}

func newToolProviderForTest(t *testing.T, store memorykit.LifecycleStore, recall *memorykit.Recaller, validator memorykit.ContentValidator) *ToolProvider {
	t.Helper()
	provider, err := NewToolProvider(ToolProviderConfig{
		Store: store, DeepRecall: recall, ResolveScope: resolveToolScope,
		AllowExplicitWrite: allowToolWrite, ValidateContent: validator,
		NewID: func() string { return toolMemoryID1 }, Now: func() time.Time { return adapterNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func trustedMetadata(tenant, project string, write bool) map[string]any {
	return map[string]any{"tenant": tenant, "project": project, "write": write}
}

func resolveToolScope(metadata map[string]any) (memorykit.Scope, error) {
	if nested, ok := metadata["nested"].(map[string]any); ok {
		nested["value"] = "resolver-mutated-copy"
	}
	tenant, _ := metadata["tenant"].(string)
	project, _ := metadata["project"].(string)
	return toolScope(tenant, project), nil
}

func allowToolWrite(request agentcore.RunRequest) bool {
	allowed, _ := request.Metadata["write"].(bool)
	return allowed
}

func toolScope(tenant, project string) memorykit.Scope {
	return memorykit.Scope{TenantID: tenant, SubjectType: memorykit.SubjectProject, SubjectID: project}
}

func assertToolNames(t *testing.T, registered []tools.Tool, want ...string) {
	t.Helper()
	got := make([]string, len(registered))
	for index, tool := range registered {
		got[index] = tool.Spec().Name
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool names = %v, want %v", got, want)
	}
}

func findTool(t *testing.T, registered []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tool := range registered {
		if tool.Spec().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func newToolMemoryStore(t *testing.T) *memorystore.Store {
	t.Helper()
	store, err := memorystore.New(adapterTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func createToolMemory(t *testing.T, store memorykit.LifecycleStore, id string, scope memorykit.Scope, status memorykit.Status, from, until time.Time, content string) {
	t.Helper()
	_, err := store.Create(context.Background(), memorykit.CreateRequest{
		ID: id, Scope: scope, Kind: memorykit.KindFact, Key: "key." + id[:4], Status: status, Content: content,
		ValidFrom: from, ValidUntil: until, SourceAgentID: "test", Actor: "test", Reason: "fixture", Now: adapterNow.Add(-2 * time.Hour),
		Sources: []memorykit.Source{{Kind: "test", Ref: "source:" + id}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

type acceptContentValidator struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (v *acceptContentValidator) ValidateMemoryContent(_ context.Context, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	return v.err
}

type toolRecallStore struct {
	mu      sync.Mutex
	set     memorykit.CandidateSet
	err     error
	queries []memorykit.CandidateQuery
}

func (s *toolRecallStore) SearchCandidates(_ context.Context, query memorykit.CandidateQuery) (memorykit.CandidateSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query.Keys = append([]string(nil), query.Keys...)
	query.Kinds = append([]memorykit.Kind(nil), query.Kinds...)
	s.queries = append(s.queries, query)
	return s.set, s.err
}

func newToolRecaller(t *testing.T, store memorykit.RecallStore) *memorykit.Recaller {
	t.Helper()
	recaller, err := memorykit.NewRecaller(memorykit.RecallConfig{
		Store:       store,
		CountTokens: func(string) int { return 1 },
		Policy: memorykit.RecallPolicy{
			Version: "tools-test-v1", ExactLimit: 5, FullTextLimit: 5, VectorLimit: 0,
			RRFK: 60, MinVectorSimilarity: 0.5, MaxItems: 5, MaxTokens: 20,
			MaxQueryRunes: 100, MaxKeys: 5, Deadline: time.Second,
			EmbeddingProfileID: "unused-profile", EmbeddingDimensions: 3,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return recaller
}

type toolLifecycleStore struct {
	mu             sync.Mutex
	delegate       memorykit.LifecycleStore
	createErr      error
	createRequests []memorykit.CreateRequest
	getMemory      memorykit.Memory
	getErr         error
	sources        []memorykit.Source
	sourcesErr     error
	sourcesCalls   int
}

func (s *toolLifecycleStore) Create(ctx context.Context, request memorykit.CreateRequest) (memorykit.Memory, error) {
	s.mu.Lock()
	s.createRequests = append(s.createRequests, cloneCreateRequestForToolTest(request))
	err := s.createErr
	delegate := s.delegate
	s.mu.Unlock()
	if err != nil {
		return memorykit.Memory{}, err
	}
	if delegate != nil {
		return delegate.Create(ctx, request)
	}
	return memorykit.Memory{ID: request.ID, Scope: request.Scope, Kind: request.Kind, Key: request.Key, Content: request.Content, Status: request.Status, ValidFrom: request.ValidFrom, ValidUntil: request.ValidUntil, Importance: request.Importance, Confidence: request.Confidence, Version: 1}, nil
}

func (s *toolLifecycleStore) Get(ctx context.Context, scope memorykit.Scope, id string) (memorykit.Memory, error) {
	if s.getErr != nil {
		return memorykit.Memory{}, s.getErr
	}
	if s.delegate != nil {
		return s.delegate.Get(ctx, scope, id)
	}
	return s.getMemory, nil
}

func (s *toolLifecycleStore) Sources(ctx context.Context, scope memorykit.Scope, id string) ([]memorykit.Source, error) {
	s.sourcesCalls++
	if s.sourcesErr != nil {
		return nil, s.sourcesErr
	}
	if s.delegate != nil {
		return s.delegate.Sources(ctx, scope, id)
	}
	return append([]memorykit.Source(nil), s.sources...), nil
}

func (s *toolLifecycleStore) List(ctx context.Context, query memorykit.ListQuery) ([]memorykit.Memory, error) {
	if s.delegate == nil {
		return nil, fmt.Errorf("unsupported")
	}
	return s.delegate.List(ctx, query)
}
func (s *toolLifecycleStore) Activate(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	if s.delegate == nil {
		return memorykit.Memory{}, fmt.Errorf("unsupported")
	}
	return s.delegate.Activate(ctx, command)
}
func (s *toolLifecycleStore) Correct(ctx context.Context, request memorykit.CorrectRequest) (memorykit.Memory, error) {
	if s.delegate == nil {
		return memorykit.Memory{}, fmt.Errorf("unsupported")
	}
	return s.delegate.Correct(ctx, request)
}
func (s *toolLifecycleStore) Dismiss(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	if s.delegate == nil {
		return memorykit.Memory{}, fmt.Errorf("unsupported")
	}
	return s.delegate.Dismiss(ctx, command)
}
func (s *toolLifecycleStore) Forget(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	if s.delegate == nil {
		return memorykit.Memory{}, fmt.Errorf("unsupported")
	}
	return s.delegate.Forget(ctx, command)
}
func (s *toolLifecycleStore) Erase(ctx context.Context, command memorykit.VersionedCommand) error {
	if s.delegate == nil {
		return fmt.Errorf("unsupported")
	}
	return s.delegate.Erase(ctx, command)
}
func (s *toolLifecycleStore) Revisions(ctx context.Context, query memorykit.RevisionQuery) ([]memorykit.Revision, error) {
	if s.delegate == nil {
		return nil, fmt.Errorf("unsupported")
	}
	return s.delegate.Revisions(ctx, query)
}

func cloneCreateRequestForToolTest(request memorykit.CreateRequest) memorykit.CreateRequest {
	request.Sources = append([]memorykit.Source(nil), request.Sources...)
	return request
}
