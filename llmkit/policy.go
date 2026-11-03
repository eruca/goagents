package llmkit

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Candidate is one routable model/account option.
type Candidate struct {
	Model                     ModelCapability      `json:"model"`
	AccountAlias              string               `json:"account_alias,omitempty"`
	AccountRateLimited        bool                 `json:"account_rate_limited,omitempty"`
	AccountMaxConcurrency     int                  `json:"account_max_concurrency,omitempty"`
	AccountCurrentConcurrency int                  `json:"account_current_concurrency,omitempty"`
	ProviderAvailability      ProviderAvailability `json:"provider_availability,omitempty"`
	ProviderInFlight          int                  `json:"provider_in_flight,omitempty"`
	ProviderMaxConcurrency    int                  `json:"provider_max_concurrency,omitempty"`
	ProviderQuotaRemaining    int                  `json:"provider_quota_remaining,omitempty"`
	ProviderQuotaExhausted    bool                 `json:"provider_quota_exhausted,omitempty"`
	ProviderFailureStreak     int                  `json:"provider_failure_streak,omitempty"`
	ProviderCooldownUntil     time.Time            `json:"provider_cooldown_until,omitempty"`
}

// CandidateScore records why a candidate was accepted, rejected, or ranked.
type CandidateScore struct {
	Alias          string         `json:"alias,omitempty"`
	AccountAlias   string         `json:"account_alias,omitempty"`
	Available      bool           `json:"available"`
	Score          int            `json:"score,omitempty"`
	ScoreBreakdown map[string]int `json:"score_breakdown,omitempty"`
	Reason         string         `json:"reason,omitempty"`
}

// RouteDecision is the deterministic routing result and its traceable
// explanation.
type RouteDecision struct {
	Selected       Candidate        `json:"selected"`
	SelectedAlias  string           `json:"selected_alias,omitempty"`
	Score          int              `json:"score"`
	ScoreBreakdown map[string]int   `json:"score_breakdown,omitempty"`
	Reason         string           `json:"reason,omitempty"`
	Candidates     []CandidateScore `json:"candidates"`
}

// RoutePolicy selects a candidate using hard filters followed by stable
// deterministic scoring.
type RoutePolicy struct {
	Now func() time.Time
}

// Select returns the best candidate for profile, or a clear error when no
// candidate can legally handle the task.
func (p RoutePolicy) Select(profile TaskProfile, candidates []Candidate) (RouteDecision, error) {
	if len(candidates) == 0 {
		return RouteDecision{}, errors.New("no available route candidates: candidate list is empty")
	}

	scored := make([]CandidateScore, 0, len(candidates))
	available := make([]scoredCandidate, 0, len(candidates))

	for _, candidate := range candidates {
		score := CandidateScore{
			Alias:        candidate.Model.Alias,
			AccountAlias: candidate.AccountAlias,
		}

		if reason := p.unavailableReason(profile, candidate); reason != "" {
			score.Reason = reason
			scored = append(scored, score)
			continue
		}

		breakdown := scoreCandidate(profile, candidate)
		total := totalScore(breakdown)
		score.Available = true
		score.Score = total
		score.ScoreBreakdown = breakdown
		score.Reason = selectedReason(candidate, total, breakdown)
		scored = append(scored, score)
		available = append(available, scoredCandidate{
			candidate: candidate,
			score:     score,
		})
	}

	if len(available) == 0 {
		return RouteDecision{}, errors.New("no available route candidates: all candidates were filtered out")
	}

	sort.SliceStable(available, func(i, j int) bool {
		if available[i].score.Score != available[j].score.Score {
			return available[i].score.Score > available[j].score.Score
		}
		return stableCandidateKey(available[i].candidate) < stableCandidateKey(available[j].candidate)
	})

	best := available[0]
	return RouteDecision{
		Selected:       best.candidate,
		SelectedAlias:  best.candidate.Model.Alias,
		Score:          best.score.Score,
		ScoreBreakdown: copyBreakdown(best.score.ScoreBreakdown),
		Reason:         best.score.Reason,
		Candidates:     scored,
	}, nil
}

type scoredCandidate struct {
	candidate Candidate
	score     CandidateScore
}

func stableCandidateKey(candidate Candidate) string {
	return strings.Join([]string{
		candidate.Model.Alias,
		candidate.AccountAlias,
		candidate.Model.Provider,
	}, "\x00")
}

func (p RoutePolicy) unavailableReason(profile TaskProfile, candidate Candidate) string {
	if candidate.AccountRateLimited {
		return "account is rate limited"
	}
	if candidate.AccountMaxConcurrency > 0 && candidate.AccountCurrentConcurrency >= candidate.AccountMaxConcurrency {
		return "account concurrency is full"
	}
	if !candidate.ProviderCooldownUntil.IsZero() && p.now().Before(candidate.ProviderCooldownUntil) {
		return "provider is in cooldown"
	}
	if candidate.ProviderAvailability == ProviderUnavailable && candidate.ProviderCooldownUntil.IsZero() {
		return "provider is unavailable"
	}
	if candidate.ProviderAvailability == ProviderUnavailable && candidate.ProviderInFlight > 0 {
		// Half-open: after cooldown one probe tests recovery; the rest wait
		// for its outcome instead of all hitting a provider that may still be down.
		return "provider recovery probe is in flight"
	}
	if candidate.ProviderQuotaExhausted || candidate.ProviderQuotaRemaining < 0 {
		return "provider quota is exhausted"
	}
	if candidate.ProviderMaxConcurrency > 0 && candidate.ProviderInFlight >= candidate.ProviderMaxConcurrency {
		return "provider concurrency is full"
	}
	if profile.MaxEstimatedCents > 0 && candidate.Model.EstimatedCents > profile.MaxEstimatedCents {
		return "estimated cost exceeds task budget"
	}
	if !candidate.Model.Matches(profile) {
		return "model does not match task requirements"
	}
	return ""
}

