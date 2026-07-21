package agentadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/tools"
	"github.com/eruca/goagents/memorykit"
)

const (
	searchMemorySchema = `{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string","minLength":1},"key":{"type":"string"},"kinds":{"type":"array","items":{"enum":["fact","decision","constraint","lesson"]},"uniqueItems":true}}}`
	readMemorySchema   = `{"type":"object","additionalProperties":false,"required":["memory_id"],"properties":{"memory_id":{"type":"string","format":"uuid"}}}`
	writeMemorySchema  = `{"type":"object","additionalProperties":false,"required":["kind","key","content","reason"],"properties":{"kind":{"enum":["fact","decision","constraint","lesson"]},"key":{"type":"string","minLength":1},"content":{"type":"string","minLength":1},"valid_until":{"type":"string","format":"date-time"},"reason":{"type":"string","minLength":1}}}`
)

type ExplicitWriteAllowed func(agentcore.RunRequest) bool

type ToolProviderConfig struct {
	Store              memorykit.LifecycleStore
	DeepRecall         *memorykit.Recaller
	ResolveScope       ScopeResolver
	AllowExplicitWrite ExplicitWriteAllowed
	ValidateContent    memorykit.ContentValidator
	NewID              func() string
	Now                func() time.Time
}

type writeCacheKey struct {
	runID  agentcore.RunID
	digest [sha256.Size]byte
}

type writeIdentity struct {
	id  string
	now time.Time
}

type ToolProvider struct {
	cfg ToolProviderConfig

	writeMu    sync.Mutex
	writeCache map[writeCacheKey]writeIdentity
}

func NewToolProvider(config ToolProviderConfig) (*ToolProvider, error) {
	if config.Store == nil || isTypedNil(config.Store) || config.DeepRecall == nil ||
		config.ResolveScope == nil || config.AllowExplicitWrite == nil || config.ValidateContent == nil ||
		isTypedNil(config.ValidateContent) || config.NewID == nil || config.Now == nil {
		return nil, fmt.Errorf("%w: incomplete memory tool provider configuration", memorykit.ErrInvalidMemory)
	}
	return &ToolProvider{cfg: config, writeCache: make(map[writeCacheKey]writeIdentity)}, nil
}

func (p *ToolProvider) Tools(ctx context.Context, request agentcore.RunRequest) ([]tools.Tool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metadata, err := cloneMetadata(request.Metadata)
	if err != nil {
		return nil, err
	}
	resolverMetadata, err := cloneMetadata(metadata)
	if err != nil {
		return nil, err
	}
	scope, err := p.cfg.ResolveScope(resolverMetadata)
	if err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	request.Metadata = metadata
	writeAllowed := p.cfg.AllowExplicitWrite(request)
	if writeAllowed && (request.RunID.IsZero() || strings.TrimSpace(request.UserID) == "") {
		return nil, fmt.Errorf("%w: explicit memory write requires trusted run and user identity", memorykit.ErrInvalidMemory)
	}

	registered := []tools.Tool{
		p.newReadTool(scope),
		p.newSearchTool(scope),
	}
	if writeAllowed {
		registered = append(registered, p.newWriteTool(scope, request.UserID, request.RunID))
	}
	return registered, nil
}

type requestScopedTool struct {
	spec tools.Spec
	run  func(context.Context, json.RawMessage) (*tools.Result, error)
}

func (t requestScopedTool) Spec() tools.Spec { return t.spec }

func (t requestScopedTool) Execute(ctx context.Context, input json.RawMessage, _ tools.Env) (*tools.Result, error) {
	if err := t.spec.Schema.ValidateInput(input); err != nil {
		return nil, fmt.Errorf("%w: invalid %s input", memorykit.ErrInvalidMemory, t.spec.Name)
	}
	return t.run(ctx, append(json.RawMessage(nil), input...))
}

func (p *ToolProvider) newSearchTool(scope memorykit.Scope) tools.Tool {
	return requestScopedTool{
		spec: tools.Spec{
			Name: "search_memory", Description: "Search effective project memory by query and optional filters.",
			Permission: policy.PermissionRead, Schema: tools.Schema{JSONSchema: json.RawMessage(searchMemorySchema)},
		},
		run: func(ctx context.Context, input json.RawMessage) (*tools.Result, error) {
			var parsed struct {
				Query string           `json:"query"`
				Key   string           `json:"key"`
				Kinds []memorykit.Kind `json:"kinds"`
			}
			if err := decodeToolInput(input, &parsed); err != nil {
				return nil, err
			}
			for _, kind := range parsed.Kinds {
				if !kind.IsValid() {
					return nil, fmt.Errorf("%w: invalid search kind", memorykit.ErrInvalidMemory)
				}
			}
			now := p.cfg.Now()
			if now.IsZero() {
				return nil, fmt.Errorf("%w: memory tool clock returned zero time", memorykit.ErrInvalidMemory)
			}
			keys := make([]string, 0, 1)
			if parsed.Key != "" {
				keys = append(keys, parsed.Key)
			}
			result, err := p.cfg.DeepRecall.Recall(ctx, memorykit.RecallRequest{
				Scope: scope, Text: parsed.Query, Keys: keys,
				Kinds: append([]memorykit.Kind(nil), parsed.Kinds...), Now: now,
			})
			if err != nil {
				if memorykit.IsRecoverable(err) {
					return memoryReadFailure(), nil
				}
				return nil, err
			}
			records, err := recallToolRecords(result.Items)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(records)
			if err != nil {
				return nil, fmt.Errorf("%w: encode search result", memorykit.ErrInvalidRecallResult)
			}
			return &tools.Result{ForLLM: string(encoded)}, nil
		},
	}
}

