package memorykit

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

type LifecycleStore interface {
	Create(context.Context, CreateRequest) (Memory, error)
	Get(context.Context, Scope, string) (Memory, error)
	Sources(context.Context, Scope, string) ([]Source, error)
	List(context.Context, ListQuery) ([]Memory, error)
	Activate(context.Context, VersionedCommand) (Memory, error)
	Correct(context.Context, CorrectRequest) (Memory, error)
	Dismiss(context.Context, VersionedCommand) (Memory, error)
	Forget(context.Context, VersionedCommand) (Memory, error)
	Erase(context.Context, VersionedCommand) error
	Revisions(context.Context, RevisionQuery) ([]Revision, error)
}

type RecallStore interface {
	SearchCandidates(context.Context, CandidateQuery) (CandidateSet, error)
}

type Channel string

const (
	ChannelExact    Channel = "exact"
	ChannelFullText Channel = "full_text"
	ChannelVector   Channel = "vector"
)

type Candidate struct {
	Memory     Memory
	Sources    []Source
	Channel    Channel
	Rank       int
	Similarity float64
}

type CandidateQuery struct {
	Scope               Scope
	Text                string
	Keys                []string
	Kinds               []Kind
	QueryVector         []float32
	Now                 time.Time
	ExactLimit          int
	FullTextLimit       int
	VectorLimit         int
	MinVectorSimilarity float64
	EmbeddingProfileID  string
	EmbeddingDimensions int
}

type CandidateSet struct {
	Exact, FullText, Vector []Candidate
}

type Store interface {
	LifecycleStore
	RecallStore
}

// ValidateCandidateQuery validates both shared filters and channel-specific inputs.
func ValidateCandidateQuery(query CandidateQuery, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if err := query.Scope.Validate(); err != nil {
		return err
	}
	channelLimits := []int{query.ExactLimit, query.FullTextLimit, query.VectorLimit}
	positive := false
	for _, limit := range channelLimits {
		if limit < 0 || limit > limits.MaxListItems {
			return fmt.Errorf("%w: invalid candidate channel limit", ErrInvalidMemory)
		}
		positive = positive || limit > 0
	}
	if !positive || utf8.RuneCountInString(query.Text) > limits.MaxContentRunes || len(query.Keys) > limits.MaxListItems {
		return fmt.Errorf("%w: invalid candidate query", ErrInvalidMemory)
	}

	seenKeys := make(map[string]struct{}, len(query.Keys))
	for _, key := range query.Keys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%w: blank candidate key", ErrInvalidMemory)
		}
		if _, exists := seenKeys[key]; exists {
			return fmt.Errorf("%w: duplicate candidate key", ErrInvalidMemory)
		}
		seenKeys[key] = struct{}{}
	}
	seenKinds := make(map[Kind]struct{}, len(query.Kinds))
	for _, kind := range query.Kinds {
		if !kind.IsValid() {
			return fmt.Errorf("%w: invalid candidate kind", ErrInvalidMemory)
		}
		if _, exists := seenKinds[kind]; exists {
			return fmt.Errorf("%w: duplicate candidate kind", ErrInvalidMemory)
		}
		seenKinds[kind] = struct{}{}
	}

	if query.VectorLimit == 0 {
		if len(query.QueryVector) != 0 || query.EmbeddingProfileID != "" || query.EmbeddingDimensions != 0 || query.MinVectorSimilarity != 0 {
			return fmt.Errorf("%w: vector fields require vector search", ErrInvalidMemory)
		}
		return nil
	}
	if strings.TrimSpace(query.EmbeddingProfileID) == "" || query.EmbeddingProfileID != strings.TrimSpace(query.EmbeddingProfileID) ||
		query.EmbeddingDimensions <= 0 || len(query.QueryVector) != query.EmbeddingDimensions ||
		math.IsNaN(query.MinVectorSimilarity) || math.IsInf(query.MinVectorSimilarity, 0) ||
		query.MinVectorSimilarity < 0 || query.MinVectorSimilarity > 1 {
		return fmt.Errorf("%w: invalid vector query", ErrInvalidMemory)
	}
	for _, value := range query.QueryVector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("%w: vector values must be finite", ErrInvalidMemory)
		}
	}
	return nil
}
