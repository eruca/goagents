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
	"time"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/policy"
	"github.com/eruca/goagents/goagent/tools"
	"github.com/eruca/goagents/memorykit"
	"github.com/google/uuid"
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
	Limits             memorykit.Limits
	Now                func() time.Time
}

type ToolProvider struct {
	cfg ToolProviderConfig
}

func NewToolProvider(config ToolProviderConfig) (*ToolProvider, error) {
	if config.Store == nil || isTypedNil(config.Store) || config.DeepRecall == nil ||
		config.ResolveScope == nil || config.AllowExplicitWrite == nil || config.ValidateContent == nil ||
		isTypedNil(config.ValidateContent) || config.Now == nil {
		return nil, fmt.Errorf("%w: incomplete memory tool provider configuration", memorykit.ErrInvalidMemory)
	}
	if err := config.Limits.Validate(); err != nil {
		return nil, err
	}
	return &ToolProvider{cfg: config}, nil
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
			records, err := recallToolRecords(result.Items, scope, now, p.cfg.Limits)
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
			memory, getErr := p.cfg.Store.Get(ctx, scope, parsed.MemoryID)
			if contextErr := canonicalContextError(ctx, getErr); contextErr != nil {
				return nil, contextErr
			}
			if getErr != nil {
				if errors.Is(getErr, memorykit.ErrNotFound) {
					return memoryNotFound(), nil
				}
				if memorykit.IsRecoverable(getErr) {
					return memoryReadFailure(), nil
				}
				return nil, getErr
			}
			if memory.ID != parsed.MemoryID || memory.Scope != scope || !memory.Status.IsValid() {
				return nil, memorykit.ErrInvalidRecallResult
			}
			if !effectiveMemory(memory, now) {
				return memoryNotFound(), nil
			}
			sources, sourcesErr := p.cfg.Store.Sources(ctx, scope, parsed.MemoryID)
			if contextErr := canonicalContextError(ctx, sourcesErr); contextErr != nil {
				return nil, contextErr
			}
			if sourcesErr != nil {
				if errors.Is(sourcesErr, memorykit.ErrNotFound) {
					return memoryNotFound(), nil
				}
				if memorykit.IsRecoverable(sourcesErr) {
					return memoryReadFailure(), nil
				}
				return nil, sourcesErr
			}
			confirmed, confirmErr := p.cfg.Store.Get(ctx, scope, parsed.MemoryID)
			if contextErr := canonicalContextError(ctx, confirmErr); contextErr != nil {
				return nil, contextErr
			}
			if confirmErr != nil {
				if errors.Is(confirmErr, memorykit.ErrNotFound) {
					return memoryNotFound(), nil
				}
				if memorykit.IsRecoverable(confirmErr) {
					return memoryReadFailure(), nil
				}
				return nil, confirmErr
			}
			if confirmed != memory || !effectiveMemory(confirmed, now) {
				return memoryNotFound(), nil
			}
			if err := validateStoredMemory(confirmed, sources, p.cfg.Limits); err != nil {
				return nil, err
			}
			refs, err := sourceRefs(sources)
			if err != nil {
				return nil, err
			}
			record := readToolRecord{
				Found: true, ID: confirmed.ID, Kind: confirmed.Kind, Key: confirmed.Key,
				Content: confirmed.Content, Version: confirmed.Version, SourceRefs: refs,
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
			id := uuid.NewSHA1(uuid.UUID(runID), digest[:]).String()
			idempotencyKey := "agent-write:" + runID.String() + ":" + hex.EncodeToString(digest[:])

			validationErr := p.cfg.ValidateContent.ValidateMemoryContent(ctx, parsed.Content)
			if contextErr := canonicalContextError(ctx, validationErr); contextErr != nil {
				return nil, contextErr
			}
			if validationErr != nil {
				return memoryWriteFailure(), nil
			}

			expectation := writeExpectation{
				id: id, scope: scope, kind: parsed.Kind, key: parsed.Key,
				actor: actor, sourceAgentID: runID.String(), idempotencyKey: idempotencyKey,
			}
			current, getErr := p.cfg.Store.Get(ctx, scope, id)
			if contextErr := canonicalContextError(ctx, getErr); contextErr != nil {
				return nil, contextErr
			}
			if getErr == nil {
				if err := validateReplayMemory(current, expectation, p.cfg.Limits); err != nil {
					return nil, err
				}
				return memoryWriteSuccess(current)
			}
			if !errors.Is(getErr, memorykit.ErrNotFound) {
				return memoryWriteFailure(), nil
			}

			now := p.cfg.Now()
			if now.IsZero() {
				return nil, fmt.Errorf("%w: memory tool clock returned zero time", memorykit.ErrInvalidMemory)
			}
			if validUntil != nil && !validUntil.After(now) {
				return nil, fmt.Errorf("%w: valid_until must follow valid_from", memorykit.ErrInvalidMemory)
			}
			until := time.Time{}
			if validUntil != nil {
				until = *validUntil
			}
			request := memorykit.CreateRequest{
				ID: id, Scope: scope, Kind: parsed.Kind, Key: parsed.Key,
				Status: memorykit.StatusActive, Content: parsed.Content,
				ValidFrom: now, ValidUntil: until,
				Importance: 0, Confidence: 0, SourceAgentID: runID.String(),
				Actor: actor, Reason: parsed.Reason,
				IdempotencyKey: idempotencyKey,
				Sources:        []memorykit.Source{{Kind: "agent_run", Ref: "agent-run:" + runID.String()}},
				Now:            now,
			}
			created, createErr := p.cfg.Store.Create(ctx, request)
			if contextErr := canonicalContextError(ctx, createErr); contextErr != nil {
				return nil, contextErr
			}
			if errors.Is(createErr, memorykit.ErrConflict) {
				current, getErr = p.cfg.Store.Get(ctx, scope, id)
				if contextErr := canonicalContextError(ctx, getErr); contextErr != nil {
					return nil, contextErr
				}
				if getErr != nil {
					return memoryWriteFailure(), nil
				}
				if err := validateReplayMemory(current, expectation, p.cfg.Limits); err != nil {
					return nil, err
				}
				return memoryWriteSuccess(current)
			}
			if createErr != nil {
				return memoryWriteFailure(), nil
			}
			if err := validateInitialWriteMemory(created, request, p.cfg.Limits); err != nil {
				return nil, err
			}
			return memoryWriteSuccess(created)
		},
	}
}

