package agentadapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	"github.com/google/uuid"
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
		Limits: adapterTestLimits(), Now: func() time.Time { return adapterNow },
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
		{name: "limits", mutate: func(c *ToolProviderConfig) { c.Limits = memorykit.Limits{} }},
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

func TestReadMemoryRejectsMixedSnapshotsAndDisappearingRecordsAsNotFound(t *testing.T) {
	first := validToolMemory(toolMemoryID1, "original")
	mutations := []struct {
		name   string
		second memorykit.Memory
	}{
		{name: "correct", second: func() memorykit.Memory { value := first; value.Content = "corrected"; value.Version++; return value }()},
		{name: "forget", second: func() memorykit.Memory {
			value := first
			value.Status = memorykit.StatusInactive
			value.Version++
			return value
		}()},
		{name: "erase tombstone", second: func() memorykit.Memory {
			value := first
			value.Status = memorykit.StatusInactive
			value.Content = ""
			value.Version++
			value.UpdatedAt = value.UpdatedAt.Add(time.Minute)
			return value
		}()},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			store := &toolLifecycleStore{
				getResults: []toolGetResult{{memory: first}, {memory: test.second}},
				sources:    []memorykit.Source{{Kind: "run", Ref: "source:1"}},
			}
			result, err := executeReadTool(t, store, toolMemoryID1)
			if err != nil || result == nil || result.IsError || result.ForLLM != `{"found":false}` {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}

	store := &toolLifecycleStore{getMemory: first, sourcesErr: memorykit.ErrNotFound}
	result, err := executeReadTool(t, store, toolMemoryID1)
	if err != nil || result == nil || result.ForLLM != `{"found":false}` {
		t.Fatalf("Sources ErrNotFound result=%#v err=%v", result, err)
	}
}

func TestReadMemoryClassifiesSecondGetFailures(t *testing.T) {
	first := validToolMemory(toolMemoryID1, "content")
	tests := []struct {
		name      string
		err       error
		wantLLM   string
		wantIsErr bool
		wantGoErr bool
	}{
		{name: "not found", err: memorykit.ErrNotFound, wantLLM: `{"found":false}`},
		{name: "recoverable", err: &memorykit.BackendError{Op: "get2", Recoverable: true, Err: errors.New("private")}, wantLLM: "项目记忆暂时无法读取", wantIsErr: true},
		{name: "integrity", err: memorykit.ErrInvalidMemory, wantGoErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &toolLifecycleStore{
				getResults: []toolGetResult{{memory: first}, {err: test.err}},
				sources:    []memorykit.Source{{Kind: "run", Ref: "source:1"}},
			}
			result, err := executeReadTool(t, store, toolMemoryID1)
			if test.wantGoErr {
				if err == nil || result != nil {
					t.Fatalf("result=%#v err=%v", result, err)
				}
				return
			}
			if err != nil || result == nil || result.ForLLM != test.wantLLM || result.IsError != test.wantIsErr {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestReadMemoryRejectsMalformedSnapshotsBeforeClassifyingRaces(t *testing.T) {
	valid := validToolMemory(toolMemoryID1, "content")
	tests := []struct {
		name   string
		first  memorykit.Memory
		second memorykit.Memory
	}{
		{name: "foreign scope in second", first: valid, second: func() memorykit.Memory { value := valid; value.Scope.SubjectID = "foreign"; return value }()},
		{name: "zero version in second", first: valid, second: func() memorykit.Memory { value := valid; value.Version = 0; return value }()},
		{name: "nan confidence in second", first: valid, second: func() memorykit.Memory { value := valid; value.Confidence = math.NaN(); return value }()},
		{name: "same nan snapshot twice", first: func() memorykit.Memory { value := valid; value.Confidence = math.NaN(); return value }(), second: func() memorykit.Memory { value := valid; value.Confidence = math.NaN(); return value }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &toolLifecycleStore{
				getResults: []toolGetResult{{memory: test.first}, {memory: test.second}},
				sources:    []memorykit.Source{{Kind: "run", Ref: "source:1"}},
			}
			result, err := executeReadTool(t, store, toolMemoryID1)
			if result != nil || !errors.Is(err, memorykit.ErrInvalidRecallResult) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestReadMemoryReturnsNotFoundForMemorystoreEraseTombstone(t *testing.T) {
	store := newToolMemoryStore(t)
	scope := toolScope("tenant-1", "project-1")
	createToolMemory(t, store, toolMemoryID1, scope, memorykit.StatusActive, adapterNow.Add(-time.Hour), time.Time{}, "content")
	created, err := store.Get(context.Background(), scope, toolMemoryID1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Erase(context.Background(), memorykit.VersionedCommand{
		Scope: scope, ID: created.ID, ExpectedVersion: created.Version,
		Actor: "privacy", Reason: "erase", Now: adapterNow,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := executeReadTool(t, store, toolMemoryID1)
	if err != nil || result == nil || result.IsError || result.ForLLM != `{"found":false}` {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestReadMemoryRejectsInvalidEmptyContentSnapshots(t *testing.T) {
	valid := validToolMemory(toolMemoryID1, "content")
	tests := []struct {
		name   string
		memory memorykit.Memory
	}{
		{name: "active", memory: func() memorykit.Memory { value := valid; value.Content = ""; return value }()},
		{name: "candidate", memory: func() memorykit.Memory {
			value := valid
			value.Status = memorykit.StatusCandidate
			value.Content = ""
			return value
		}()},
		{name: "tombstone zero version", memory: func() memorykit.Memory {
			value := valid
			value.Status = memorykit.StatusInactive
			value.Content = ""
			value.Version = 0
			return value
		}()},
		{name: "tombstone nan confidence", memory: func() memorykit.Memory {
			value := valid
			value.Status = memorykit.StatusInactive
			value.Content = ""
			value.Confidence = math.NaN()
			return value
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := executeReadTool(t, &toolLifecycleStore{getMemory: test.memory}, toolMemoryID1)
			if result != nil || !errors.Is(err, memorykit.ErrInvalidRecallResult) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestReadMemoryValidatesStoredMemoryAndSourcesAgainstLimits(t *testing.T) {
	limits := adapterTestLimits()
	tests := []struct {
		name    string
		memory  memorykit.Memory
		sources []memorykit.Source
	}{
		{name: "content", memory: func() memorykit.Memory {
			value := validToolMemory(toolMemoryID1, "content")
			value.Content = strings.Repeat("x", limits.MaxContentRunes+1)
			return value
		}(), sources: []memorykit.Source{{Kind: "run", Ref: "source:1"}}},
		{name: "source ref", memory: validToolMemory(toolMemoryID1, "content"), sources: []memorykit.Source{{Kind: "run", Ref: strings.Repeat("x", limits.MaxMetadataRunes+1)}}},
		{name: "source count", memory: validToolMemory(toolMemoryID1, "content"), sources: func() []memorykit.Source {
			values := make([]memorykit.Source, limits.MaxSourcesPerMemory+1)
			for index := range values {
				values[index] = memorykit.Source{Kind: "run", Ref: fmt.Sprintf("source:%d", index)}
			}
			return values
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &toolLifecycleStore{getMemory: test.memory, sources: test.sources}
			result, err := executeReadTool(t, store, toolMemoryID1)
			if err == nil || result != nil {
				t.Fatalf("result=%#v err=%v", result, err)
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

func TestSearchMemoryValidatesRecallItemsAgainstToolLimits(t *testing.T) {
	limits := adapterTestLimits()
	manySources := make([]memorykit.Source, limits.MaxSourcesPerMemory+1)
	for index := range manySources {
		manySources[index] = memorykit.Source{Kind: "run", Ref: fmt.Sprintf("source:%d", index)}
	}
	tests := []struct {
		name   string
		limits memorykit.Limits
		set    memorykit.CandidateSet
	}{
		{name: "content", limits: limits, set: memorykit.CandidateSet{FullText: []memorykit.Candidate{searchToolCandidate(toolMemoryID1, strings.Repeat("x", limits.MaxContentRunes+1), []memorykit.Source{{Kind: "run", Ref: "source:1"}}, 1)}}},
		{name: "source ref", limits: limits, set: memorykit.CandidateSet{FullText: []memorykit.Candidate{searchToolCandidate(toolMemoryID1, "content", []memorykit.Source{{Kind: "run", Ref: strings.Repeat("x", limits.MaxMetadataRunes+1)}}, 1)}}},
		{name: "source count", limits: limits, set: memorykit.CandidateSet{FullText: []memorykit.Candidate{searchToolCandidate(toolMemoryID1, "content", manySources, 1)}}},
		{name: "item count", limits: func() memorykit.Limits { value := limits; value.MaxListItems = 1; return value }(), set: memorykit.CandidateSet{FullText: []memorykit.Candidate{
			searchToolCandidate(toolMemoryID1, "first", []memorykit.Source{{Kind: "run", Ref: "source:1"}}, 1),
			searchToolCandidate(toolMemoryID2, "second", []memorykit.Source{{Kind: "run", Ref: "source:2"}}, 2),
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, err := NewToolProvider(ToolProviderConfig{
				Store: newToolMemoryStore(t), DeepRecall: newToolRecaller(t, &toolRecallStore{set: test.set}),
				ResolveScope: resolveToolScope, AllowExplicitWrite: allowToolWrite,
				ValidateContent: &acceptContentValidator{}, Limits: test.limits, Now: func() time.Time { return adapterNow },
			})
			if err != nil {
				t.Fatal(err)
			}
			registered, err := provider.Tools(context.Background(), agentcore.RunRequest{Metadata: trustedMetadata("tenant-1", "project-1", false)})
			if err != nil {
				t.Fatal(err)
			}
			result, executeErr := findTool(t, registered, "search_memory").Execute(context.Background(), json.RawMessage(`{"query":"test"}`), ports.ToolEnv{})
			if executeErr == nil || result != nil {
				t.Fatalf("result=%#v err=%v", result, executeErr)
			}
		})
	}
}

func TestRememberMemoryCreatesActiveScopedRecordAndRetriesIdempotently(t *testing.T) {
	base := newToolMemoryStore(t)
	store := &toolLifecycleStore{delegate: base}
	validator := &acceptContentValidator{}
	nowCalls := 0
	provider, err := NewToolProvider(ToolProviderConfig{
		Store: store, DeepRecall: newToolRecaller(t, &toolRecallStore{}), ResolveScope: resolveToolScope,
		AllowExplicitWrite: allowToolWrite, ValidateContent: validator,
		Limits: adapterTestLimits(),
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
	expectedID, expectedDigest := expectedToolWriteIdentity(t, runID, input)
	maliciousEnv := ports.ToolEnv{UserID: "attacker", Metadata: trustedMetadata("foreign", "foreign", false)}
	first, err := write.Execute(context.Background(), input, maliciousEnv)
	if err != nil || first.IsError || first.ForUser != "项目记忆已保存" || !strings.Contains(first.ForLLM, expectedID) || strings.Contains(first.ForLLM, "run real gate") {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := write.Execute(context.Background(), input, maliciousEnv)
	if err != nil || second.IsError || second.ForLLM != first.ForLLM {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	if len(store.createRequests) != 1 {
		t.Fatalf("Create calls = %d, want 1", len(store.createRequests))
	}
	request := store.createRequests[0]
	if request.ID != expectedID || request.Scope != toolScope("tenant-1", "project-1") || request.Status != memorykit.StatusActive ||
		request.Actor != "user-1" || request.SourceAgentID != runID.String() || request.ValidFrom != adapterNow ||
		request.Importance != 0 || request.Confidence != 0 || request.Now != adapterNow ||
		len(request.Sources) != 1 || request.Sources[0].Kind != "agent_run" || request.Sources[0].Ref != "agent-run:"+runID.String() ||
		request.IdempotencyKey != "agent-write:"+runID.String()+":"+expectedDigest {
		t.Fatalf("create request = %#v", request)
	}
	if validator.calls != 2 {
		t.Fatalf("validator calls = %d, want 2", validator.calls)
	}

	differentInput := json.RawMessage(`{"kind":"lesson","key":"testing.other","content":"different","reason":"explicit"}`)
	differentID, _ := expectedToolWriteIdentity(t, runID, differentInput)
	different, err := write.Execute(context.Background(), differentInput, maliciousEnv)
	if err != nil || different.IsError || !strings.Contains(different.ForLLM, differentID) || differentID == expectedID || nowCalls != 2 {
		t.Fatalf("different=%#v err=%v differentID=%s nowCalls=%d", different, err, differentID, nowCalls)
	}
}

func TestRememberMemoryRebuildsStableIdentityAcrossToolsAndProviders(t *testing.T) {
	store := &toolLifecycleStore{delegate: newToolMemoryStore(t)}
	runID := agentcore.NewRunID()
	request := agentcore.RunRequest{
		RunID: runID, UserID: "user-1", Metadata: trustedMetadata("tenant-1", "project-1", true),
	}
	input := json.RawMessage(`{"kind":"decision","key":"architecture.memory","content":"use project memory","reason":"explicit"}`)
	expectedID, _ := expectedToolWriteIdentity(t, runID, input)

	firstProvider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
	firstTools, err := firstProvider.Tools(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	secondTools, err := firstProvider.Tools(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	clockCalled := false
	secondProvider, err := NewToolProvider(ToolProviderConfig{
		Store: store, DeepRecall: newToolRecaller(t, &toolRecallStore{}), ResolveScope: resolveToolScope,
		AllowExplicitWrite: allowToolWrite, ValidateContent: &acceptContentValidator{}, Limits: adapterTestLimits(),
		Now: func() time.Time { clockCalled = true; return adapterNow.Add(time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	thirdTools, err := secondProvider.Tools(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	var observations []string
	for _, registered := range [][]tools.Tool{firstTools, secondTools, thirdTools} {
		result, executeErr := findTool(t, registered, "remember_project_memory").Execute(context.Background(), input, ports.ToolEnv{})
		if executeErr != nil || result == nil || result.IsError {
			t.Fatalf("result=%#v err=%v", result, executeErr)
		}
		observations = append(observations, result.ForLLM)
	}
	if observations[0] != observations[1] || observations[1] != observations[2] || !strings.Contains(observations[0], expectedID) {
		t.Fatalf("observations = %v", observations)
	}
	if len(store.createRequests) != 1 || clockCalled {
		t.Fatalf("Create calls=%d second clock called=%v", len(store.createRequests), clockCalled)
	}
}

func TestRememberMemoryConcurrentProvidersConvergeOnDeterministicIdentity(t *testing.T) {
	const workers = 12
	store := &toolLifecycleStore{delegate: newToolMemoryStore(t)}
	runID := agentcore.NewRunID()
	request := agentcore.RunRequest{RunID: runID, UserID: "user", Metadata: trustedMetadata("tenant-1", "project-1", true)}
	input := json.RawMessage(`{"kind":"constraint","key":"release.gate","content":"run verification","reason":"explicit"}`)
	expectedID, _ := expectedToolWriteIdentity(t, runID, input)
	start := make(chan struct{})
	results := make(chan string, workers)
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		provider, err := NewToolProvider(ToolProviderConfig{
			Store: store, DeepRecall: newToolRecaller(t, &toolRecallStore{}), ResolveScope: resolveToolScope,
			AllowExplicitWrite: allowToolWrite, ValidateContent: &acceptContentValidator{}, Limits: adapterTestLimits(),
			Now: func() time.Time { return adapterNow },
		})
		if err != nil {
			t.Fatal(err)
		}
		registered, err := provider.Tools(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		write := findTool(t, registered, "remember_project_memory")
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, executeErr := write.Execute(context.Background(), input, ports.ToolEnv{})
			if executeErr != nil {
				errorsCh <- executeErr
				return
			}
			if result == nil || result.IsError {
				errorsCh <- fmt.Errorf("unexpected result: %#v", result)
				return
			}
			results <- result.ForLLM
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	for observation := range results {
		if !strings.Contains(observation, expectedID) {
			t.Errorf("observation = %q, want ID %s", observation, expectedID)
		}
	}
	for index := 1; index < len(store.createRequests); index++ {
		if !reflect.DeepEqual(store.createRequests[0], store.createRequests[index]) {
			t.Fatalf("concurrent requests differ: %#v", store.createRequests)
		}
	}
}

func TestRememberMemoryRecoversCreateConflictByReadingDeterministicRecord(t *testing.T) {
	runID := agentcore.NewRunID()
	input := json.RawMessage(`{"kind":"fact","key":"conflict","content":"content","reason":"explicit"}`)
	id, digest := expectedToolWriteIdentity(t, runID, input)
	current := memorykit.Memory{
		ID: id, Scope: toolScope("tenant-1", "project-1"), Kind: memorykit.KindFact, Key: "conflict",
		Content: "content", Status: memorykit.StatusActive, ValidFrom: adapterNow,
		SourceAgentID: runID.String(), CreatedBy: "user", IdempotencyKey: "agent-write:" + runID.String() + ":" + digest,
		Version: 1, CreatedAt: adapterNow, UpdatedAt: adapterNow,
	}
	store := &toolLifecycleStore{
		getResults: []toolGetResult{{err: memorykit.ErrNotFound}, {memory: current}},
		createErr:  memorykit.ErrConflict,
	}
	provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
	registered, err := provider.Tools(context.Background(), agentcore.RunRequest{RunID: runID, UserID: "user", Metadata: trustedMetadata("tenant-1", "project-1", true)})
	if err != nil {
		t.Fatal(err)
	}
	result, executeErr := findTool(t, registered, "remember_project_memory").Execute(context.Background(), input, ports.ToolEnv{})
	if executeErr != nil || result == nil || result.IsError || !strings.Contains(result.ForLLM, id) || len(store.createRequests) != 1 {
		t.Fatalf("result=%#v err=%v Create calls=%d", result, executeErr, len(store.createRequests))
	}
}

func TestRememberMemoryDoesNotReclassifyCommittedCreateWhenSourcesUnavailable(t *testing.T) {
	store := &toolLifecycleStore{getErr: memorykit.ErrNotFound, sourcesErr: errors.New("sources unavailable")}
	provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
	registered, err := provider.Tools(context.Background(), agentcore.RunRequest{RunID: agentcore.NewRunID(), UserID: "user", Metadata: trustedMetadata("tenant-1", "project-1", true)})
	if err != nil {
		t.Fatal(err)
	}
	result, executeErr := findTool(t, registered, "remember_project_memory").Execute(context.Background(), json.RawMessage(`{"kind":"fact","key":"committed","content":"content","reason":"explicit"}`), ports.ToolEnv{})
	if executeErr != nil || result == nil || result.IsError || result.ForUser != "项目记忆已保存" {
		t.Fatalf("result=%#v err=%v", result, executeErr)
	}
}

func TestRememberMemoryReplayRejectsCandidateStatus(t *testing.T) {
	runID := agentcore.NewRunID()
	input := json.RawMessage(`{"kind":"fact","key":"candidate","content":"content","reason":"explicit"}`)
	id, digest := expectedToolWriteIdentity(t, runID, input)
	current := memorykit.Memory{
		ID: id, Scope: toolScope("tenant-1", "project-1"), Kind: memorykit.KindFact, Key: "candidate",
		Content: "content", Status: memorykit.StatusCandidate, ValidFrom: adapterNow,
		SourceAgentID: runID.String(), CreatedBy: "user", IdempotencyKey: "agent-write:" + runID.String() + ":" + digest,
		Version: 1, CreatedAt: adapterNow, UpdatedAt: adapterNow,
	}
	store := &toolLifecycleStore{getMemory: current}
	provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
	registered, err := provider.Tools(context.Background(), agentcore.RunRequest{RunID: runID, UserID: "user", Metadata: trustedMetadata("tenant-1", "project-1", true)})
	if err != nil {
		t.Fatal(err)
	}
	result, executeErr := findTool(t, registered, "remember_project_memory").Execute(context.Background(), input, ports.ToolEnv{})
	if executeErr == nil || result != nil || len(store.createRequests) != 0 {
		t.Fatalf("result=%#v err=%v Create calls=%d", result, executeErr, len(store.createRequests))
	}
}

func TestRememberMemoryReplayReturnsCurrentCorrectedForgottenOrSupersededRecord(t *testing.T) {
	for _, action := range []string{"correct", "forget", "supersede"} {
		t.Run(action, func(t *testing.T) {
			base := newToolMemoryStore(t)
			store := &toolLifecycleStore{delegate: base}
			provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
			runID := agentcore.NewRunID()
			registered, err := provider.Tools(context.Background(), agentcore.RunRequest{
				RunID: runID, UserID: "user", Metadata: trustedMetadata("tenant-1", "project-1", true),
			})
			if err != nil {
				t.Fatal(err)
			}
			input := json.RawMessage(`{"kind":"lesson","key":"retry.current","content":"original","reason":"explicit"}`)
			write := findTool(t, registered, "remember_project_memory")
			if result, executeErr := write.Execute(context.Background(), input, ports.ToolEnv{}); executeErr != nil || result == nil || result.IsError {
				t.Fatalf("initial result=%#v err=%v", result, executeErr)
			}
			id, _ := expectedToolWriteIdentity(t, runID, input)
			current, err := base.Get(context.Background(), toolScope("tenant-1", "project-1"), id)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "correct":
				current, err = base.Correct(context.Background(), memorykit.CorrectRequest{
					Command: memorykit.VersionedCommand{Scope: current.Scope, ID: id, ExpectedVersion: current.Version, Actor: "reviewer", Reason: "correct", Now: adapterNow.Add(time.Minute)},
					Content: "corrected", ValidFrom: current.ValidFrom, ValidUntil: current.ValidUntil,
					Importance: current.Importance, Confidence: current.Confidence,
					Sources: []memorykit.Source{{Kind: "review", Ref: "review:1"}},
				})
			case "forget":
				current, err = base.Forget(context.Background(), memorykit.VersionedCommand{
					Scope: current.Scope, ID: id, ExpectedVersion: current.Version, Actor: "reviewer", Reason: "forget", Now: adapterNow.Add(time.Minute),
				})
			case "supersede":
				_, err = base.Create(context.Background(), memorykit.CreateRequest{
					ID: toolMemoryID3, Scope: current.Scope, Kind: current.Kind, Key: current.Key,
					Status: memorykit.StatusActive, Content: "replacement", ValidFrom: adapterNow.Add(time.Minute),
					SourceAgentID: "reviewer", Actor: "reviewer", Reason: "replace", Now: adapterNow.Add(time.Minute),
				})
				if err == nil {
					current, err = base.Get(context.Background(), current.Scope, id)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			result, executeErr := write.Execute(context.Background(), input, ports.ToolEnv{})
			if executeErr != nil || result == nil || result.IsError || !strings.Contains(result.ForLLM, fmt.Sprintf(`"version":%d`, current.Version)) {
				t.Fatalf("replay result=%#v err=%v current=%#v", result, executeErr, current)
			}
			if len(store.createRequests) != 1 {
				t.Fatalf("Create calls = %d, want 1", len(store.createRequests))
			}
		})
	}
}

func TestRememberMemoryReplayRejectsErasedCurrentRecord(t *testing.T) {
	base := newToolMemoryStore(t)
	store := &toolLifecycleStore{delegate: base}
	provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
	runID := agentcore.NewRunID()
	registered, err := provider.Tools(context.Background(), agentcore.RunRequest{RunID: runID, UserID: "user", Metadata: trustedMetadata("tenant-1", "project-1", true)})
	if err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"kind":"fact","key":"privacy","content":"erase me","reason":"explicit"}`)
	write := findTool(t, registered, "remember_project_memory")
	if result, executeErr := write.Execute(context.Background(), input, ports.ToolEnv{}); executeErr != nil || result == nil || result.IsError {
		t.Fatalf("initial result=%#v err=%v", result, executeErr)
	}
	id, _ := expectedToolWriteIdentity(t, runID, input)
	current, err := base.Get(context.Background(), toolScope("tenant-1", "project-1"), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Erase(context.Background(), memorykit.VersionedCommand{
		Scope: current.Scope, ID: id, ExpectedVersion: current.Version, Actor: "privacy", Reason: "erase", Now: adapterNow.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	result, executeErr := write.Execute(context.Background(), input, ports.ToolEnv{})
	if executeErr == nil || result != nil || len(store.createRequests) != 1 {
		t.Fatalf("replay result=%#v err=%v Create calls=%d", result, executeErr, len(store.createRequests))
	}
}

func TestRememberMemoryRejectsMalformedCreateResultWithoutClaimingSuccess(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(memorykit.Memory) memorykit.Memory
	}{
		{name: "foreign", mutate: func(memory memorykit.Memory) memorykit.Memory { memory.Scope.SubjectID = "foreign"; return memory }},
		{name: "nonactive", mutate: func(memory memorykit.Memory) memorykit.Memory {
			memory.Status = memorykit.StatusInactive
			return memory
		}},
		{name: "wrong content", mutate: func(memory memorykit.Memory) memorykit.Memory { memory.Content = "different"; return memory }},
		{name: "oversized", mutate: func(memory memorykit.Memory) memorykit.Memory {
			memory.Content = strings.Repeat("x", adapterTestLimits().MaxContentRunes+1)
			return memory
		}},
		{name: "timestamp", mutate: func(memory memorykit.Memory) memorykit.Memory {
			memory.UpdatedAt = memory.UpdatedAt.Add(time.Second)
			return memory
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			store := &toolLifecycleStore{getErr: memorykit.ErrNotFound, createMutate: test.mutate}
			provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
			registered, err := provider.Tools(context.Background(), agentcore.RunRequest{
				RunID: agentcore.NewRunID(), UserID: "user", Metadata: trustedMetadata("tenant-1", "project-1", true),
			})
			if err != nil {
				t.Fatal(err)
			}
			result, executeErr := findTool(t, registered, "remember_project_memory").Execute(context.Background(), json.RawMessage(`{"kind":"fact","key":"integrity","content":"expected","reason":"explicit"}`), ports.ToolEnv{})
			if executeErr == nil || result != nil {
				t.Fatalf("result=%#v err=%v", result, executeErr)
			}
		})
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

func TestRememberMemoryRejectsInvalidClockAndValidityAsIntegrityErrors(t *testing.T) {
	tests := []struct {
		name  string
		now   func() time.Time
		input json.RawMessage
	}{
		{name: "clock", now: func() time.Time { return time.Time{} }, input: json.RawMessage(`{"kind":"fact","key":"k","content":"c","reason":"r"}`)},
		{name: "valid until", now: func() time.Time { return adapterNow }, input: json.RawMessage(`{"kind":"fact","key":"k","content":"c","valid_until":"2026-07-21T12:00:00Z","reason":"r"}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &toolLifecycleStore{delegate: newToolMemoryStore(t)}
			provider, err := NewToolProvider(ToolProviderConfig{
				Store: store, DeepRecall: newToolRecaller(t, &toolRecallStore{}), ResolveScope: resolveToolScope,
				AllowExplicitWrite: allowToolWrite, ValidateContent: &acceptContentValidator{}, Limits: adapterTestLimits(), Now: test.now,
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

func TestMemoryToolsPreserveCanonicalCancellation(t *testing.T) {
	t.Run("pre canceled", func(t *testing.T) {
		provider := newToolProviderForTest(t, newToolMemoryStore(t), newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
		registered, err := provider.Tools(context.Background(), agentcore.RunRequest{Metadata: trustedMetadata("tenant-1", "project-1", false)})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, executeErr := findTool(t, registered, "read_memory").Execute(ctx, json.RawMessage(`{}`), ports.ToolEnv{})
		if result != nil || !errors.Is(executeErr, context.Canceled) {
			t.Fatalf("result=%#v err=%v", result, executeErr)
		}
	})

	writeInput := json.RawMessage(`{"kind":"fact","key":"cancel","content":"content","reason":"explicit"}`)
	for _, test := range []struct {
		name      string
		validator memorykit.ContentValidator
		store     *toolLifecycleStore
		want      error
	}{
		{name: "validator canceled", validator: &acceptContentValidator{err: context.Canceled}, store: &toolLifecycleStore{delegate: newToolMemoryStore(t)}, want: context.Canceled},
		{name: "create deadline", validator: &acceptContentValidator{}, store: &toolLifecycleStore{delegate: newToolMemoryStore(t), createErr: context.DeadlineExceeded}, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newToolProviderForTest(t, test.store, newToolRecaller(t, &toolRecallStore{}), test.validator)
			registered, err := provider.Tools(context.Background(), agentcore.RunRequest{RunID: agentcore.NewRunID(), UserID: "user", Metadata: trustedMetadata("tenant-1", "project-1", true)})
			if err != nil {
				t.Fatal(err)
			}
			result, executeErr := findTool(t, registered, "remember_project_memory").Execute(context.Background(), writeInput, ports.ToolEnv{})
			if result != nil || !errors.Is(executeErr, test.want) {
				t.Fatalf("result=%#v err=%v", result, executeErr)
			}
		})
	}

	for _, test := range []struct {
		name  string
		store *toolLifecycleStore
		want  error
	}{
		{name: "get canceled", store: &toolLifecycleStore{getErr: context.Canceled}, want: context.Canceled},
		{name: "sources deadline", store: &toolLifecycleStore{getMemory: validToolMemory(toolMemoryID1, "content"), sourcesErr: context.DeadlineExceeded}, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, executeErr := executeReadTool(t, test.store, toolMemoryID1)
			if result != nil || !errors.Is(executeErr, test.want) {
				t.Fatalf("result=%#v err=%v", result, executeErr)
			}
		})
	}
}

func newToolProviderForTest(t *testing.T, store memorykit.LifecycleStore, recall *memorykit.Recaller, validator memorykit.ContentValidator) *ToolProvider {
	t.Helper()
	provider, err := NewToolProvider(ToolProviderConfig{
		Store: store, DeepRecall: recall, ResolveScope: resolveToolScope,
		AllowExplicitWrite: allowToolWrite, ValidateContent: validator,
		Limits: adapterTestLimits(), Now: func() time.Time { return adapterNow },
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

func expectedToolWriteIdentity(t *testing.T, runID agentcore.RunID, input json.RawMessage) (string, string) {
	t.Helper()
	var parsed struct {
		Kind       memorykit.Kind `json:"kind"`
		Key        string         `json:"key"`
		Content    string         `json:"content"`
		ValidUntil string         `json:"valid_until"`
		Reason     string         `json:"reason"`
	}
	if err := json.Unmarshal(input, &parsed); err != nil {
		t.Fatal(err)
	}
	var validUntil *time.Time
	if parsed.ValidUntil != "" {
		value, err := time.Parse(time.RFC3339, parsed.ValidUntil)
		if err != nil {
			t.Fatal(err)
		}
		validUntil = &value
	}
	canonical, err := json.Marshal(struct {
		Kind       memorykit.Kind `json:"kind"`
		Key        string         `json:"key"`
		Content    string         `json:"content"`
		ValidUntil *time.Time     `json:"valid_until,omitempty"`
		Reason     string         `json:"reason"`
	}{parsed.Kind, parsed.Key, parsed.Content, validUntil, parsed.Reason})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	return uuid.NewSHA1(uuid.UUID(runID), digest[:]).String(), fmt.Sprintf("%x", digest)
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

func validToolMemory(id, content string) memorykit.Memory {
	return memorykit.Memory{
		ID: id, Scope: toolScope("tenant-1", "project-1"), Kind: memorykit.KindLesson,
		Key: "testing", Content: content, Status: memorykit.StatusActive,
		ValidFrom: adapterNow.Add(-time.Hour), Importance: 10, Confidence: 0.8,
		SourceAgentID: "agent-1", CreatedBy: "user-1", IdempotencyKey: "fixture", Version: 1,
		CreatedAt: adapterNow.Add(-time.Hour), UpdatedAt: adapterNow.Add(-time.Hour),
	}
}

func searchToolCandidate(id, content string, sources []memorykit.Source, rank int) memorykit.Candidate {
	memory := validToolMemory(id, content)
	return memorykit.Candidate{Memory: memory, Sources: sources, Channel: memorykit.ChannelFullText, Rank: rank}
}

func executeReadTool(t *testing.T, store memorykit.LifecycleStore, id string) (*tools.Result, error) {
	t.Helper()
	provider := newToolProviderForTest(t, store, newToolRecaller(t, &toolRecallStore{}), &acceptContentValidator{})
	registered, err := provider.Tools(context.Background(), agentcore.RunRequest{Metadata: trustedMetadata("tenant-1", "project-1", false)})
	if err != nil {
		t.Fatal(err)
	}
	return findTool(t, registered, "read_memory").Execute(context.Background(), json.RawMessage(`{"memory_id":"`+id+`"}`), ports.ToolEnv{})
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
	createMutate   func(memorykit.Memory) memorykit.Memory
	createRequests []memorykit.CreateRequest
	createdSources []memorykit.Source
	getMemory      memorykit.Memory
	getErr         error
	getResults     []toolGetResult
	getCalls       int
	sources        []memorykit.Source
	sourcesErr     error
	sourcesCalls   int
}

type toolGetResult struct {
	memory memorykit.Memory
	err    error
}

func (s *toolLifecycleStore) Create(ctx context.Context, request memorykit.CreateRequest) (memorykit.Memory, error) {
	s.mu.Lock()
	s.createRequests = append(s.createRequests, cloneCreateRequestForToolTest(request))
	s.createdSources = append([]memorykit.Source(nil), request.Sources...)
	err := s.createErr
	delegate := s.delegate
	mutate := s.createMutate
	s.mu.Unlock()
	if err != nil {
		return memorykit.Memory{}, err
	}
	if delegate != nil {
		return delegate.Create(ctx, request)
	}
	memory := memorykit.Memory{
		ID: request.ID, Scope: request.Scope, Kind: request.Kind, Key: request.Key, Content: request.Content,
		Status: request.Status, ValidFrom: request.ValidFrom, ValidUntil: request.ValidUntil,
		Importance: request.Importance, Confidence: request.Confidence, SourceAgentID: request.SourceAgentID,
		CreatedBy: request.Actor, IdempotencyKey: request.IdempotencyKey, Version: 1,
		CreatedAt: request.Now, UpdatedAt: request.Now,
	}
	if mutate != nil {
		memory = mutate(memory)
	}
	return memory, nil
}

func (s *toolLifecycleStore) Get(ctx context.Context, scope memorykit.Scope, id string) (memorykit.Memory, error) {
	s.mu.Lock()
	if s.getCalls < len(s.getResults) {
		result := s.getResults[s.getCalls]
		s.getCalls++
		s.mu.Unlock()
		return result.memory, result.err
	}
	s.getCalls++
	err := s.getErr
	delegate := s.delegate
	memory := s.getMemory
	s.mu.Unlock()
	if err != nil {
		return memorykit.Memory{}, err
	}
	if delegate != nil {
		return delegate.Get(ctx, scope, id)
	}
	return memory, nil
}

func (s *toolLifecycleStore) Sources(ctx context.Context, scope memorykit.Scope, id string) ([]memorykit.Source, error) {
	s.mu.Lock()
	s.sourcesCalls++
	err := s.sourcesErr
	delegate := s.delegate
	sources := append([]memorykit.Source(nil), s.sources...)
	createdSources := append([]memorykit.Source(nil), s.createdSources...)
	sourcesWereNil := s.sources == nil
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if delegate != nil {
		return delegate.Sources(ctx, scope, id)
	}
	if sourcesWereNil {
		return createdSources, nil
	}
	return sources, nil
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