func (p *ToolProvider) newReadTool(scope memorykit.Scope) tools.Tool {
	return requestScopedTool{
		spec: tools.Spec{
			Name: "read_memory", Description: "Read one effective project memory record by ID.",
			Permission: policy.PermissionRead, Schema: tools.Schema{JSONSchema: json.RawMessage(readMemorySchema)},
		},
		run: func(ctx context.Context, input json.RawMessage) (*tools.Result, error) {
			var parsed struct {
				MemoryID string `json:"memory_id"`
			}
			if err := decodeToolInput(input, &parsed); err != nil {
				return nil, err
			}
			if err := memorykit.ValidateMemoryID(parsed.MemoryID); err != nil {
				return nil, err
			}
			now := p.cfg.Now()
			if now.IsZero() {
				return nil, fmt.Errorf("%w: memory tool clock returned zero time", memorykit.ErrInvalidMemory)
			}
			memory, err := p.cfg.Store.Get(ctx, scope, parsed.MemoryID)
			if err != nil {
				if errors.Is(err, memorykit.ErrNotFound) {
					return memoryNotFound(), nil
				}
				if memorykit.IsRecoverable(err) {
					return memoryReadFailure(), nil
				}
				return nil, err
			}
			if memory.ID != parsed.MemoryID || memory.Scope != scope || !memory.Kind.IsValid() || !memory.Status.IsValid() ||
				strings.TrimSpace(memory.Key) == "" || strings.TrimSpace(memory.Content) == "" || memory.Version <= 0 ||
				(!memory.ValidUntil.IsZero() && !memory.ValidUntil.After(memory.ValidFrom)) {
				return nil, memorykit.ErrInvalidRecallResult
			}
			if memory.Status != memorykit.StatusActive || now.Before(memory.ValidFrom) ||
				(!memory.ValidUntil.IsZero() && !now.Before(memory.ValidUntil)) {
				return memoryNotFound(), nil
			}
			sources, err := p.cfg.Store.Sources(ctx, scope, parsed.MemoryID)
			if err != nil {
				if memorykit.IsRecoverable(err) {
					return memoryReadFailure(), nil
				}
				return nil, err
			}
			refs, err := sourceRefs(sources)
			if err != nil {
				return nil, err
			}
			record := readToolRecord{
				Found: true, ID: memory.ID, Kind: memory.Kind, Key: memory.Key,
				Content: memory.Content, Version: memory.Version, SourceRefs: refs,
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				return nil, fmt.Errorf("%w: encode read result", memorykit.ErrInvalidRecallResult)
			}
			return &tools.Result{ForLLM: string(encoded)}, nil
		},
	}
}

