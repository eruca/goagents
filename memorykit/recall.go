package memorykit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type RecallPolicy struct {
	Version                                string
	ExactLimit, FullTextLimit, VectorLimit int
	RRFK                                   float64
	MinVectorSimilarity                    float64
	MaxItems, MaxTokens                    int
	MaxQueryRunes, MaxKeys                 int
	Deadline                               time.Duration
	EmbeddingProfileID                     string
	EmbeddingDimensions                    int
}

type EmbedRequest struct {
	ProfileID string
	Texts     []string
}

type Embedder interface {
	Embed(context.Context, EmbedRequest) ([][]float32, error)
}

type TokenCounter func(string) int

type RecallRequest struct {
	Scope Scope
	Text  string
	Keys  []string
	Kinds []Kind
	Now   time.Time
}

type RecallItem struct {
	Memory  Memory
	Sources []Source
}

type RecallResult struct {
	Items            []RecallItem
	DegradedChannels []Channel
	PolicyVersion    string
}

type RecallConfig struct {
	Store       RecallStore
	Embedder    Embedder
	CountTokens TokenCounter
	Policy      RecallPolicy
}

type Recaller struct {
	store       RecallStore
	embedder    Embedder
	countTokens TokenCounter
	policy      RecallPolicy
}

type ChannelError struct {
	Channel Channel
	Err     error
}

func (e *ChannelError) Error() string {
	return "memory " + string(e.Channel) + " channel: " + e.Err.Error()
}

func (e *ChannelError) Unwrap() error { return e.Err }

// Validate rejects partial policies so recall behavior is fixed at startup.
func (p RecallPolicy) Validate() error {
	if strings.TrimSpace(p.Version) == "" || p.Version != strings.TrimSpace(p.Version) ||
		p.ExactLimit < 0 || p.FullTextLimit < 0 || p.VectorLimit < 0 ||
		(p.ExactLimit == 0 && p.FullTextLimit == 0 && p.VectorLimit == 0) ||
		math.IsNaN(p.RRFK) || math.IsInf(p.RRFK, 0) || p.RRFK <= 0 ||
		math.IsNaN(p.MinVectorSimilarity) || math.IsInf(p.MinVectorSimilarity, 0) ||
		p.MinVectorSimilarity < 0 || p.MinVectorSimilarity > 1 ||
		p.MaxItems <= 0 || p.MaxTokens <= 0 || p.MaxQueryRunes <= 0 || p.MaxKeys <= 0 ||
		p.Deadline <= 0 || strings.TrimSpace(p.EmbeddingProfileID) == "" ||
		p.EmbeddingProfileID != strings.TrimSpace(p.EmbeddingProfileID) || p.EmbeddingDimensions <= 0 {
		return fmt.Errorf("%w: invalid recall policy", ErrInvalidMemory)
	}
	return nil
}

func NewRecaller(config RecallConfig) (*Recaller, error) {
	if err := config.Policy.Validate(); err != nil {
		return nil, err
	}
	if config.Store == nil || config.CountTokens == nil || (config.Policy.VectorLimit > 0 && config.Embedder == nil) {
		return nil, fmt.Errorf("%w: incomplete recall configuration", ErrInvalidMemory)
	}
	return &Recaller{
		store: config.Store, embedder: config.Embedder,
		countTokens: config.CountTokens, policy: config.Policy,
	}, nil
}

