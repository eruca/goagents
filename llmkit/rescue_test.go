package llmkit

import (
	"strings"
	"testing"
)

func TestRescueEvidenceMeasuresResidualRatherThanOverallSuccess(t *testing.T) {
	from := qualityCandidate("cheap", 1, 90).Evidence
	to := qualityCandidate("strong", 3, 96).Evidence
	from.CorpusSHA256 = strings.Repeat("a", 64)
	to.CorpusSHA256 = from.CorpusSHA256
	e := RescueEvidence{Task: qualityTask, CorpusSHA256: from.CorpusSHA256, From: from.ModelKey(), To: to.ModelKey(), Failed: 10, Rescued: 9, EvaluatedAt: from.EvaluatedAt, ExpiresAt: from.ExpiresAt}
	policy := RescuePolicy{MinSamples: 5, MinSuccessRate: 0.9}
	if !e.Qualifies(from, to, policy, qualityNow) || e.SuccessRate() != 0.9 {
		t.Fatal("valid residual evidence rejected")
	}
	for _, kind := range []string{"rate", "samples", "revision", "corpus", "denominator", "expiry", "task", "impossible"} {
		t.Run(kind, func(t *testing.T) {
			broken := e
			switch kind {
			case "rate":
				broken.Rescued = 8
			case "samples":
				broken.Failed = 4
				broken.Rescued = 4
			case "revision":
				broken.To.Revision = "old"
			case "corpus":
				broken.CorpusSHA256 = strings.Repeat("b", 64)
			case "denominator":
				broken.Failed = 11
			case "expiry":
				broken.ExpiresAt = qualityNow
			case "task":
				broken.Task.Version = "old"
			case "impossible":
				broken.Rescued = 11
			}
			if broken.Qualifies(from, to, policy, qualityNow) {
				t.Fatal("invalid residual evidence accepted")
			}
		})
	}
}

func TestRescuePolicyRejectsMissingAndInvalidLimits(t *testing.T) {
	for _, p := range []RescuePolicy{{}, {MinSamples: 1, MinSuccessRate: 0}, {MinSamples: 1, MinSuccessRate: 1.1}, {MinSamples: -1, MinSuccessRate: 0.5}} {
		if p.Validate() == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}