func (p *ToolProvider) newWriteTool(scope memorykit.Scope, actor string, runID agentcore.RunID) tools.Tool {
	return requestScopedTool{
		spec: tools.Spec{
			Name: "remember_project_memory", Description: "Save an explicit project memory request.",
			Permission: policy.PermissionWrite, Schema: tools.Schema{JSONSchema: json.RawMessage(writeMemorySchema)},
		},
		run: func(ctx context.Context, input json.RawMessage) (*tools.Result, error) {
			var parsed struct {
				Kind       memorykit.Kind `json:"kind"`
				Key        string         `json:"key"`
				Content    string         `json:"content"`
				ValidUntil string         `json:"valid_until"`
				Reason     string         `json:"reason"`
			}
			if err := decodeToolInput(input, &parsed); err != nil {
				return nil, err
			}
			if !parsed.Kind.IsValid() {
				return nil, fmt.Errorf("%w: invalid memory kind", memorykit.ErrInvalidMemory)
			}
			var validUntil *time.Time
			if parsed.ValidUntil != "" {
				parsedTime, err := time.Parse(time.RFC3339, parsed.ValidUntil)
				if err != nil {
					return nil, fmt.Errorf("%w: invalid valid_until", memorykit.ErrInvalidMemory)
				}
				validUntil = &parsedTime
			}
			normalized := struct {
				Kind       memorykit.Kind `json:"kind"`
				Key        string         `json:"key"`
				Content    string         `json:"content"`
				ValidUntil *time.Time     `json:"valid_until,omitempty"`
				Reason     string         `json:"reason"`
			}{parsed.Kind, parsed.Key, parsed.Content, validUntil, parsed.Reason}
			canonical, err := json.Marshal(normalized)
			if err != nil {
				return nil, fmt.Errorf("%w: canonicalize memory write", memorykit.ErrInvalidMemory)
			}
			digest := sha256.Sum256(canonical)

			if err := p.cfg.ValidateContent.ValidateMemoryContent(ctx, parsed.Content); err != nil {
				return memoryWriteFailure(), nil
			}
			identity, err := p.writeIdentity(runID, digest)
			if err != nil {
				return nil, err
			}
			if validUntil != nil && !validUntil.After(identity.now) {
				return nil, fmt.Errorf("%w: valid_until must follow valid_from", memorykit.ErrInvalidMemory)
			}
			until := time.Time{}
			if validUntil != nil {
				until = *validUntil
			}
			created, err := p.cfg.Store.Create(ctx, memorykit.CreateRequest{
				ID: identity.id, Scope: scope, Kind: parsed.Kind, Key: parsed.Key,
				Status: memorykit.StatusActive, Content: parsed.Content,
				ValidFrom: identity.now, ValidUntil: until,
				Importance: 0, Confidence: 0, SourceAgentID: runID.String(),
				Actor: actor, Reason: parsed.Reason,
				IdempotencyKey: "agent-write:" + runID.String() + ":" + hex.EncodeToString(digest[:]),
				Sources:        []memorykit.Source{{Kind: "agent_run", Ref: "agent-run:" + runID.String()}},
				Now:            identity.now,
			})
			if err != nil {
				return memoryWriteFailure(), nil
			}
			if created.ID != identity.id || created.Version <= 0 {
				return nil, memorykit.ErrInvalidRecallResult
			}
			encoded, err := json.Marshal(struct {
				ID      string `json:"id"`
				Version int64  `json:"version"`
			}{ID: created.ID, Version: created.Version})
			if err != nil {
				return nil, fmt.Errorf("%w: encode memory write result", memorykit.ErrInvalidRecallResult)
			}
			return &tools.Result{ForLLM: string(encoded), ForUser: "项目记忆已保存"}, nil
		},
	}
}

func (p *ToolProvider) writeIdentity(runID agentcore.RunID, digest [sha256.Size]byte) (writeIdentity, error) {
	key := writeCacheKey{runID: runID, digest: digest}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if identity, exists := p.writeCache[key]; exists {
		return identity, nil
	}
	id := p.cfg.NewID()
	if err := memorykit.ValidateMemoryID(id); err != nil {
		return writeIdentity{}, err
	}
	now := p.cfg.Now()
	if now.IsZero() {
		return writeIdentity{}, fmt.Errorf("%w: memory tool clock returned zero time", memorykit.ErrInvalidMemory)
	}
	identity := writeIdentity{id: id, now: now}
	p.writeCache[key] = identity
	return identity, nil
}

type toolRecord struct {
	ID         string         `json:"id"`
	Kind       memorykit.Kind `json:"kind"`
	Key        string         `json:"key"`
	Content    string         `json:"content"`
	SourceRefs []string       `json:"source_refs"`
}

type readToolRecord struct {
	Found      bool           `json:"found"`
	ID         string         `json:"id"`
	Kind       memorykit.Kind `json:"kind"`
	Key        string         `json:"key"`
	Content    string         `json:"content"`
	Version    int64          `json:"version"`
	SourceRefs []string       `json:"source_refs"`
}

func recallToolRecords(items []memorykit.RecallItem) ([]toolRecord, error) {
	records := make([]toolRecord, len(items))
	for index, item := range items {
		refs, err := sourceRefs(item.Sources)
		if err != nil {
			return nil, err
		}
		records[index] = toolRecord{
			ID: item.Memory.ID, Kind: item.Memory.Kind, Key: item.Memory.Key,
			Content: item.Memory.Content, SourceRefs: refs,
		}
	}
	return records, nil
}

func sourceRefs(sources []memorykit.Source) ([]string, error) {
	seen := make(map[string]struct{}, len(sources))
	refs := make([]string, 0, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.Ref) == "" {
			return nil, memorykit.ErrInvalidRecallResult
		}
		if _, exists := seen[source.Ref]; exists {
			continue
		}
		seen[source.Ref] = struct{}{}
		refs = append(refs, source.Ref)
	}
	sort.Strings(refs)
	return refs, nil
}

func decodeToolInput(input json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(input)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: invalid tool input", memorykit.ErrInvalidMemory)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%w: invalid tool input", memorykit.ErrInvalidMemory)
	}
	return nil
}

func memoryNotFound() *tools.Result {
	return &tools.Result{ForLLM: `{"found":false}`, ForUser: "未找到项目记忆"}
}

func memoryReadFailure() *tools.Result {
	return &tools.Result{ForLLM: "项目记忆暂时无法读取", ForUser: "项目记忆暂时无法读取", IsError: true}
}

func memoryWriteFailure() *tools.Result {
	return &tools.Result{ForLLM: "项目记忆未保存", ForUser: "项目记忆未保存", IsError: true}
}
