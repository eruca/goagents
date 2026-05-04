package llmkit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	routeEventsFile = "route-events.jsonl"
	outcomesFile    = "outcomes.jsonl"
)

var (
	credentialTokenPattern = regexp.MustCompile(`(?i)secret[_-]?key|bearer`)
	skTokenPattern         = regexp.MustCompile(`(?i)sk-[a-z0-9_-]{8,}`)
)

// RouteTrace is the allowlisted audit record for a routing decision. It stores
// aliases and routing explanations only; host-owned secrets must not be added.
type RouteTrace struct {
	RouteID               string         `json:"route_id,omitempty"`
	TaskID                string         `json:"task_id,omitempty"`
	Attempt               int            `json:"attempt,omitempty"`
	RecordedAt            time.Time      `json:"recorded_at,omitempty"`
	TaskType              string         `json:"task_type,omitempty"`
	AccountAlias          string         `json:"account_alias,omitempty"`
	ModelAlias            string         `json:"model_alias,omitempty"`
	Provider              string         `json:"provider,omitempty"`
	Selected              bool           `json:"selected,omitempty"`
	Reason                string         `json:"reason,omitempty"`
	Score                 int            `json:"score,omitempty"`
	ScoreBreakdown        map[string]int `json:"score_breakdown,omitempty"`
	CandidateModelAliases []string       `json:"candidate_model_aliases,omitempty"`
}

// TaskOutcome is the allowlisted audit record for the result of an LLM task.
// It records outcome metadata, not prompts, responses, API keys, or headers.
type TaskOutcome struct {
	RouteID        string    `json:"route_id,omitempty"`
	TaskID         string    `json:"task_id,omitempty"`
	Attempt        int       `json:"attempt,omitempty"`
	RecordedAt     time.Time `json:"recorded_at,omitempty"`
	TaskType       string    `json:"task_type,omitempty"`
	AccountAlias   string    `json:"account_alias,omitempty"`
	ModelAlias     string    `json:"model_alias,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Success        bool      `json:"success"`
	ErrorCode      string    `json:"error_code,omitempty"`
	LatencyMillis  int       `json:"latency_ms,omitempty"`
	InputTokens    int       `json:"input_tokens,omitempty"`
	OutputTokens   int       `json:"output_tokens,omitempty"`
	EstimatedCents int       `json:"estimated_cents,omitempty"`
}

// Recorder persists routing and outcome audit records.
type Recorder interface {
	RecordRoute(context.Context, RouteTrace) error
	RecordOutcome(context.Context, TaskOutcome) error
}

// JSONLRecorder appends one JSON object per line under its configured home.
type JSONLRecorder struct {
	home string

	mu sync.Mutex
}

// NewJSONLRecorder creates a JSONL recorder and ensures its target directory
// exists. It writes route-events.jsonl and outcomes.jsonl under home.
func NewJSONLRecorder(home string) (*JSONLRecorder, error) {
	clean := filepath.Clean(home)
	if err := ensurePrivateDir(clean); err != nil {
		return nil, err
	}
	return &JSONLRecorder{home: clean}, nil
}

// RecordRoute appends a sanitized routing trace to route-events.jsonl.
func (r *JSONLRecorder) RecordRoute(ctx context.Context, trace RouteTrace) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.appendJSONL(ctx, routeEventsFile, sanitizeRouteTrace(trace))
}

// RecordOutcome appends a sanitized task outcome to outcomes.jsonl.
func (r *JSONLRecorder) RecordOutcome(ctx context.Context, outcome TaskOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.appendJSONL(ctx, outcomesFile, sanitizeTaskOutcome(outcome))
}

func (r *JSONLRecorder) appendJSONL(ctx context.Context, name string, record any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensurePrivateDir(r.home); err != nil {
		return err
	}

	path := filepath.Join(r.home, name)
	if err := chmodExistingFile(path, 0o600); err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}

	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return nil
}

func sanitizeRouteTrace(trace RouteTrace) RouteTrace {
	if trace.RecordedAt.IsZero() {
		trace.RecordedAt = time.Now().UTC()
	}
	trace.RouteID = sanitizeAuditString(trace.RouteID)
	trace.TaskID = sanitizeAuditString(trace.TaskID)
	trace.TaskType = sanitizeAuditString(trace.TaskType)
	trace.AccountAlias = sanitizeAuditString(trace.AccountAlias)
	trace.ModelAlias = sanitizeAuditString(trace.ModelAlias)
	trace.Provider = sanitizeAuditString(trace.Provider)
	trace.Reason = sanitizeAuditString(trace.Reason)
	trace.CandidateModelAliases = sanitizeAuditStrings(trace.CandidateModelAliases)
	trace.ScoreBreakdown = copyBreakdown(trace.ScoreBreakdown)
	return trace
}

func sanitizeTaskOutcome(outcome TaskOutcome) TaskOutcome {
	if outcome.RecordedAt.IsZero() {
		outcome.RecordedAt = time.Now().UTC()
	}
	outcome.RouteID = sanitizeAuditString(outcome.RouteID)
	outcome.TaskID = sanitizeAuditString(outcome.TaskID)
	outcome.TaskType = sanitizeAuditString(outcome.TaskType)
	outcome.AccountAlias = sanitizeAuditString(outcome.AccountAlias)
	outcome.ModelAlias = sanitizeAuditString(outcome.ModelAlias)
	outcome.Provider = sanitizeAuditString(outcome.Provider)
	outcome.ErrorCode = sanitizeAuditString(outcome.ErrorCode)
	return outcome
}

func sanitizeAuditStrings(values []string) []string {
	if values == nil {
		return nil
	}
	copied := make([]string, len(values))
	for i, value := range values {
		copied[i] = sanitizeAuditString(value)
	}
	return copied
}

func sanitizeAuditString(value string) string {
	value = skTokenPattern.ReplaceAllString(value, "[redacted]")
	value = credentialTokenPattern.ReplaceAllString(value, "credential")
	return strings.TrimSpace(value)
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func chmodExistingFile(path string, mode os.FileMode) error {
	if err := os.Chmod(path, mode); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
