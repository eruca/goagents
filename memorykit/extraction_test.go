package memorykit

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestValidateCandidateDraftReusesCreateValidation(t *testing.T) {
	limits := extractionTestLimits()
	valid := CandidateDraft{
		Kind: KindLesson, Key: "testing.pgvector", Content: "Run the real integration gate",
		ValidFrom: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC), Importance: 10, Confidence: 0.82,
	}
	if err := ValidateCandidateDraft(valid, limits); err != nil {
		t.Fatalf("ValidateCandidateDraft(valid) = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*CandidateDraft)
	}{
		{name: "kind", mutate: func(d *CandidateDraft) { d.Kind = Kind("other") }},
		{name: "key", mutate: func(d *CandidateDraft) { d.Key = "" }},
		{name: "content", mutate: func(d *CandidateDraft) { d.Content = "012345678901234567890123456789012345678901234567890" }},
		{name: "time range", mutate: func(d *CandidateDraft) { d.ValidUntil = d.ValidFrom }},
		{name: "importance", mutate: func(d *CandidateDraft) { d.Importance = 101 }},
		{name: "confidence", mutate: func(d *CandidateDraft) { d.Confidence = math.NaN() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			draft := valid
			test.mutate(&draft)
			if err := ValidateCandidateDraft(draft, limits); !errors.Is(err, ErrInvalidMemory) {
				t.Fatalf("ValidateCandidateDraft() = %v, want ErrInvalidMemory", err)
			}
		})
	}
}

func extractionTestLimits() Limits {
	return Limits{
		Version: "test-v1", MaxKeyRunes: 40, MaxContentRunes: 50,
		MaxMetadataRunes: 100, MaxSourcesPerMemory: 4, MaxListItems: 10,
	}
}

var _ CandidateExtractor = candidateExtractorFunc(nil)

type candidateExtractorFunc func(context.Context, ExtractionRequest) ([]CandidateDraft, error)

func (f candidateExtractorFunc) Extract(ctx context.Context, request ExtractionRequest) ([]CandidateDraft, error) {
	return f(ctx, request)
}
