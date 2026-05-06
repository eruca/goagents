package llmkit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const modelStatsFile = "model-stats.json"

// ModelStats is the generated summary of routing and outcome audit records.
type ModelStats struct {
	GeneratedAt time.Time                  `json:"generated_at"`
	Models      map[string]ModelStatsEntry `json:"models"`
}

// ModelStatsEntry summarizes one task/model/account/provider route bucket.
type ModelStatsEntry struct {
	TaskType          string     `json:"task_type,omitempty"`
	AccountAlias      string     `json:"account_alias,omitempty"`
	ModelAlias        string     `json:"model_alias,omitempty"`
	Provider          string     `json:"provider,omitempty"`
	RouteAttempts     int        `json:"route_attempts"`
	OutcomeCount      int        `json:"outcome_count"`
	PendingOutcomes   int        `json:"pending_outcomes"`
	Successes         int        `json:"successes"`
	Failures          int        `json:"failures"`
	SuccessRate       float64    `json:"success_rate"`
	FailureRate       float64    `json:"failure_rate"`
	AvgLatencyMillis  int        `json:"avg_latency_ms,omitempty"`
	AvgInputTokens    int        `json:"avg_input_tokens,omitempty"`
	AvgOutputTokens   int        `json:"avg_output_tokens,omitempty"`
	AvgEstimatedCents int        `json:"avg_estimated_cents,omitempty"`
	LastSeen          *time.Time `json:"last_seen,omitempty"`
}

// RefreshModelStats rebuilds model-stats.json from route-events.jsonl and
// outcomes.jsonl under home.
func RefreshModelStats(home string) (*ModelStats, error) {
	stats, err := BuildModelStats(home)
	if err != nil {
		return nil, err
	}
	if err := WriteModelStats(home, *stats); err != nil {
		return nil, err
	}
	return stats, nil
}

// BuildModelStats reads audit JSONL files and aggregates route/outcome facts.
func BuildModelStats(home string) (*ModelStats, error) {
	builder := modelStatsBuilder{
		entries: make(map[string]*modelStatsAccumulator),
	}
	if err := readJSONL(filepath.Join(home, routeEventsFile), func(line []byte) error {
		var trace RouteTrace
		if err := json.Unmarshal(line, &trace); err != nil {
			return err
		}
		builder.addRoute(trace)
		return nil
	}); err != nil {
		return nil, err
	}
	latestOutcomes := map[string]TaskOutcome{}
	if err := readJSONL(filepath.Join(home, outcomesFile), func(line []byte) error {
		var outcome TaskOutcome
		if err := json.Unmarshal(line, &outcome); err != nil {
			return err
		}
		if strings.TrimSpace(outcome.RouteID) == "" {
			builder.addOutcome(outcome)
			return nil
		}
		latestOutcomes[outcome.RouteID] = outcome
		return nil
	}); err != nil {
		return nil, err
	}
	for _, outcome := range latestOutcomes {
		builder.addOutcome(outcome)
	}

	return builder.stats(), nil
}

// LoadModelStats reads model-stats.json from home.
func LoadModelStats(home string) (*ModelStats, error) {
	raw, err := os.ReadFile(filepath.Join(home, modelStatsFile))
	if err != nil {
		return nil, err
	}
	var stats ModelStats
	if err := json.Unmarshal(raw, &stats); err != nil {
		return nil, err
	}
	if stats.Models == nil {
		stats.Models = map[string]ModelStatsEntry{}
	}
	return &stats, nil
}

// ApplyModelStats returns a candidate copy enriched with task-specific history.
// Hosts call this explicitly when they want model-stats.json to influence route
// scoring through ModelCapability recent reliability fields.
func ApplyModelStats(stats ModelStats, profile TaskProfile, candidates []Candidate) []Candidate {
	if len(candidates) == 0 || len(stats.Models) == 0 {
		return append([]Candidate(nil), candidates...)
	}

	enriched := make([]Candidate, len(candidates))
	copy(enriched, candidates)
	for i := range enriched {
		entry, ok := stats.Models[modelStatsKey(profile.TaskType, enriched[i].AccountAlias, enriched[i].Model.Alias, enriched[i].Model.Provider)]
		if !ok {
			continue
		}
		if strings.TrimSpace(entry.TaskType) != "" && entry.TaskType != profile.TaskType {
			continue
		}
		enriched[i].Model.RecentFailureCount = entry.Failures
		enriched[i].Model.RecentFailureRate = entry.FailureRate
		enriched[i].Model.RecentLatencyMillis = entry.AvgLatencyMillis
		enriched[i].Model.EstimatedCents = entry.AvgEstimatedCents
	}
	return enriched
}

