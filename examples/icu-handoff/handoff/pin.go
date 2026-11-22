package handoff

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/goagent/tools"
)

// ModelTools are the gocms tools a patient's run may call. patients,
// overview and search are host-only or unneeded: the host already fetched
// the list and the overview.
var ModelTools = []string{"labs", "vitals", "drugs", "io", "scores", "alerts", "timeline", "expand"}

// pinnedArgs are the arguments the host sets on every call; they are removed
// from the schema the model sees.
var pinnedArgs = []string{"patient", "as_of", "with_data"}

// ToolLimitMessage is the tool result once a run has used its tool calls.
const ToolLimitMessage = "工具调用次数已用完，请根据已有信息作答。"

// toolUnavailableMessage replaces a transport failure so the run continues;
// it mirrors gocms' own unavailable error.
const toolUnavailableMessage = "错误[unavailable]：暂时取不到数据，稍后再试。"

// attempt is the state of one model run for one patient.
type attempt struct {
	key      string // unique per attempt; RunRequest.Metadata[attemptKey]
	stay     string
	asOf     string
	runID    string // X-Agent-Run-Id, shared by a patient's attempts
	maxCalls int32
	evidence *Evidence
	calls    atomic.Int32
	counters *toolCounters
	// cache answers a repeated call (same tool, same arguments) without
	// asking gocms again; it is shared by a patient's attempts.
	cache *toolCache
	// compact selects the short tool descriptions (see compact.go).
	compact bool

	// Model calls of this attempt, for per-round statistics.
	rounds atomic.Int32
	// truncated is set when a model call used its whole output budget.
	truncated atomic.Bool
}

// toolsLeft reports whether the attempt may still call a tool.
func (a *attempt) toolsLeft() bool { return a.calls.Load() < a.maxCalls }

type attemptCtxKey struct{}

func withAttempt(ctx context.Context, a *attempt) context.Context {
	return context.WithValue(ctx, attemptCtxKey{}, a)
}

func attemptFrom(ctx context.Context) *attempt {
	a, _ := ctx.Value(attemptCtxKey{}).(*attempt)
	return a
}

// toolCache holds one patient's tool results by tool name and arguments.
type toolCache struct {
	mu      sync.Mutex
	results map[string]*ports.ToolResult
}

func newToolCache() *toolCache { return &toolCache{results: map[string]*ports.ToolResult{}} }

func (c *toolCache) get(key string) (*ports.ToolResult, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.results[key]
	if !ok {
		return nil, false
	}
	clone := *r
	return &clone, true
}

func (c *toolCache) put(key string, r *ports.ToolResult) {
	if c == nil || r == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	clone := *r
	c.results[key] = &clone
}

// toolCounters aggregates tool use over the whole handoff for stats.json.
type toolCounters struct {
	mu        sync.Mutex
	byTool    map[string]int
	limitHits int
	errors    int
	// duplicates are repeated calls answered from the cache.
	duplicates int
	// errorCodes counts failed calls by gocms error code.
	errorCodes map[string]int
}

func newToolCounters() *toolCounters {
	return &toolCounters{byTool: map[string]int{}, errorCodes: map[string]int{}}
}

func (c *toolCounters) duplicate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.duplicates++
}

// add counts one call; errCode is "" for a successful call.
func (c *toolCounters) add(name string, limited bool, errCode string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limited {
		c.limitHits++
		return
	}
	c.byTool[name]++
	if errCode != "" {
		c.errors++
		c.errorCodes[name+":"+errCode]++
	}
}

// pinnedTool wraps a gocms MCP tool for one patient: the model never sees or
// chooses the patient, and every call is counted against the run's limit.
type pinnedTool struct {
	inner   ports.Tool
	spec    ports.ToolSpec
	attempt *attempt
}

func newPinnedTool(inner ports.Tool, a *attempt) (*pinnedTool, error) {
	spec := inner.Spec()
	schema, err := stripSchema(spec.Schema.JSONSchema, pinnedArgs)
	if err != nil {
		return nil, fmt.Errorf("tool %s: %w", spec.Name, err)
	}
	spec.Schema = ports.ToolSchema{JSONSchema: schema}
	if a.compact {
		if spec, err = compactSpec(spec); err != nil {
			return nil, err
		}
	}
	return &pinnedTool{inner: inner, spec: spec, attempt: a}, nil
}