type writeExpectation struct {
	id, key, actor, sourceAgentID, idempotencyKey string
	scope                                         memorykit.Scope
	kind                                          memorykit.Kind
}

func validateReplayMemory(memory memorykit.Memory, expected writeExpectation, limits memorykit.Limits) error {
	if memory.ID != expected.id || memory.Scope != expected.scope || memory.Kind != expected.kind || memory.Key != expected.key ||
		memory.CreatedBy != expected.actor || memory.SourceAgentID != expected.sourceAgentID ||
		memory.IdempotencyKey != expected.idempotencyKey || strings.TrimSpace(memory.Content) == "" ||
		(memory.Status != memorykit.StatusActive && memory.Status != memorykit.StatusInactive) {
		return memorykit.ErrInvalidRecallResult
	}
	if err := validateStoredMemory(memory, nil, limits); err != nil {
		return err
	}
	return nil
}

func validateInitialWriteMemory(memory memorykit.Memory, request memorykit.CreateRequest, limits memorykit.Limits) error {
	if memory.ID != request.ID || memory.Scope != request.Scope || memory.Kind != request.Kind || memory.Key != request.Key ||
		memory.Status != request.Status || memory.Content != request.Content || !memory.ValidFrom.Equal(request.ValidFrom) ||
		!memory.ValidUntil.Equal(request.ValidUntil) || memory.Importance != request.Importance ||
		memory.Confidence != request.Confidence || memory.SourceAgentID != request.SourceAgentID ||
		memory.CreatedBy != request.Actor || memory.IdempotencyKey != request.IdempotencyKey || memory.Version != 1 ||
		memory.CreatedAt.IsZero() || memory.UpdatedAt.IsZero() ||
		timeDifference(memory.CreatedAt, request.Now) > time.Microsecond ||
		timeDifference(memory.UpdatedAt, request.Now) > time.Microsecond {
		return memorykit.ErrInvalidRecallResult
	}
	return validateStoredMemory(memory, request.Sources, limits)
}

func timeDifference(left, right time.Time) time.Duration {
	difference := left.Sub(right)
	if difference < 0 {
		return -difference
	}
	return difference
}

func memoryWriteSuccess(memory memorykit.Memory) (*tools.Result, error) {
	encoded, err := json.Marshal(struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}{ID: memory.ID, Version: memory.Version})
	if err != nil {
		return nil, fmt.Errorf("%w: encode memory write result", memorykit.ErrInvalidRecallResult)
	}
	return &tools.Result{ForLLM: string(encoded), ForUser: "项目记忆已保存"}, nil
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

func recallToolRecords(items []memorykit.RecallItem, scope memorykit.Scope, now time.Time, limits memorykit.Limits) ([]toolRecord, error) {
	if len(items) > limits.MaxListItems {
		return nil, memorykit.ErrInvalidRecallResult
	}
	records := make([]toolRecord, len(items))
	for index, item := range items {
		if item.Memory.Scope != scope || !effectiveMemory(item.Memory, now) {
			return nil, memorykit.ErrInvalidRecallResult
		}
		if err := validateStoredMemory(item.Memory, item.Sources, limits); err != nil {
			return nil, err
		}
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

func effectiveMemory(memory memorykit.Memory, now time.Time) bool {
	return memory.Status == memorykit.StatusActive && !now.Before(memory.ValidFrom) &&
		(memory.ValidUntil.IsZero() || now.Before(memory.ValidUntil))
}

func validateStoredMemory(memory memorykit.Memory, sources []memorykit.Source, limits memorykit.Limits) error {
	if memory.Version <= 0 {
		return memorykit.ErrInvalidRecallResult
	}
	if err := memorykit.ValidateCreateRequest(memorykit.CreateRequest{
		ID: memory.ID, Scope: memory.Scope, Kind: memory.Kind, Key: memory.Key,
		Status: memory.Status, Content: memory.Content,
		ValidFrom: memory.ValidFrom, ValidUntil: memory.ValidUntil,
		Importance: memory.Importance, Confidence: memory.Confidence,
		SourceAgentID: memory.SourceAgentID, Actor: memory.CreatedBy,
		IdempotencyKey: memory.IdempotencyKey, Sources: append([]memorykit.Source(nil), sources...),
	}, limits); err != nil {
		return memorykit.ErrInvalidRecallResult
	}
	return nil
}

func canonicalContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
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