func (r *Recaller) Recall(ctx context.Context, request RecallRequest) (RecallResult, error) {
	result := RecallResult{
		Items:            make([]RecallItem, 0, r.policy.MaxItems),
		DegradedChannels: make([]Channel, 0, 1),
		PolicyVersion:    r.policy.Version,
	}
	request, err := r.validateAndCopyRequest(request)
	if err != nil {
		return RecallResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return RecallResult{}, err
	}

	// A single child context makes embedding and candidate retrieval share one budget.
	recallCtx, cancel := context.WithTimeout(ctx, r.policy.Deadline)
	defer cancel()

	query := CandidateQuery{
		Scope: request.Scope, Text: request.Text,
		Keys: append([]string(nil), request.Keys...), Kinds: append([]Kind(nil), request.Kinds...),
		Now: request.Now, ExactLimit: r.policy.ExactLimit, FullTextLimit: r.policy.FullTextLimit,
	}
	if strings.TrimSpace(request.Text) == "" {
		query.FullTextLimit = 0
	}
	if r.policy.VectorLimit > 0 && strings.TrimSpace(request.Text) != "" {
		vectors, embedErr := r.embedder.Embed(recallCtx, EmbedRequest{
			ProfileID: r.policy.EmbeddingProfileID,
			Texts:     []string{request.Text},
		})
		if err := recallCtx.Err(); err != nil {
			return RecallResult{}, err
		}
		if embedErr != nil {
			if !IsRecoverable(embedErr) {
				return RecallResult{}, embedErr
			}
			result.DegradedChannels = append(result.DegradedChannels, ChannelVector)
		} else {
			vector, validateErr := validateEmbeddingResult(vectors, r.policy.EmbeddingDimensions)
			if validateErr != nil {
				return RecallResult{}, validateErr
			}
			query.QueryVector = vector
			query.VectorLimit = r.policy.VectorLimit
			query.MinVectorSimilarity = r.policy.MinVectorSimilarity
			query.EmbeddingProfileID = r.policy.EmbeddingProfileID
			query.EmbeddingDimensions = r.policy.EmbeddingDimensions
		}
	}
	if query.ExactLimit == 0 && query.FullTextLimit == 0 && query.VectorLimit == 0 {
		return copyRecallResult(result), nil
	}

	set, searchErr := r.store.SearchCandidates(recallCtx, query)
	if err := recallCtx.Err(); err != nil {
		return RecallResult{}, err
	}
	if searchErr != nil {
		var channelErr *ChannelError
		if !errors.As(searchErr, &channelErr) || channelErr.Channel != ChannelVector || !IsRecoverable(searchErr) {
			return RecallResult{}, searchErr
		}
		if !containsChannel(result.DegradedChannels, ChannelVector) {
			result.DegradedChannels = append(result.DegradedChannels, ChannelVector)
		}
		set.Vector = nil
	}

	fused, err := r.fuse(set, request)
	if err != nil {
		return RecallResult{}, err
	}
	remainingTokens := r.policy.MaxTokens
	for _, item := range fused {
		if len(result.Items) == r.policy.MaxItems {
			break
		}
		payload, err := canonicalRecallPayload(item)
		if err != nil {
			return RecallResult{}, err
		}
		cost := r.countTokens(payload)
		if cost < 0 {
			return RecallResult{}, fmt.Errorf("%w: token counter returned a negative value", ErrInvalidMemory)
		}
		if cost > remainingTokens {
			continue
		}
		remainingTokens -= cost
		result.Items = append(result.Items, copyRecallItem(item))
	}
	return copyRecallResult(result), nil
}

func (r *Recaller) validateAndCopyRequest(request RecallRequest) (RecallRequest, error) {
	if err := request.Scope.Validate(); err != nil {
		return RecallRequest{}, err
	}
	if (strings.TrimSpace(request.Text) == "" && len(request.Keys) == 0) ||
		utf8.RuneCountInString(request.Text) > r.policy.MaxQueryRunes || len(request.Keys) > r.policy.MaxKeys {
		return RecallRequest{}, fmt.Errorf("%w: invalid recall request", ErrInvalidMemory)
	}
	seenKeys := make(map[string]struct{}, len(request.Keys))
	for _, key := range request.Keys {
		if strings.TrimSpace(key) == "" {
			return RecallRequest{}, fmt.Errorf("%w: blank recall key", ErrInvalidMemory)
		}
		if _, exists := seenKeys[key]; exists {
			return RecallRequest{}, fmt.Errorf("%w: duplicate recall key", ErrInvalidMemory)
		}
		seenKeys[key] = struct{}{}
	}
	seenKinds := make(map[Kind]struct{}, len(request.Kinds))
	for _, kind := range request.Kinds {
		if !kind.IsValid() {
			return RecallRequest{}, fmt.Errorf("%w: invalid recall kind", ErrInvalidMemory)
		}
		if _, exists := seenKinds[kind]; exists {
			return RecallRequest{}, fmt.Errorf("%w: duplicate recall kind", ErrInvalidMemory)
		}
		seenKinds[kind] = struct{}{}
	}
	request.Keys = append([]string(nil), request.Keys...)
	request.Kinds = append([]Kind(nil), request.Kinds...)
	return request, nil
}

type fusedRecallItem struct {
	item  RecallItem
	score float64
}

