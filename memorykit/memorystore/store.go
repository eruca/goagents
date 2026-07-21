package memorystore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eruca/goagents/memorykit"
)

type Store struct {
	mu         sync.RWMutex
	limits     memorykit.Limits
	memories   map[string]memorykit.Memory
	sources    map[string][]memorykit.Source
	revisions  map[string][]memorykit.Revision
	creates    map[idempotencyIdentity]createRecord
	embeddings map[string]storedEmbedding
}

type storedEmbedding struct {
	profileID  string
	dimensions int
	vector     []float32
}

type idempotencyIdentity struct {
	scope memorykit.Scope
	key   string
}

type createRecord struct {
	id string
	// fingerprint proves an exact replay without retaining a second content/source copy.
	// Erase clears it and keeps only the tombstone identity, so erased input cannot replay.
	fingerprint [sha256.Size]byte
	erased      bool
}

func New(limits memorykit.Limits) (*Store, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &Store{
		limits:     limits,
		memories:   make(map[string]memorykit.Memory),
		sources:    make(map[string][]memorykit.Source),
		revisions:  make(map[string][]memorykit.Revision),
		creates:    make(map[idempotencyIdentity]createRecord),
		embeddings: make(map[string]storedEmbedding),
	}, nil
}

func (s *Store) Create(ctx context.Context, request memorykit.CreateRequest) (memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	if err := memorykit.ValidateCreateRequest(request, s.limits); err != nil {
		return memorykit.Memory{}, err
	}
	var fingerprint [sha256.Size]byte
	if request.IdempotencyKey != "" {
		var err error
		fingerprint, err = createFingerprint(request)
		if err != nil {
			return memorykit.Memory{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}

	if request.IdempotencyKey != "" {
		identity := idempotencyIdentity{scope: request.Scope, key: request.IdempotencyKey}
		if previous, exists := s.creates[identity]; exists {
			if !previous.erased && previous.id == request.ID && previous.fingerprint == fingerprint {
				current, found := s.memories[request.ID]
				if found {
					return current, nil
				}
			}
			return memorykit.Memory{}, fmt.Errorf("%w: idempotency key was already used", memorykit.ErrConflict)
		}
	}
	if _, exists := s.memories[request.ID]; exists {
		return memorykit.Memory{}, fmt.Errorf("%w: memory ID already exists", memorykit.ErrConflict)
	}

	memory := memorykit.Memory{
		ID:             request.ID,
		Scope:          request.Scope,
		Kind:           request.Kind,
		Key:            request.Key,
		Status:         request.Status,
		Content:        request.Content,
		ValidFrom:      request.ValidFrom,
		ValidUntil:     request.ValidUntil,
		Importance:     request.Importance,
		Confidence:     request.Confidence,
		SourceAgentID:  request.SourceAgentID,
		CreatedBy:      request.Actor,
		IdempotencyKey: request.IdempotencyKey,
		Version:        1,
		CreatedAt:      request.Now,
		UpdatedAt:      request.Now,
	}
	if memory.Status == memorykit.StatusActive {
		s.supersedeActiveLocked(memory, memory.ID, request.Actor, request.Reason, request.Now)
	}
	s.memories[memory.ID] = memory
	s.sources[memory.ID] = copySources(request.Sources)
	s.appendRevisionLocked(memory, memorykit.RevisionCreate, request.Actor, request.Reason, request.Now, false)
	if request.IdempotencyKey != "" {
		s.creates[idempotencyIdentity{scope: request.Scope, key: request.IdempotencyKey}] = createRecord{
			id: request.ID, fingerprint: fingerprint,
		}
	}
	return memory, nil
}

func (s *Store) Get(ctx context.Context, scope memorykit.Scope, id string) (memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	if err := validateIdentity(scope, id); err != nil {
		return memorykit.Memory{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	memory, ok := s.memories[id]
	if !ok || memory.Scope != scope {
		return memorykit.Memory{}, memorykit.ErrNotFound
	}
	return memory, nil
}

func (s *Store) Sources(ctx context.Context, scope memorykit.Scope, id string) ([]memorykit.Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateIdentity(scope, id); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	memory, ok := s.memories[id]
	if !ok || memory.Scope != scope {
		return nil, memorykit.ErrNotFound
	}
	return copySources(s.sources[id]), nil
}

func (s *Store) List(ctx context.Context, query memorykit.ListQuery) ([]memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := memorykit.ValidateListQuery(query, s.limits); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	result := make([]memorykit.Memory, 0)
	for _, memory := range s.memories {
		if memory.Scope != query.Scope || (query.Kind != "" && memory.Kind != query.Kind) ||
			(query.Status != "" && memory.Status != query.Status) || (query.Key != "" && memory.Key != query.Key) {
			continue
		}
		result = append(result, memory)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	if len(result) > query.Limit {
		result = result[:query.Limit]
	}
	return result, nil
}

// SearchCandidates is a deterministic reference implementation for tests and local use.
func (s *Store) SearchCandidates(ctx context.Context, query memorykit.CandidateQuery) (memorykit.CandidateSet, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.CandidateSet{}, err
	}
	if err := memorykit.ValidateCandidateQuery(query, s.limits); err != nil {
		return memorykit.CandidateSet{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return memorykit.CandidateSet{}, err
	}

	keys := make(map[string]struct{}, len(query.Keys))
	for _, key := range query.Keys {
		keys[key] = struct{}{}
	}
	kinds := make(map[memorykit.Kind]struct{}, len(query.Kinds))
	for _, kind := range query.Kinds {
		kinds[kind] = struct{}{}
	}
	foldedText := strings.ToLower(query.Text)
	result := memorykit.CandidateSet{
		Exact: make([]memorykit.Candidate, 0), FullText: make([]memorykit.Candidate, 0),
		Vector: make([]memorykit.Candidate, 0),
	}
	for id, memory := range s.memories {
		if memory.Scope != query.Scope || memory.Status != memorykit.StatusActive ||
			query.Now.Before(memory.ValidFrom) ||
			(!memory.ValidUntil.IsZero() && !query.Now.Before(memory.ValidUntil)) ||
			(len(kinds) > 0 && !containsKind(kinds, memory.Kind)) {
			continue
		}
		sources := copySources(s.sources[id])
		if query.ExactLimit > 0 {
			if _, matches := keys[memory.Key]; matches {
				result.Exact = append(result.Exact, memorykit.Candidate{Memory: memory, Sources: sources})
			}
		}
		if query.FullTextLimit > 0 && foldedText != "" &&
			(strings.Contains(strings.ToLower(memory.Key), foldedText) || strings.Contains(strings.ToLower(memory.Content), foldedText)) {
			result.FullText = append(result.FullText, memorykit.Candidate{Memory: memory, Sources: copySources(sources)})
		}
		if query.VectorLimit > 0 {
			embedding, exists := s.embeddings[id]
			if !exists || embedding.profileID != query.EmbeddingProfileID || embedding.dimensions != query.EmbeddingDimensions {
				continue
			}
			similarity, ok := cosineSimilarity(query.QueryVector, embedding.vector)
			if !ok || similarity < query.MinVectorSimilarity {
				continue
			}
			result.Vector = append(result.Vector, memorykit.Candidate{
				Memory: memory, Sources: copySources(sources), Similarity: similarity,
			})
		}
	}

	sort.Slice(result.Exact, func(i, j int) bool { return candidateMetadataLess(result.Exact[i], result.Exact[j]) })
	sort.Slice(result.FullText, func(i, j int) bool { return candidateMetadataLess(result.FullText[i], result.FullText[j]) })
	sort.Slice(result.Vector, func(i, j int) bool {
		if result.Vector[i].Similarity != result.Vector[j].Similarity {
			return result.Vector[i].Similarity > result.Vector[j].Similarity
		}
		return candidateMetadataLess(result.Vector[i], result.Vector[j])
	})
	result.Exact = rankCandidates(result.Exact, query.ExactLimit, memorykit.ChannelExact)
	result.FullText = rankCandidates(result.FullText, query.FullTextLimit, memorykit.ChannelFullText)
	result.Vector = rankCandidates(result.Vector, query.VectorLimit, memorykit.ChannelVector)
	return result, nil
}

func (s *Store) Activate(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	if err := memorykit.ValidateCommand(command, s.limits); err != nil {
		return memorykit.Memory{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	memory, err := s.commandTargetLocked(command)
	if err != nil {
		return memorykit.Memory{}, err
	}

	s.supersedeActiveLocked(memory, memory.ID, command.Actor, command.Reason, command.Now)
	memory.Status = memorykit.StatusActive
	memory.Version++
	memory.UpdatedAt = command.Now
	s.memories[memory.ID] = memory
	s.appendRevisionLocked(memory, memorykit.RevisionActivate, command.Actor, command.Reason, command.Now, false)
	return memory, nil
}

func (s *Store) Correct(ctx context.Context, request memorykit.CorrectRequest) (memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	if err := memorykit.ValidateCorrectRequest(request, s.limits); err != nil {
		return memorykit.Memory{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	memory, err := s.commandTargetLocked(request.Command)
	if err != nil {
		return memorykit.Memory{}, err
	}

	memory.Content = request.Content
	memory.ValidFrom = request.ValidFrom
	memory.ValidUntil = request.ValidUntil
	memory.Importance = request.Importance
	memory.Confidence = request.Confidence
	memory.Version++
	memory.UpdatedAt = request.Command.Now
	s.memories[memory.ID] = memory
	s.sources[memory.ID] = copySources(request.Sources)
	s.appendRevisionLocked(memory, memorykit.RevisionCorrect, request.Command.Actor, request.Command.Reason, request.Command.Now, false)
	return memory, nil
}

func (s *Store) Dismiss(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	return s.transitionInactive(ctx, command, memorykit.RevisionDismiss)
}

func (s *Store) Forget(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	return s.transitionInactive(ctx, command, memorykit.RevisionForget)
}

func (s *Store) Erase(ctx context.Context, command memorykit.VersionedCommand) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := memorykit.ValidateCommand(command, s.limits); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	memory, err := s.commandTargetLocked(command)
	if err != nil {
		return err
	}

	memory.Content = ""
	memory.Status = memorykit.StatusInactive
	memory.Version++
	memory.UpdatedAt = command.Now
	s.memories[memory.ID] = memory
	s.sources[memory.ID] = make([]memorykit.Source, 0)
	delete(s.embeddings, memory.ID)
	for identity, record := range s.creates {
		if record.id != memory.ID {
			continue
		}
		record.fingerprint = [sha256.Size]byte{}
		record.erased = true
		s.creates[identity] = record
	}
	for index := range s.revisions[memory.ID] {
		revision := s.revisions[memory.ID][index]
		revision.Snapshot.Content = ""
		revision.ContentErased = true
		s.revisions[memory.ID][index] = revision
	}
	s.appendRevisionLocked(memory, memorykit.RevisionErase, command.Actor, command.Reason, command.Now, true)
	return nil
}

func (s *Store) Revisions(ctx context.Context, query memorykit.RevisionQuery) ([]memorykit.Revision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := memorykit.ValidateRevisionQuery(query, s.limits); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	memory, ok := s.memories[query.MemoryID]
	if !ok || memory.Scope != query.Scope {
		return nil, memorykit.ErrNotFound
	}

	stored := s.revisions[query.MemoryID]
	result := make([]memorykit.Revision, 0, min(len(stored), query.Limit))
	for index := len(stored) - 1; index >= 0 && len(result) < query.Limit; index-- {
		revision := stored[index]
		if query.BeforeVersion > 0 && revision.Version >= query.BeforeVersion {
			continue
		}
		result = append(result, revision)
	}
	return result, nil
}

func (s *Store) transitionInactive(ctx context.Context, command memorykit.VersionedCommand, action memorykit.RevisionAction) (memorykit.Memory, error) {
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	if err := memorykit.ValidateCommand(command, s.limits); err != nil {
		return memorykit.Memory{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return memorykit.Memory{}, err
	}
	memory, err := s.commandTargetLocked(command)
	if err != nil {
		return memorykit.Memory{}, err
	}
	memory.Status = memorykit.StatusInactive
	memory.Version++
	memory.UpdatedAt = command.Now
	s.memories[memory.ID] = memory
	s.appendRevisionLocked(memory, action, command.Actor, command.Reason, command.Now, false)
	return memory, nil
}

func (s *Store) commandTargetLocked(command memorykit.VersionedCommand) (memorykit.Memory, error) {
	memory, ok := s.memories[command.ID]
	if !ok || memory.Scope != command.Scope {
		return memorykit.Memory{}, memorykit.ErrNotFound
	}
	if memory.Version != command.ExpectedVersion {
		return memorykit.Memory{}, memorykit.ErrConflict
	}
	return memory, nil
}

// supersedeActiveLocked is called inside the same write lock as the activating mutation.
func (s *Store) supersedeActiveLocked(target memorykit.Memory, excludeID, actor, reason string, now time.Time) {
	for id, memory := range s.memories {
		if id == excludeID || memory.Status != memorykit.StatusActive || !sameConflictKey(memory, target) {
			continue
		}
		memory.Status = memorykit.StatusInactive
		memory.Version++
		memory.UpdatedAt = now
		s.memories[id] = memory
		s.appendRevisionLocked(memory, memorykit.RevisionSupersede, actor, reason, now, false)
	}
}

func sameConflictKey(a, b memorykit.Memory) bool {
	return a.Scope == b.Scope && a.Kind == b.Kind && a.Key == b.Key
}

func (s *Store) appendRevisionLocked(memory memorykit.Memory, action memorykit.RevisionAction, actor, reason string, createdAt time.Time, erased bool) {
	s.revisions[memory.ID] = append(s.revisions[memory.ID], memorykit.Revision{
		MemoryID: memory.ID, Version: memory.Version, Action: action,
		Actor: actor, Reason: reason, Snapshot: memory, ContentErased: erased, CreatedAt: createdAt,
	})
}

func createFingerprint(request memorykit.CreateRequest) ([sha256.Size]byte, error) {
	request.Now = time.Time{}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("%w: encode create fingerprint: %v", memorykit.ErrInvalidMemory, err)
	}
	return sha256.Sum256(encoded), nil
}

func copySources(sources []memorykit.Source) []memorykit.Source {
	return append([]memorykit.Source{}, sources...)
}

func containsKind(kinds map[memorykit.Kind]struct{}, kind memorykit.Kind) bool {
	_, exists := kinds[kind]
	return exists
}

func candidateMetadataLess(left, right memorykit.Candidate) bool {
	if left.Memory.Importance != right.Memory.Importance {
		return left.Memory.Importance > right.Memory.Importance
	}
	if !left.Memory.UpdatedAt.Equal(right.Memory.UpdatedAt) {
		return left.Memory.UpdatedAt.After(right.Memory.UpdatedAt)
	}
	return left.Memory.ID < right.Memory.ID
}

func rankCandidates(candidates []memorykit.Candidate, limit int, channel memorykit.Channel) []memorykit.Candidate {
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	result := make([]memorykit.Candidate, len(candidates))
	for index := range candidates {
		result[index] = candidates[index]
		result[index].Sources = copySources(candidates[index].Sources)
		result[index].Channel = channel
		result[index].Rank = index + 1
	}
	return result
}

func cosineSimilarity(left, right []float32) (float64, bool) {
	if len(left) == 0 || len(left) != len(right) {
		return 0, false
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		leftValue, rightValue := float64(left[index]), float64(right[index])
		dot += leftValue * rightValue
		leftNorm += leftValue * leftValue
		rightNorm += rightValue * rightValue
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0, false
	}
	similarity := dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
	if similarity > 1 {
		similarity = 1
	} else if similarity < -1 {
		similarity = -1
	}
	return similarity, true
}

func validateIdentity(scope memorykit.Scope, id string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: memory ID is required", memorykit.ErrInvalidMemory)
	}
	return nil
}