func scoreCandidate(profile TaskProfile, candidate Candidate) map[string]int {
	model := candidate.Model
	breakdown := map[string]int{
		"capability":  capabilityScore(profile, model),
		"price":       priceScore(model.PriceClass),
		"local":       localScore(profile, model),
		"latency":     latencyScore(profile, model),
		"reliability": reliabilityScore(model),
		"health":      healthScore(candidate),
	}
	return breakdown
}

func capabilityScore(profile TaskProfile, model ModelCapability) int {
	score := capabilityRank(model.CapabilityLevel) * 6
	if profile.FailureCost == FailureCostHigh {
		score += capabilityRank(model.CapabilityLevel) * 8
	}
	if profile.NeedsReasoning {
		score += capabilityRank(model.CapabilityLevel) * 4
	}
	return score
}

func priceScore(class PriceClass) int {
	switch class {
	case PriceFree:
		return 30
	case PriceLow:
		return 20
	case PriceMedium:
		return 10
	case PriceHigh:
		return 0
	default:
		return 0
	}
}

func localScore(profile TaskProfile, model ModelCapability) int {
	if !model.IsLocal {
		return 0
	}
	switch profile.Privacy {
	case PrivacyLocalPreferred:
		return 15
	case PrivacyLocalOnly:
		return 5
	default:
		return 3
	}
}

// Observed latency thresholds for effectiveLatencyClass.
const (
	fastLatencyMillis   = 2000
	normalLatencyMillis = 10000
)

// effectiveLatencyClass prefers observed latency from model stats over the
// configured class, which may be stale or optimistic.
func effectiveLatencyClass(model ModelCapability) LatencyClass {
	switch {
	case model.RecentLatencyMillis <= 0:
		return model.LatencyClass
	case model.RecentLatencyMillis <= fastLatencyMillis:
		return LatencyFastClass
	case model.RecentLatencyMillis <= normalLatencyMillis:
		return LatencyNormalClass
	default:
		return LatencySlowClass
	}
}

func latencyScore(profile TaskProfile, model ModelCapability) int {
	class := effectiveLatencyClass(model)
	switch profile.Latency {
	case LatencyUrgent:
		switch class {
		case LatencyFastClass:
			return 20
		case LatencyNormalClass:
			return 10
		default:
			return 0
		}
	case LatencyNormal:
		switch class {
		case LatencyFastClass:
			return 10
		case LatencyNormalClass:
			return 5
		default:
			return 0
		}
	default:
		return 0
	}
}

// The reliability prior assumes a 5% failure rate, worth 10 observations:
// weak enough that ten straight failures read as a majority failure rate.
const (
	reliabilityPriorFailures = 0.5
	reliabilityPriorOutcomes = 10.0
)

// reliabilityScore is minus the smoothed failure rate in percent, so it stays
// within [-100, 0]. Observed counts are blended with the prior (a Beta
// posterior mean): a model with no history scores the prior, a few failures do
// not condemn a new model, and a long clean history beats no history.
func reliabilityScore(model ModelCapability) int {
	if model.RecentOutcomes > 0 {
		failures := min(model.RecentFailures, model.RecentOutcomes)
		rate := (failures + reliabilityPriorFailures) / (model.RecentOutcomes + reliabilityPriorOutcomes)
		return -int(math.Round(rate * 100))
	}
	if model.RecentFailureRate > 0 {
		// A rate supplied without counts is used as is.
		return -int(math.Round(math.Min(model.RecentFailureRate, 1) * 100))
	}
	return -int(math.Round(100 * reliabilityPriorFailures / reliabilityPriorOutcomes))
}

func healthScore(candidate Candidate) int {
	score := 0
	switch normalizedAvailability(candidate.ProviderAvailability) {
	case ProviderAvailable:
		score += 2
	case ProviderDegraded:
		score -= 20
	case ProviderUnavailable:
		score -= 100
	}
	if candidate.ProviderFailureStreak > 0 {
		score -= candidate.ProviderFailureStreak * 10
	}
	return score
}

func totalScore(breakdown map[string]int) int {
	total := 0
	for _, value := range breakdown {
		total += value
	}
	return total
}

func selectedReason(candidate Candidate, total int, breakdown map[string]int) string {
	parts := make([]string, 0, len(breakdown))
	keys := make([]string, 0, len(breakdown))
	for key := range breakdown {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, breakdown[key]))
	}
	return fmt.Sprintf("selected %s with score %d (%s)", candidate.Model.Alias, total, strings.Join(parts, ", "))
}

func copyBreakdown(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (p RoutePolicy) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}
