package llmkit

import (
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

// TaskKey binds qualification evidence to one task and one frozen test suite.
type TaskKey struct{ Type, Version, TestSuiteVersion string }

// QualityEvidence contains automatic test results, never transport success.
// Revision includes the model, prompt and generation settings used by the host.
type QualityEvidence struct {
	CorpusSHA256                                      string
	Task                                              TaskKey
	AccountAlias, ModelAlias, Provider, ModelRevision string
	Passed, Samples                                   int
	EvaluatedAt, ExpiresAt                            time.Time
}

func (e QualityEvidence) SuccessRate() float64 {
	if e.Samples <= 0 {
		return 0
	}
	return float64(e.Passed) / float64(e.Samples)
}

type QualityCandidate struct {
	Candidate     Candidate
	ModelRevision string
	Evidence      QualityEvidence
}

// QualityPolicy requires explicit sample, attempt and cumulative estimated-cost
// limits. A zero cost budget admits only explicitly free models.
// MinSuccessRate is the qualification threshold: a model qualifies only when its
// observed test success is strictly greater than it. Zero means
// DefaultMinSuccessRate.
type QualityPolicy struct {
	MinSamples, MaxAttempts, MaxEstimatedCents int
	MinSuccessRate                             float64
}

// DefaultMinSuccessRate is the qualification threshold used when
// QualityPolicy.MinSuccessRate is zero.
const DefaultMinSuccessRate = 0.85

func (p QualityPolicy) minSuccessRate() float64 {
	if p.MinSuccessRate == 0 {
		return DefaultMinSuccessRate
	}
	return p.MinSuccessRate
}

var ErrNoQualifiedModel = errors.New("no model qualifies for the task's automatic tests")
var ErrInvalidQualityConfig = errors.New("invalid quality cascade configuration")

func (k TaskKey) Validate() error {
	for _, s := range []string{k.Type, k.Version, k.TestSuiteVersion} {
		if !validQualityLabel(s) {
			return ErrInvalidQualityConfig
		}
	}
	return nil
}

func validQualityLabel(s string) bool {
	return s != "" && s == strings.TrimSpace(s) && len(s) <= 256 &&
		strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func (p QualityPolicy) Validate() error {
	if p.MinSamples <= 0 || p.MaxAttempts <= 0 || p.MaxEstimatedCents < 0 ||
		math.IsNaN(p.MinSuccessRate) || p.MinSuccessRate < 0 || p.MinSuccessRate >= 1 {
		return ErrInvalidQualityConfig
	}
	return nil
}

func (e QualityEvidence) Validate() error {
	if e.CorpusSHA256 != "" && !validCorpusHash(e.CorpusSHA256) {
		return ErrInvalidQualityConfig
	}
	if e.Task.Validate() != nil || !validQualityLabel(e.AccountAlias) || !validQualityLabel(e.ModelAlias) ||
		!validQualityLabel(e.Provider) || !validQualityLabel(e.ModelRevision) ||
		e.Samples <= 0 || e.Passed < 0 || e.Passed > e.Samples || e.EvaluatedAt.IsZero() || !e.ExpiresAt.After(e.EvaluatedAt) {
		return ErrInvalidQualityConfig
	}
	return nil
}

// Plan preserves capability/health filters and orders qualified candidates by
// cost, then test success. A success rate equal to the threshold is not
// qualified.
func (p QualityPolicy) Plan(task TaskKey, profile TaskProfile, candidates []QualityCandidate, now time.Time) ([]QualityCandidate, error) {
	if p.Validate() != nil || task.Validate() != nil || now.IsZero() {
		return nil, ErrInvalidQualityConfig
	}
	policy := RoutePolicy{Now: func() time.Time { return now }}
	eligible := make([]QualityCandidate, 0, len(candidates))
	for _, c := range candidates {
		e, m := c.Evidence, c.Candidate.Model
		if e.Validate() != nil || e.Task != task || e.ModelRevision != c.ModelRevision ||
			e.AccountAlias != c.Candidate.AccountAlias || e.ModelAlias != m.Alias || e.Provider != m.Provider ||
			e.Samples < p.MinSamples || e.SuccessRate() <= p.minSuccessRate() ||
			e.EvaluatedAt.After(now) || !now.Before(e.ExpiresAt) {
			continue
		}
		// Zero on a paid model is unknown cost, not evidence that the call is free.
		if m.EstimatedCents < 0 || (m.EstimatedCents == 0 && m.PriceClass != PriceFree) || m.EstimatedCents > p.MaxEstimatedCents {
			continue
		}
		if policy.unavailableReason(profile, c.Candidate) != "" {
			continue
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		return nil, ErrNoQualifiedModel
	}
	sort.Slice(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.Candidate.Model.EstimatedCents != b.Candidate.Model.EstimatedCents {
			return a.Candidate.Model.EstimatedCents < b.Candidate.Model.EstimatedCents
		}
		if a.Evidence.SuccessRate() != b.Evidence.SuccessRate() {
			return a.Evidence.SuccessRate() > b.Evidence.SuccessRate()
		}
		return stableCandidateKey(a.Candidate) < stableCandidateKey(b.Candidate)
	})
	return eligible, nil
}