func (t *pinnedTool) Spec() ports.ToolSpec { return t.spec }

func (t *pinnedTool) Execute(ctx context.Context, input json.RawMessage, env ports.ToolEnv) (*ports.ToolResult, error) {
	a := t.attempt
	if a.calls.Add(1) > a.maxCalls {
		a.counters.add(t.spec.Name, true, "")
		return &ports.ToolResult{ForLLM: ToolLimitMessage}, nil
	}
	args, err := pinArgs(input, a.stay, a.asOf)
	if err != nil {
		a.counters.add(t.spec.Name, false, "invalid_argument")
		return &ports.ToolResult{ForLLM: "错误[invalid_argument]：参数必须是 JSON 对象。", IsError: true}, nil
	}
	// pinArgs marshals a map, so keys are sorted and equal arguments give
	// equal keys. A repeat still counts against the limit.
	key := t.spec.Name + " " + string(args)
	if cached, ok := a.cache.get(key); ok {
		a.counters.duplicate()
		return cached, nil
	}
	result, err := t.inner.Execute(WithRunID(ctx, a.runID), args, env)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Transport failures are not cached: a later call may succeed.
		a.counters.add(t.spec.Name, false, "unavailable")
		return &ports.ToolResult{ForLLM: toolUnavailableMessage, IsError: true}, nil
	}
	errCode := ""
	if result.IsError {
		errCode = toolErrorCode(result.ForLLM)
	}
	a.counters.add(t.spec.Name, false, errCode)
	a.evidence.Add(result.ForLLM)
	a.cache.put(key, result)
	return result, nil
}

// pinArgs forces the patient (and as_of, when set) whatever the model sent,
// and drops with_data so results stay text only.
func pinArgs(input json.RawMessage, stay, asOf string) (json.RawMessage, error) {
	args := map[string]any{}
	if len(input) > 0 && string(input) != "null" {
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, err
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	delete(args, "with_data")
	delete(args, "as_of")
	args["patient"] = stay
	if asOf != "" {
		args["as_of"] = asOf
	}
	return json.Marshal(args)
}

// stripSchema removes keys from an object schema's properties and required
// list. additionalProperties is dropped as well, so a model that still sends
// "patient" is not rejected locally; the host overwrites it, and gocms
// validates every other argument itself.
func stripSchema(raw json.RawMessage, keys []string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	drop := map[string]bool{}
	for _, key := range keys {
		drop[key] = true
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		for key := range drop {
			delete(props, key)
		}
	}
	if required, ok := schema["required"].([]any); ok {
		kept := make([]any, 0, len(required))
		for _, name := range required {
			if s, ok := name.(string); ok && drop[s] {
				continue
			}
			kept = append(kept, name)
		}
		if len(kept) == 0 {
			delete(schema, "required")
		} else {
			schema["required"] = kept
		}
	}
	delete(schema, "additionalProperties")
	return json.Marshal(schema)
}

// attemptKey is the RunRequest metadata key naming the attempt.
const attemptKey = "icu_handoff_attempt"

// toolProvider hands each run the gocms tools pinned to its patient. Runs
// are told apart by the attempt key in RunRequest.Metadata.
type toolProvider struct {
	base     []ports.Tool
	attempts sync.Map // string -> *attempt
}

func (p *toolProvider) register(a *attempt) func() {
	p.attempts.Store(a.key, a)
	return func() { p.attempts.Delete(a.key) }
}

func (p *toolProvider) Tools(ctx context.Context, req agentcore.RunRequest) ([]tools.Tool, error) {
	key, _ := req.Metadata[attemptKey].(string)
	value, ok := p.attempts.Load(key)
	if !ok {
		return nil, fmt.Errorf("icu-handoff: no attempt registered for this run")
	}
	a := value.(*attempt)
	out := make([]tools.Tool, 0, len(p.base))
	for _, inner := range p.base {
		pinned, err := newPinnedTool(inner, a)
		if err != nil {
			return nil, err
		}
		out = append(out, pinned)
	}
	return out, nil
}
