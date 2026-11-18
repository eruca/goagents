package llmkit

import (
	"encoding/hex"
	"math"
	"time"
)

// QualityModelKey identifies the exact provider configuration evaluated.
type QualityModelKey struct{ AccountAlias, ModelAlias, Provider, Revision string }

func (e QualityEvidence) ModelKey() QualityModelKey {
	return QualityModelKey{e.AccountAlias, e.ModelAlias, e.Provider, e.ModelRevision}
}

// RescueEvidence measures To on the subset where From failed, using the same
// frozen corpus. Zero failures means rescue ability was not measured.
type RescueEvidence struct {
	Task                   TaskKey
	CorpusSHA256           string
	From, To               QualityModelKey
	Failed, Rescued        int
	EvaluatedAt, ExpiresAt time.Time
}

func (e RescueEvidence) SuccessRate() float64 {
	if e.Failed == 0 {
		return 0
	}
	return float64(e.Rescued) / float64(e.Failed)
}

type RescuePolicy struct {
	MinSamples     int
	MinSuccessRate float64
}

func (p RescuePolicy) Validate() error {
	if p.MinSamples <= 0 || math.IsNaN(p.MinSuccessRate) || p.MinSuccessRate <= 0 || p.MinSuccessRate > 1 {
		return ErrInvalidQualityConfig
	}
	return nil
}

func (e RescueEvidence) Validate() error {
	if e.Task.Validate() != nil || !validCorpusHash(e.CorpusSHA256) || !e.From.valid() || !e.To.valid() || e.From == e.To ||
		e.Failed < 0 || e.Rescued < 0 || e.Rescued > e.Failed || e.EvaluatedAt.IsZero() || !e.ExpiresAt.After(e.EvaluatedAt) {
		return ErrInvalidQualityConfig
	}
	return nil
}

func (k QualityModelKey) valid() bool {
	return validQualityLabel(k.AccountAlias) && validQualityLabel(k.ModelAlias) && validQualityLabel(k.Provider) && validQualityLabel(k.Revision)
}

func validCorpusHash(s string) bool {
	b, err := hex.DecodeString(s)
	return len(s) == 64 && err == nil && len(b) == 32
}

// Qualifies rejects absent, stale, mismatched or insufficient paired evidence.
func (e RescueEvidence) Qualifies(from, to QualityEvidence, p RescuePolicy, now time.Time) bool {
	return p.Validate() == nil && e.Validate() == nil && from.Validate() == nil && to.Validate() == nil &&
		!now.IsZero() && !e.EvaluatedAt.After(now) && now.Before(e.ExpiresAt) &&
		!from.EvaluatedAt.After(now) && !to.EvaluatedAt.After(now) &&
		now.Before(from.ExpiresAt) && now.Before(to.ExpiresAt) &&
		e.Task == from.Task && e.Task == to.Task && e.From == from.ModelKey() && e.To == to.ModelKey() &&
		e.CorpusSHA256 == from.CorpusSHA256 && e.CorpusSHA256 == to.CorpusSHA256 &&
		from.Samples == to.Samples && e.Failed == from.Samples-from.Passed && e.Rescued <= to.Passed &&
		e.Failed >= p.MinSamples && e.SuccessRate() >= p.MinSuccessRate
}
