package memorykit

import (
	"errors"
	"testing"
	"time"
)

func TestBacklogQueriesRejectInvalidInputs(t *testing.T) {
	limits := backlogTestLimits()
	for name, query := range map[string]EmbeddingBacklogQuery{
		"blank profile":   {},
		"padded profile":  {ProfileID: " profile-a "},
		"control profile": {ProfileID: "profile\n"},
	} {
		t.Run("embedding/"+name, func(t *testing.T) {
			if err := ValidateEmbeddingBacklogQuery(query, limits); !errors.Is(err, ErrInvalidMemory) {
				t.Fatalf("ValidateEmbeddingBacklogQuery() = %v, want ErrInvalidMemory", err)
			}
		})
	}

	for name, query := range map[string]ExtractionBacklogQuery{
		"zero time":     {MaxAttempts: 3},
		"zero attempts": {Now: time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run("extraction/"+name, func(t *testing.T) {
			if err := ValidateExtractionBacklogQuery(query); !errors.Is(err, ErrInvalidMemory) {
				t.Fatalf("ValidateExtractionBacklogQuery() = %v, want ErrInvalidMemory", err)
			}
		})
	}
}

func TestBacklogQueriesAcceptValidInputs(t *testing.T) {
	if err := ValidateEmbeddingBacklogQuery(
		EmbeddingBacklogQuery{ProfileID: "profile-a"},
		backlogTestLimits(),
	); err != nil {
		t.Fatalf("ValidateEmbeddingBacklogQuery() = %v", err)
	}
	if err := ValidateExtractionBacklogQuery(ExtractionBacklogQuery{
		Now:         time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		MaxAttempts: 3,
	}); err != nil {
		t.Fatalf("ValidateExtractionBacklogQuery() = %v", err)
	}
}

func backlogTestLimits() Limits {
	return Limits{
		Version:             "project-memory-v1",
		MaxKeyRunes:         128,
		MaxContentRunes:     4096,
		MaxMetadataRunes:    512,
		MaxSourcesPerMemory: 16,
		MaxListItems:        100,
	}
}
