package memorykit

import (
	"context"
	"time"
)

// CandidateDraft is untrusted extracted content that has not been persisted or activated.
type CandidateDraft struct {
	Kind       Kind      `json:"kind"`
	Key        string    `json:"key"`
	Content    string    `json:"content"`
	ValidFrom  time.Time `json:"valid_from,omitempty"`
	ValidUntil time.Time `json:"valid_until,omitempty"`
	Importance int       `json:"importance,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
}

type ExtractionRequest struct {
	Scope                            Scope
	Source                           Source
	SourceAgentID, ExtractorID, Text string
	Now                              time.Time
}

type CandidateExtractor interface {
	Extract(context.Context, ExtractionRequest) ([]CandidateDraft, error)
}

type ContentValidator interface {
	ValidateMemoryContent(context.Context, string) error
}

// ValidateCandidateDraft delegates field validation to the create contract while
// fixing the lifecycle state to candidate and using validation-only identities.
func ValidateCandidateDraft(draft CandidateDraft, limits Limits) error {
	return ValidateCreateRequest(CreateRequest{
		ID:         "00000000-0000-4000-8000-000000000001",
		Scope:      Scope{TenantID: "validation", SubjectType: SubjectProject, SubjectID: "validation"},
		Kind:       draft.Kind,
		Key:        draft.Key,
		Status:     StatusCandidate,
		Content:    draft.Content,
		ValidFrom:  draft.ValidFrom,
		ValidUntil: draft.ValidUntil,
		Importance: draft.Importance,
		Confidence: draft.Confidence,
	}, limits)
}
