package llmkit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
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
	TaskType        string  `json:"task_type,omitempty"`
	AccountAlias    string  `json:"account_alias,omitempty"`
	ModelAlias      string  `json:"model_alias,omitempty"`
	Provider        string  `json:"provider,omitempty"`
	RouteAttempts   int     `json:"route_attempts"`
	OutcomeCount    int     `json:"outcome_count"`
	PendingOutcomes int     `json:"pending_outcomes"`
	Successes       int     `json:"successes"`
	Failures        int     `json:"failures"`
	SuccessRate     float64 `json:"success_rate"`
	FailureRate     float64 `json:"failure_rate"`
	// RecentOutcomes and RecentFailures weight each outcome by its age,
	// halving every ModelStatsHalfLife, so routing reacts to how a model
	// behaves now rather than over its whole history.
	RecentOutcomes    float64    `json:"recent_outcomes"`
	RecentFailures    float64    `json:"recent_failures"`
	AvgLatencyMillis  int        `json:"avg_latency_ms,omitempty"`
	AvgInputTokens    int        `json:"avg_input_tokens,omitempty"`
	AvgOutputTokens   int        `json:"avg_output_tokens,omitempty"`
	AvgEstimatedCents int        `json:"avg_estimated_cents,omitempty"`
	LastSeen          *time.Time `json:"last_seen,omitempty"`
}

// ModelStatsHalfLife is the age at which an outcome counts half as much as a
// fresh one in RecentOutcomes and RecentFailures.
const ModelStatsHalfLife = 7 * 24 * time.Hour

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
		now:     time.Now().UTC(),
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
		// A later record for the same attempt, such as a business outcome,
		// replaces the provider outcome; fallback attempts are kept apart.
		latestOutcomes[outcomeKey(outcome.RouteID, outcome.Attempt)] = outcome
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
		enriched[i].Model.RecentOutcomes = entry.RecentOutcomes
		enriched[i].Model.RecentFailures = entry.RecentFailures
		if entry.RecentOutcomes == 0 && entry.OutcomeCount > 0 {
			// Stats written without decayed counts: use the raw totals. Decay
			// never reaches exactly zero, so zero means "not computed".
			enriched[i].Model.RecentOutcomes = float64(entry.OutcomeCount)
			enriched[i].Model.RecentFailures = float64(entry.Failures)
		}
		enriched[i].Model.RecentFailureRate = entry.FailureRate
		// Without observed samples the configured values stay authoritative.
		if entry.AvgLatencyMillis > 0 {
			enriched[i].Model.RecentLatencyMillis = entry.AvgLatencyMillis
		}
		if entry.AvgEstimatedCents > 0 {
			enriched[i].Model.EstimatedCents = entry.AvgEstimatedCents
		}
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
	now     time.Time
	entries map[string]*modelStatsAccumulator
}

type modelStatsAccumulator struct {
	entry              ModelStatsEntry
	latencyTotal       int
	latencySamples     int
	inputTokenTotal    int
	outputTokenTotal   int
	estimatedCentTotal int
	costSamples        int
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
	weight := b.decayWeight(outcome.RecordedAt)
	acc.entry.RecentOutcomes += weight
	if outcome.Success {
		acc.entry.Successes++
	} else {
		acc.entry.Failures++
		acc.entry.RecentFailures += weight
	}
	// Outcomes that did not report latency or cost must not pull averages toward zero.
	if outcome.LatencyMillis > 0 {
		acc.latencyTotal += outcome.LatencyMillis
		acc.latencySamples++
	}
	acc.inputTokenTotal += outcome.InputTokens
	acc.outputTokenTotal += outcome.OutputTokens
	if outcome.EstimatedCents > 0 {
		acc.estimatedCentTotal += outcome.EstimatedCents
		acc.costSamples++
	}
	acc.observe(outcome.RecordedAt)
}

// decayWeight is 2^(-age/ModelStatsHalfLife). An outcome without a timestamp
// or from the future counts fully.
func (b *modelStatsBuilder) decayWeight(recorded time.Time) float64 {
	if recorded.IsZero() || !recorded.Before(b.now) {
		return 1
	}
	return math.Exp2(-float64(b.now.Sub(recorded)) / float64(ModelStatsHalfLife))
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
		GeneratedAt: b.now,
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
			entry.AvgInputTokens = acc.inputTokenTotal / entry.OutcomeCount
			entry.AvgOutputTokens = acc.outputTokenTotal / entry.OutcomeCount
		}
		if acc.latencySamples > 0 {
			entry.AvgLatencyMillis = acc.latencyTotal / acc.latencySamples
		}
		if acc.costSamples > 0 {
			entry.AvgEstimatedCents = acc.estimatedCentTotal / acc.costSamples
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

func outcomeKey(routeID string, attempt int) string {
	return routeID + "|" + strconv.Itoa(attempt)
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