// WriteModelStats writes model-stats.json under home with private permissions.
func WriteModelStats(home string, stats ModelStats) error {
	clean := filepath.Clean(home)
	if err := ensurePrivateDir(clean); err != nil {
		return err
	}

	path := filepath.Join(clean, modelStatsFile)
	if err := chmodExistingFile(path, 0o600); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

type modelStatsBuilder struct {
	entries map[string]*modelStatsAccumulator
}

type modelStatsAccumulator struct {
	entry              ModelStatsEntry
	latencyTotal       int
	inputTokenTotal    int
	outputTokenTotal   int
	estimatedCentTotal int
}

func (b *modelStatsBuilder) addRoute(trace RouteTrace) {
	key := modelStatsKey(trace.TaskType, trace.AccountAlias, trace.ModelAlias, trace.Provider)
	acc := b.entry(key, trace.TaskType, trace.AccountAlias, trace.ModelAlias, trace.Provider)
	acc.entry.RouteAttempts++
	acc.observe(trace.RecordedAt)
}

func (b *modelStatsBuilder) addOutcome(outcome TaskOutcome) {
	key := modelStatsKey(outcome.TaskType, outcome.AccountAlias, outcome.ModelAlias, outcome.Provider)
	acc := b.entry(key, outcome.TaskType, outcome.AccountAlias, outcome.ModelAlias, outcome.Provider)
	acc.entry.OutcomeCount++
	if outcome.Success {
		acc.entry.Successes++
	} else {
		acc.entry.Failures++
	}
	acc.latencyTotal += outcome.LatencyMillis
	acc.inputTokenTotal += outcome.InputTokens
	acc.outputTokenTotal += outcome.OutputTokens
	acc.estimatedCentTotal += outcome.EstimatedCents
	acc.observe(outcome.RecordedAt)
}

func (b *modelStatsBuilder) entry(key, taskType, accountAlias, modelAlias, provider string) *modelStatsAccumulator {
	if acc, ok := b.entries[key]; ok {
		return acc
	}
	acc := &modelStatsAccumulator{
		entry: ModelStatsEntry{
			TaskType:     taskType,
			AccountAlias: accountAlias,
			ModelAlias:   modelAlias,
			Provider:     provider,
		},
	}
	b.entries[key] = acc
	return acc
}

func (b *modelStatsBuilder) stats() *ModelStats {
	stats := &ModelStats{
		GeneratedAt: time.Now().UTC(),
		Models:      make(map[string]ModelStatsEntry, len(b.entries)),
	}
	for key, acc := range b.entries {
		entry := acc.entry
		entry.PendingOutcomes = entry.RouteAttempts - entry.OutcomeCount
		if entry.PendingOutcomes < 0 {
			entry.PendingOutcomes = 0
		}
		if entry.OutcomeCount > 0 {
			entry.SuccessRate = float64(entry.Successes) / float64(entry.OutcomeCount)
			entry.FailureRate = float64(entry.Failures) / float64(entry.OutcomeCount)
			entry.AvgLatencyMillis = acc.latencyTotal / entry.OutcomeCount
			entry.AvgInputTokens = acc.inputTokenTotal / entry.OutcomeCount
			entry.AvgOutputTokens = acc.outputTokenTotal / entry.OutcomeCount
			entry.AvgEstimatedCents = acc.estimatedCentTotal / entry.OutcomeCount
		}
		stats.Models[key] = entry
	}
	return stats
}

func (a *modelStatsAccumulator) observe(seen time.Time) {
	if seen.IsZero() {
		return
	}
	if a.entry.LastSeen == nil || seen.After(*a.entry.LastSeen) {
		seen = seen.UTC()
		a.entry.LastSeen = &seen
	}
}

func modelStatsKey(taskType, accountAlias, modelAlias, provider string) string {
	return taskType + "|" + accountAlias + "|" + modelAlias + "|" + provider
}

func readJSONL(path string, handle func([]byte) error) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := handle(line); err != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
	}
	return scanner.Err()
}
