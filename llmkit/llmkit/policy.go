package llmkit

import (
	"errors"
	"fmt"
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

func latencyScore(profile TaskProfile, model ModelCapability) int {
	switch profile.Latency {
	case LatencyUrgent:
		switch model.LatencyClass {
		case LatencyFastClass:
			return 20
		case LatencyNormalClass:
			return 10
		default:
			return 0
		}
	case LatencyNormal:
		switch model.LatencyClass {
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

func reliabilityScore(model ModelCapability) int {
	score := 0
	if model.RecentFailureCount > 0 {
		score -= model.RecentFailureCount * 2
	}
	if model.RecentFailureRate > 0 {
		score -= int(model.RecentFailureRate * 100)
	}
	return score
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