func (r *Recaller) fuse(set CandidateSet, request RecallRequest) ([]RecallItem, error) {
	byID := make(map[string]*fusedRecallItem)
	channels := []struct {
		channel    Channel
		candidates []Candidate
	}{
		{channel: ChannelExact, candidates: set.Exact},
		{channel: ChannelFullText, candidates: set.FullText},
		{channel: ChannelVector, candidates: set.Vector},
	}
	for _, listed := range channels {
		seen := make(map[string]struct{}, len(listed.candidates))
		for _, candidate := range listed.candidates {
			if candidate.Channel != listed.channel || candidate.Rank <= 0 || strings.TrimSpace(candidate.Memory.ID) == "" {
				return nil, fmt.Errorf("%w: malformed recall candidate", ErrInvalidMemory)
			}
			if _, duplicate := seen[candidate.Memory.ID]; duplicate {
				return nil, fmt.Errorf("%w: duplicate candidate in channel", ErrInvalidMemory)
			}
			seen[candidate.Memory.ID] = struct{}{}
			if candidate.Memory.Scope != request.Scope || candidate.Memory.Status != StatusActive ||
				request.Now.Before(candidate.Memory.ValidFrom) ||
				(!candidate.Memory.ValidUntil.IsZero() && !request.Now.Before(candidate.Memory.ValidUntil)) {
				continue
			}
			if !candidate.Memory.Kind.IsValid() || strings.TrimSpace(candidate.Memory.Key) == "" || strings.TrimSpace(candidate.Memory.Content) == "" {
				return nil, fmt.Errorf("%w: malformed recalled memory", ErrInvalidMemory)
			}
			if listed.channel == ChannelVector {
				if math.IsNaN(candidate.Similarity) || math.IsInf(candidate.Similarity, 0) || candidate.Similarity < 0 || candidate.Similarity > 1 {
					return nil, fmt.Errorf("%w: invalid vector similarity", ErrInvalidMemory)
				}
				if candidate.Similarity < r.policy.MinVectorSimilarity {
					continue
				}
			}
			fused, exists := byID[candidate.Memory.ID]
			if !exists {
				fused = &fusedRecallItem{item: RecallItem{
					Memory: candidate.Memory, Sources: append([]Source(nil), candidate.Sources...),
				}}
				byID[candidate.Memory.ID] = fused
			}
			fused.score += 1 / (r.policy.RRFK + float64(candidate.Rank))
		}
	}

	ranked := make([]fusedRecallItem, 0, len(byID))
	for _, item := range byID {
		ranked = append(ranked, *item)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		left, right := ranked[i].item.Memory, ranked[j].item.Memory
		if left.Importance != right.Importance {
			return left.Importance > right.Importance
		}
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		return left.ID < right.ID
	})
	result := make([]RecallItem, len(ranked))
	for index := range ranked {
		result[index] = copyRecallItem(ranked[index].item)
	}
	return result, nil
}

type canonicalRecallRecord struct {
	ID         string   `json:"id"`
	Kind       Kind     `json:"kind"`
	Key        string   `json:"key"`
	Content    string   `json:"content"`
	SourceRefs []string `json:"source_refs"`
}

func canonicalRecallPayload(item RecallItem) (string, error) {
	refs := make([]string, 0, len(item.Sources))
	seen := make(map[string]struct{}, len(item.Sources))
	for _, source := range item.Sources {
		if _, exists := seen[source.Ref]; exists {
			continue
		}
		seen[source.Ref] = struct{}{}
		refs = append(refs, source.Ref)
	}
	sort.Strings(refs)
	payload, err := json.Marshal(canonicalRecallRecord{
		ID: item.Memory.ID, Kind: item.Memory.Kind, Key: item.Memory.Key,
		Content: item.Memory.Content, SourceRefs: refs,
	})
	if err != nil {
		return "", fmt.Errorf("%w: encode recall token payload", ErrInvalidMemory)
	}
	return string(payload), nil
}

func validateEmbeddingResult(vectors [][]float32, dimensions int) ([]float32, error) {
	if len(vectors) != 1 || len(vectors[0]) != dimensions {
		return nil, fmt.Errorf("%w: invalid embedding result", ErrInvalidMemory)
	}
	result := append([]float32(nil), vectors[0]...)
	for _, value := range result {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("%w: invalid embedding result", ErrInvalidMemory)
		}
	}
	return result, nil
}

func containsChannel(channels []Channel, target Channel) bool {
	for _, channel := range channels {
		if channel == target {
			return true
		}
	}
	return false
}

func copyRecallItem(item RecallItem) RecallItem {
	item.Sources = append([]Source(nil), item.Sources...)
	return item
}

func copyRecallResult(result RecallResult) RecallResult {
	items := make([]RecallItem, len(result.Items))
	for index := range result.Items {
		items[index] = copyRecallItem(result.Items[index])
	}
	result.Items = items
	result.DegradedChannels = append([]Channel(nil), result.DegradedChannels...)
	return result
}
