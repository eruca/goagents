package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/google/uuid"
)

type createMemoryRequest struct {
	Kind           string                `json:"kind"`
	Key            string                `json:"key"`
	Content        string                `json:"content"`
	ValidUntil     string                `json:"valid_until,omitempty"`
	Reason         string                `json:"reason"`
	IdempotencyKey string                `json:"idempotency_key,omitempty"`
	Importance     int                   `json:"importance"`
	Confidence     float64               `json:"confidence"`
	Sources        []memorySourceRequest `json:"sources,omitempty"`
}

type memorySourceRequest struct {
	Kind         string `json:"kind"`
	Ref          string `json:"ref"`
	EvidenceHash string `json:"evidence_hash,omitempty"`
}

type versionedMemoryRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
}

type correctMemoryRequest struct {
	ExpectedVersion int64                 `json:"expected_version"`
	Content         string                `json:"content"`
	ValidFrom       string                `json:"valid_from"`
	ValidUntil      string                `json:"valid_until,omitempty"`
	Reason          string                `json:"reason"`
	Importance      int                   `json:"importance"`
	Confidence      float64               `json:"confidence"`
	Sources         []memorySourceRequest `json:"sources,omitempty"`
}

type memoryResponse struct {
	ID            string                 `json:"id"`
	SubjectType   memorykit.SubjectType  `json:"subject_type"`
	SubjectID     string                 `json:"subject_id"`
	Kind          memorykit.Kind         `json:"kind"`
	Key           string                 `json:"key"`
	Status        memorykit.Status       `json:"status"`
	Content       string                 `json:"content"`
	ValidFrom     string                 `json:"valid_from"`
	ValidUntil    string                 `json:"valid_until,omitempty"`
	Importance    int                    `json:"importance"`
	Confidence    float64                `json:"confidence"`
	SourceAgentID string                 `json:"source_agent_id,omitempty"`
	CreatedBy     string                 `json:"created_by,omitempty"`
	Sources       []memorySourceResponse `json:"sources"`
	Version       int64                  `json:"version"`
	CreatedAt     string                 `json:"created_at"`
	UpdatedAt     string                 `json:"updated_at"`
}

type memorySourceResponse struct {
	Kind         string `json:"kind"`
	Ref          string `json:"ref"`
	EvidenceHash string `json:"evidence_hash,omitempty"`
}

type memoryListResponse struct {
	Memories []memoryResponse `json:"memories"`
}

type memoryRevisionResponse struct {
	MemoryID      string                   `json:"memory_id"`
	Version       int64                    `json:"version"`
	Action        memorykit.RevisionAction `json:"action"`
	Actor         string                   `json:"actor,omitempty"`
	Reason        string                   `json:"reason,omitempty"`
	Snapshot      memoryResponse           `json:"snapshot"`
	ContentErased bool                     `json:"content_erased"`
	CreatedAt     string                   `json:"created_at"`
}

type memoryRevisionListResponse struct {
	Revisions []memoryRevisionResponse `json:"revisions"`
}

var (
	errMemorySnapshotChanged    = errors.New("memory snapshot changed")
	errMemoryInvalidStoreResult = errors.New("memory store returned invalid data")
	errMemoryClockRollback      = errors.New("memory clock precedes stored memory")
)

func (s *Server) handleListMemories(w http.ResponseWriter, r *http.Request) {
	queryValues, err := parseMemoryQuery(r.URL.RawQuery, map[string]struct{}{
		"kind": {}, "status": {}, "key": {}, "limit": {},
	})
	if err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory list query")
		return
	}
	status := memorykit.StatusActive
	capability := memoryRead
	if values, present := queryValues["status"]; present {
		status = memorykit.Status(values[0])
		if status == "" {
			writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory list query")
			return
		}
		if status == memorykit.StatusCandidate || status == memorykit.StatusInactive {
			capability = memoryReview
		}
	}
	limit, err := memoryQueryLimit(queryValues, s.memory.Limits.MaxListItems)
	if err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory list limit")
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		scope, _, ok := s.authorizeProjectMemory(w, r, capability)
		if !ok {
			return
		}
		query := memorykit.ListQuery{Scope: scope, Status: status, Limit: limit}
		if values, present := queryValues["kind"]; present {
			query.Kind = memorykit.Kind(values[0])
		}
		if values, present := queryValues["key"]; present {
			query.Key = values[0]
		}
		if err := memorykit.ValidateListQuery(query, s.memory.Limits); err != nil {
			writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory list query")
			return
		}
		response, err := s.stableMemoryList(r, query)
		if errors.Is(err, errMemorySnapshotChanged) {
			continue
		}
		if err != nil {
			writeMemoryStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned unstable data")
}

func (s *Server) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("memoryID")
	for attempt := 0; attempt < 2; attempt++ {
		scope, _, ok := s.authorizeProjectMemory(w, r, memoryRead)
		if !ok {
			return
		}
		if err := memorykit.ValidateMemoryID(id); err != nil {
			writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory ID")
			return
		}
		memory, err := s.memory.Store.Get(r.Context(), scope, id)
		if err != nil {
			writeMemoryStoreError(w, err)
			return
		}
		if memory.ID != id || memory.Scope != scope || validateMemorySnapshot(memory, s.memory.Limits) != nil {
			writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
			return
		}
		if memory.Status != memorykit.StatusActive {
			reviewScope, _, reviewed := s.authorizeProjectMemory(w, r, memoryReview)
			if !reviewed {
				return
			}
			if reviewScope != scope {
				writeMemoryError(w, http.StatusForbidden, "forbidden", "project memory access denied")
				return
			}
		}
		sources, err := s.readCanonicalMemorySources(r, scope, id)
		if errors.Is(err, memorykit.ErrNotFound) {
			continue
		}
		if err != nil {
			writeMemoryStoreError(w, err)
			return
		}
		confirmed, err := s.memory.Store.Get(r.Context(), scope, id)
		if errors.Is(err, memorykit.ErrNotFound) {
			continue
		}
		if err != nil {
			writeMemoryStoreError(w, err)
			return
		}
		if confirmed.ID != id || confirmed.Scope != scope || validateMemorySnapshot(confirmed, s.memory.Limits) != nil {
			writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
			return
		}
		if !sameMemorySnapshot(confirmed, memory) {
			continue
		}
		writeJSON(w, http.StatusOK, memoryToResponse(confirmed, sources))
		return
	}
	writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned unstable data")
}

// stableMemoryList rejects a mixed snapshot instead of returning content that
// changed while its sources were read. One retry is owned by the HTTP handler.
func (s *Server) stableMemoryList(r *http.Request, query memorykit.ListQuery) (memoryListResponse, error) {
	memories, err := s.memory.Store.List(r.Context(), query)
	if err != nil {
		return memoryListResponse{}, err
	}
	if len(memories) > query.Limit || !validMemoryListOrder(memories) {
		return memoryListResponse{}, errMemoryInvalidStoreResult
	}

	response := memoryListResponse{Memories: make([]memoryResponse, 0, len(memories))}
	for _, listed := range memories {
		if !memoryMatchesListQuery(listed, query) || validateMemorySnapshot(listed, s.memory.Limits) != nil {
			return memoryListResponse{}, errMemoryInvalidStoreResult
		}
		sources, err := s.readCanonicalMemorySources(r, query.Scope, listed.ID)
		if errors.Is(err, memorykit.ErrNotFound) {
			return memoryListResponse{}, errMemorySnapshotChanged
		}
		if err != nil {
			return memoryListResponse{}, err
		}
		confirmed, err := s.memory.Store.Get(r.Context(), query.Scope, listed.ID)
		if errors.Is(err, memorykit.ErrNotFound) {
			return memoryListResponse{}, errMemorySnapshotChanged
		}
		if err != nil {
			return memoryListResponse{}, err
		}
		if !memoryMatchesListQuery(confirmed, query) || validateMemorySnapshot(confirmed, s.memory.Limits) != nil {
			return memoryListResponse{}, errMemoryInvalidStoreResult
		}
		if !sameMemorySnapshot(listed, confirmed) {
			return memoryListResponse{}, errMemorySnapshotChanged
		}
		response.Memories = append(response.Memories, memoryToResponse(confirmed, sources))
	}
	return response, nil
}

func (s *Server) readCanonicalMemorySources(r *http.Request, scope memorykit.Scope, id string) ([]memorykit.Source, error) {
	sources, err := s.memory.Store.Sources(r.Context(), scope, id)
	if err != nil {
		return nil, err
	}
	if err := memorykit.ValidateSources(sources, s.memory.Limits); err != nil {
		return nil, errMemoryInvalidStoreResult
	}
	return canonicalMemorySources(sources), nil
}

// readStableStoredMemory provides the bounded Memory -> Sources -> Memory
// confirmation used before mutations.
func (s *Server) readStableStoredMemory(r *http.Request, scope memorykit.Scope, id string) (memorykit.Memory, []memorykit.Source, error) {
	first, err := s.memory.Store.Get(r.Context(), scope, id)
	if err != nil {
		return memorykit.Memory{}, nil, err
	}
	if first.ID != id || first.Scope != scope || validateMemorySnapshot(first, s.memory.Limits) != nil {
		return memorykit.Memory{}, nil, errMemoryInvalidStoreResult
	}
	sources, err := s.readCanonicalMemorySources(r, scope, id)
	if errors.Is(err, memorykit.ErrNotFound) {
		return memorykit.Memory{}, nil, errMemorySnapshotChanged
	}
	if err != nil {
		return memorykit.Memory{}, nil, err
	}
	confirmed, err := s.memory.Store.Get(r.Context(), scope, id)
	if errors.Is(err, memorykit.ErrNotFound) {
		return memorykit.Memory{}, nil, errMemorySnapshotChanged
	}
	if err != nil {
		return memorykit.Memory{}, nil, err
	}
	if confirmed.ID != id || confirmed.Scope != scope || validateMemorySnapshot(confirmed, s.memory.Limits) != nil {
		return memorykit.Memory{}, nil, errMemoryInvalidStoreResult
	}
	if !sameMemorySnapshot(first, confirmed) {
		return memorykit.Memory{}, nil, errMemorySnapshotChanged
	}
	return confirmed, sources, nil
}

// confirmStoredMemory treats the mutation result as the first Memory read, then
// verifies the sources and a fresh persisted Memory before responding.
func (s *Server) confirmStoredMemory(r *http.Request, expected memorykit.Memory) (memorykit.Memory, []memorykit.Source, error) {
	if validateMemorySnapshot(expected, s.memory.Limits) != nil {
		return memorykit.Memory{}, nil, errMemoryInvalidStoreResult
	}
	sources, err := s.readCanonicalMemorySources(r, expected.Scope, expected.ID)
	if errors.Is(err, memorykit.ErrNotFound) {
		return memorykit.Memory{}, nil, errMemorySnapshotChanged
	}
	if err != nil {
		return memorykit.Memory{}, nil, err
	}
	confirmed, err := s.memory.Store.Get(r.Context(), expected.Scope, expected.ID)
	if errors.Is(err, memorykit.ErrNotFound) {
		return memorykit.Memory{}, nil, errMemorySnapshotChanged
	}
	if err != nil {
		return memorykit.Memory{}, nil, err
	}
	if confirmed.ID != expected.ID || confirmed.Scope != expected.Scope || validateMemorySnapshot(confirmed, s.memory.Limits) != nil {
		return memorykit.Memory{}, nil, errMemoryInvalidStoreResult
	}
	if !sameMemorySnapshot(expected, confirmed) {
		return memorykit.Memory{}, nil, errMemorySnapshotChanged
	}
	return confirmed, sources, nil
}

func validMemoryListOrder(memories []memorykit.Memory) bool {
	seen := make(map[string]struct{}, len(memories))
	for index, memory := range memories {
		if _, exists := seen[memory.ID]; exists {
			return false
		}
		seen[memory.ID] = struct{}{}
		if index == 0 {
			continue
		}
		previous := memories[index-1]
		if memory.UpdatedAt.After(previous.UpdatedAt) ||
			(memory.UpdatedAt.Equal(previous.UpdatedAt) && memory.ID <= previous.ID) {
			return false
		}
	}
	return true
}

func memoryMatchesListQuery(memory memorykit.Memory, query memorykit.ListQuery) bool {
	return memory.Scope == query.Scope && memory.Status == query.Status &&
		(query.Kind == "" || memory.Kind == query.Kind) && (query.Key == "" || memory.Key == query.Key)
}

func (s *Server) handleMemoryRevisions(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := s.authorizeProjectMemory(w, r, memoryReview)
	if !ok {
		return
	}
	values, err := parseMemoryQuery(r.URL.RawQuery, map[string]struct{}{"limit": {}, "before_version": {}})
	if err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory revision query")
		return
	}
	limit, err := memoryQueryLimit(values, s.memory.Limits.MaxListItems)
	if err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory revision limit")
		return
	}
	query := memorykit.RevisionQuery{Scope: scope, MemoryID: r.PathValue("memoryID"), Limit: limit}
	if cursor, present := values["before_version"]; present {
		query.BeforeVersion, err = strconv.ParseInt(cursor[0], 10, 64)
		if err != nil || query.BeforeVersion <= 0 {
			writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory revision cursor")
			return
		}
	}
	if err := memorykit.ValidateRevisionQuery(query, s.memory.Limits); err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory revision query")
		return
	}
	current, err := s.memory.Store.Get(r.Context(), scope, query.MemoryID)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if current.ID != query.MemoryID || current.Scope != scope || validateMemorySnapshot(current, s.memory.Limits) != nil {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return
	}
	revisions, err := s.memory.Store.Revisions(r.Context(), query)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if len(revisions) > limit {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return
	}
	response := memoryRevisionListResponse{Revisions: make([]memoryRevisionResponse, 0, len(revisions))}
	var previousVersion int64
	currentIsTombstone := current.Status == memorykit.StatusInactive && current.Content == ""
	for index, revision := range revisions {
		if validateMemoryRevision(revision, query, s.memory.Limits) != nil {
			writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
			return
		}
		if index > 0 && revision.Version >= previousVersion {
			writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
			return
		}
		if currentIsTombstone && !revision.ContentErased {
			writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
			return
		}
		previousVersion = revision.Version
		response.Revisions = append(response.Revisions, memoryRevisionToResponse(revision))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleCreateMemory(w http.ResponseWriter, r *http.Request) {
	scope, identity, ok := s.authorizeProjectMemory(w, r, memoryWriteExplicit)
	if !ok {
		return
	}
	var input createMemoryRequest
	if !s.decodeMemoryJSONStrict(w, r, &input) {
		return
	}
	now, ok := s.memoryNow(w)
	if !ok {
		return
	}
	validUntil, err := parseMemoryTime(input.ValidUntil)
	if err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory validity time")
		return
	}
	id, err := s.newMemoryHTTPID(scope, input.IdempotencyKey)
	if err != nil {
		writeMemoryError(w, http.StatusInternalServerError, "memory_configuration_error", "memory ID generation failed")
		return
	}
	request := memorykit.CreateRequest{
		ID: id, Scope: scope, Kind: memorykit.Kind(input.Kind), Key: input.Key,
		Status: memorykit.StatusActive, Content: input.Content,
		ValidFrom: now, ValidUntil: validUntil, Importance: input.Importance, Confidence: input.Confidence,
		Actor: identity.Subject, Reason: input.Reason, IdempotencyKey: input.IdempotencyKey,
		Sources: memorySourcesFromRequest(input.Sources), Now: now,
	}
	if err := memorykit.ValidateCreateRequest(request, s.memory.Limits); err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory create request")
		return
	}
	if !s.validateMemoryContent(w, r, request.Content) {
		return
	}
	replay := false
	if request.IdempotencyKey != "" {
		var replayErr error
		request, replay, replayErr = s.restoreCreateReplayTime(r, request)
		if replayErr != nil {
			writeMemoryStoreError(w, replayErr)
			return
		}
	}
	created, err := s.memory.Store.Create(r.Context(), request)
	if err != nil && request.IdempotencyKey != "" && errors.Is(err, memorykit.ErrConflict) && !replay {
		var replayErr error
		request, replay, replayErr = s.restoreCreateReplayTime(r, request)
		if replayErr != nil {
			writeMemoryStoreError(w, replayErr)
			return
		}
		if replay {
			created, err = s.memory.Store.Create(r.Context(), request)
		}
	}
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if validateHTTPCreateResult(created, request, s.memory.Limits) != nil {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return
	}
	confirmed, sources, err := s.confirmStoredMemory(r, created)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if created.Version == 1 && !sameMemorySourceSet(sources, request.Sources) {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return
	}
	writeJSON(w, http.StatusCreated, memoryToResponse(confirmed, sources))
}

func (s *Server) restoreCreateReplayTime(r *http.Request, request memorykit.CreateRequest) (memorykit.CreateRequest, bool, error) {
	current, err := s.memory.Store.Get(r.Context(), request.Scope, request.ID)
	if errors.Is(err, memorykit.ErrNotFound) {
		return request, false, nil
	}
	if err != nil {
		return memorykit.CreateRequest{}, false, err
	}
	if validateMemorySnapshot(current, s.memory.Limits) != nil || current.ID != request.ID || current.Scope != request.Scope {
		return memorykit.CreateRequest{}, false, errors.New("memory store returned invalid data")
	}
	// ValidFrom is Host-owned server time and therefore not part of the HTTP
	// semantic payload. Restoring the first create timestamp lets the Store's
	// durable fingerprint verify every caller-controlled field, including reason
	// and sources, without a process-local idempotency cache.
	request.ValidFrom = current.CreatedAt
	return request, true, nil
}

func (s *Server) handleActivateMemory(w http.ResponseWriter, r *http.Request) {
	scope, identity, input, ok := s.authorizeVersionedMemory(w, r, memoryReview)
	if !ok {
		return
	}
	id := r.PathValue("memoryID")
	command, ok := s.versionedMemoryCommand(w, scope, identity, input, id)
	if !ok {
		return
	}
	memory, sources, err := s.readStableStoredMemory(r, scope, id)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if memory.Status != memorykit.StatusCandidate || memory.Version != input.ExpectedVersion {
		writeMemoryError(w, http.StatusConflict, "memory_conflict", "memory cannot be activated")
		return
	}
	if command.Now.Before(memory.UpdatedAt) {
		writeMemoryStoreError(w, errMemoryClockRollback)
		return
	}
	if !s.validateMemoryContent(w, r, memory.Content) {
		return
	}
	updated, err := s.memory.Store.Activate(r.Context(), command)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if !s.validTransitionResult(w, memory, updated, memorykit.StatusActive, command) {
		return
	}
	confirmed, persistedSources, err := s.confirmStoredMemory(r, updated)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if !sameMemorySourceSet(sources, persistedSources) {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return
	}
	writeJSON(w, http.StatusOK, memoryToResponse(confirmed, persistedSources))
}

func (s *Server) handleDismissMemory(w http.ResponseWriter, r *http.Request) {
	s.handleVersionedMemoryMutation(w, r, memoryReview, memorykit.StatusCandidate, func(command memorykit.VersionedCommand) (memorykit.Memory, error) {
		return s.memory.Store.Dismiss(r.Context(), command)
	})
}

func (s *Server) handleForgetMemory(w http.ResponseWriter, r *http.Request) {
	s.handleVersionedMemoryMutation(w, r, memoryWriteExplicit, memorykit.StatusActive, func(command memorykit.VersionedCommand) (memorykit.Memory, error) {
		return s.memory.Store.Forget(r.Context(), command)
	})
}

func (s *Server) handleCorrectMemory(w http.ResponseWriter, r *http.Request) {
	scope, identity, ok := s.authorizeProjectMemory(w, r, memoryWriteExplicit)
	if !ok {
		return
	}
	var input correctMemoryRequest
	if !s.decodeMemoryJSONStrict(w, r, &input) {
		return
	}
	validFrom, err := parseRequiredMemoryTime(input.ValidFrom)
	if err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory validity time")
		return
	}
	validUntil, err := parseMemoryTime(input.ValidUntil)
	if err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory validity time")
		return
	}
	now, ok := s.memoryNow(w)
	if !ok {
		return
	}
	request := memorykit.CorrectRequest{
		Command: memorykit.VersionedCommand{Scope: scope, ID: r.PathValue("memoryID"), ExpectedVersion: input.ExpectedVersion, Actor: identity.Subject, Reason: input.Reason, Now: now},
		Content: input.Content, ValidFrom: validFrom, ValidUntil: validUntil,
		Importance: input.Importance, Confidence: input.Confidence, Sources: memorySourcesFromRequest(input.Sources),
	}
	if err := memorykit.ValidateCorrectRequest(request, s.memory.Limits); err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory correction request")
		return
	}
	if !s.validateMemoryContent(w, r, request.Content) {
		return
	}
	current, _, err := s.readStableStoredMemory(r, scope, request.Command.ID)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if current.Version != request.Command.ExpectedVersion {
		writeMemoryError(w, http.StatusConflict, "memory_conflict", "memory version conflict")
		return
	}
	if request.Command.Now.Before(current.UpdatedAt) {
		writeMemoryStoreError(w, errMemoryClockRollback)
		return
	}
	updated, err := s.memory.Store.Correct(r.Context(), request)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if !s.validCorrectionResult(w, current, updated, request) {
		return
	}
	confirmed, persistedSources, err := s.confirmStoredMemory(r, updated)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if !sameMemorySourceSet(persistedSources, request.Sources) {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return
	}
	writeJSON(w, http.StatusOK, memoryToResponse(confirmed, persistedSources))
}

func (s *Server) handleEraseMemory(w http.ResponseWriter, r *http.Request) {
	scope, identity, input, ok := s.authorizeVersionedMemory(w, r, memoryErase)
	if !ok {
		return
	}
	command, ok := s.versionedMemoryCommand(w, scope, identity, input, r.PathValue("memoryID"))
	if !ok {
		return
	}
	current, _, err := s.readStableStoredMemory(r, scope, command.ID)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if current.Version != command.ExpectedVersion {
		writeMemoryError(w, http.StatusConflict, "memory_conflict", "memory version conflict")
		return
	}
	if command.Now.Before(current.UpdatedAt) {
		writeMemoryStoreError(w, errMemoryClockRollback)
		return
	}
	if err := s.memory.Store.Erase(r.Context(), command); err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	erased, sources, err := s.readStableStoredMemory(r, scope, command.ID)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if !validEraseResult(current, erased, sources, command) {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return
	}
	revisions, err := s.memory.Store.Revisions(r.Context(), memorykit.RevisionQuery{
		Scope: scope, MemoryID: command.ID, Limit: 1,
	})
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if len(revisions) != 1 || validateMemoryRevision(revisions[0], memorykit.RevisionQuery{
		Scope: scope, MemoryID: command.ID, Limit: 1,
	}, s.memory.Limits) != nil || !validEraseRevision(revisions[0], erased, command) {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleVersionedMemoryMutation(w http.ResponseWriter, r *http.Request, capability memoryCapability, requiredStatus memorykit.Status, mutate func(memorykit.VersionedCommand) (memorykit.Memory, error)) {
	scope, identity, input, ok := s.authorizeVersionedMemory(w, r, capability)
	if !ok {
		return
	}
	id := r.PathValue("memoryID")
	command, ok := s.versionedMemoryCommand(w, scope, identity, input, id)
	if !ok {
		return
	}
	current, sources, err := s.readStableStoredMemory(r, scope, id)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if current.Status != requiredStatus || current.Version != command.ExpectedVersion {
		writeMemoryError(w, http.StatusConflict, "memory_conflict", "memory version conflict")
		return
	}
	if command.Now.Before(current.UpdatedAt) {
		writeMemoryStoreError(w, errMemoryClockRollback)
		return
	}
	updated, err := mutate(command)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if !s.validTransitionResult(w, current, updated, memorykit.StatusInactive, command) {
		return
	}
	confirmed, persistedSources, err := s.confirmStoredMemory(r, updated)
	if err != nil {
		writeMemoryStoreError(w, err)
		return
	}
	if !sameMemorySourceSet(sources, persistedSources) {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return
	}
	writeJSON(w, http.StatusOK, memoryToResponse(confirmed, persistedSources))
}

func (s *Server) authorizeVersionedMemory(w http.ResponseWriter, r *http.Request, capability memoryCapability) (memorykit.Scope, memoryIdentity, versionedMemoryRequest, bool) {
	scope, identity, ok := s.authorizeProjectMemory(w, r, capability)
	if !ok {
		return memorykit.Scope{}, memoryIdentity{}, versionedMemoryRequest{}, false
	}
	var input versionedMemoryRequest
	if !s.decodeMemoryJSONStrict(w, r, &input) {
		return memorykit.Scope{}, memoryIdentity{}, versionedMemoryRequest{}, false
	}
	return scope, identity, input, true
}

func (s *Server) versionedMemoryCommand(w http.ResponseWriter, scope memorykit.Scope, identity memoryIdentity, input versionedMemoryRequest, id string) (memorykit.VersionedCommand, bool) {
	now, ok := s.memoryNow(w)
	if !ok {
		return memorykit.VersionedCommand{}, false
	}
	command := memorykit.VersionedCommand{Scope: scope, ID: id, ExpectedVersion: input.ExpectedVersion, Actor: identity.Subject, Reason: input.Reason, Now: now}
	if err := memorykit.ValidateCommand(command, s.memory.Limits); err != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory command")
		return memorykit.VersionedCommand{}, false
	}
	return command, true
}

func (s *Server) decodeMemoryJSONStrict(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.memory.MaxHTTPBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil || validateMemoryJSONShape(raw, target) != nil || json.Unmarshal(raw, target) != nil {
		writeMemoryError(w, http.StatusBadRequest, "invalid_json", "invalid memory request body")
		return false
	}
	return true
}

type memoryJSONShape struct {
	allowed  map[string]struct{}
	required map[string]struct{}
}

func validateMemoryJSONShape(raw []byte, target any) error {
	var shape memoryJSONShape
	switch target.(type) {
	case *createMemoryRequest:
		shape = newMemoryJSONShape(
			[]string{"kind", "key", "content", "valid_until", "reason", "idempotency_key", "importance", "confidence", "sources"},
			[]string{"kind", "key", "content", "reason", "importance", "confidence"},
		)
	case *correctMemoryRequest:
		shape = newMemoryJSONShape(
			[]string{"expected_version", "content", "valid_from", "valid_until", "reason", "importance", "confidence", "sources"},
			[]string{"expected_version", "content", "valid_from", "reason", "importance", "confidence"},
		)
	case *versionedMemoryRequest:
		shape = newMemoryJSONShape(
			[]string{"expected_version", "reason"},
			[]string{"expected_version", "reason"},
		)
	default:
		return errors.New("unsupported memory JSON target")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := validateMemoryJSONObject(decoder, shape, true); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing memory JSON")
	}
	return nil
}

func newMemoryJSONShape(allowed, required []string) memoryJSONShape {
	shape := memoryJSONShape{allowed: make(map[string]struct{}, len(allowed)), required: make(map[string]struct{}, len(required))}
	for _, field := range allowed {
		shape.allowed[field] = struct{}{}
	}
	for _, field := range required {
		shape.required[field] = struct{}{}
	}
	return shape
}

func validateMemoryJSONObject(decoder *json.Decoder, shape memoryJSONShape, topLevel bool) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("memory JSON object required")
	}
	seen := make(map[string]struct{}, len(shape.allowed))
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("memory JSON key required")
		}
		if _, allowed := shape.allowed[key]; !allowed {
			return errors.New("unknown memory JSON key")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("duplicate memory JSON key")
		}
		seen[key] = struct{}{}
		if topLevel && key == "sources" {
			if err := validateMemoryJSONSources(decoder); err != nil {
				return err
			}
			continue
		}
		if err := skipMemoryJSONValue(decoder); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	for field := range shape.required {
		if _, present := seen[field]; !present {
			return errors.New("missing memory JSON key")
		}
	}
	return nil
}

func validateMemoryJSONSources(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return errors.New("memory sources array required")
	}
	shape := newMemoryJSONShape([]string{"kind", "ref", "evidence_hash"}, []string{"kind", "ref"})
	for decoder.More() {
		if err := validateMemoryJSONObject(decoder, shape, false); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func skipMemoryJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		return nil
	}
	switch delimiter {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := skipMemoryJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := skipMemoryJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid memory JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

func (s *Server) validateMemoryContent(w http.ResponseWriter, r *http.Request, content string) bool {
	if err := s.memory.ContentValidator.ValidateMemoryContent(r.Context(), content); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeMemoryError(w, http.StatusServiceUnavailable, "memory_unavailable", "project memory is temporarily unavailable")
			return false
		}
		writeMemoryError(w, http.StatusBadRequest, "invalid_memory_content", "memory content was rejected")
		return false
	}
	return true
}

func (s *Server) memoryNow(w http.ResponseWriter) (time.Time, bool) {
	now := s.memory.Now().UTC()
	if now.IsZero() {
		writeMemoryError(w, http.StatusInternalServerError, "memory_configuration_error", "memory clock returned an invalid time")
		return time.Time{}, false
	}
	return now, true
}

func (s *Server) newMemoryHTTPID(scope memorykit.Scope, idempotencyKey string) (string, error) {
	id := ""
	if idempotencyKey == "" {
		id = s.memory.NewID()
	} else {
		canonical, err := json.Marshal(struct {
			Domain         string                `json:"domain"`
			TenantID       string                `json:"tenant_id"`
			SubjectType    memorykit.SubjectType `json:"subject_type"`
			SubjectID      string                `json:"subject_id"`
			IdempotencyKey string                `json:"idempotency_key"`
		}{
			Domain:   "github.com/eruca/goagents/examples/host-api/project-memory-create/v1",
			TenantID: scope.TenantID, SubjectType: scope.SubjectType, SubjectID: scope.SubjectID, IdempotencyKey: idempotencyKey,
		})
		if err != nil {
			return "", err
		}
		id = uuid.NewSHA1(uuid.NameSpaceURL, canonical).String()
	}
	if err := memorykit.ValidateMemoryID(id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Server) validTransitionResult(w http.ResponseWriter, before, after memorykit.Memory, expectedStatus memorykit.Status, command memorykit.VersionedCommand) bool {
	if validateMemorySnapshot(after, s.memory.Limits) != nil || !sameMemoryStableFields(before, after) ||
		after.Status != expectedStatus || after.Version != command.ExpectedVersion+1 ||
		!equivalentMemoryStoreTime(after.UpdatedAt, command.Now) {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return false
	}
	return true
}

func (s *Server) validCorrectionResult(w http.ResponseWriter, before, after memorykit.Memory, request memorykit.CorrectRequest) bool {
	if validateMemorySnapshot(after, s.memory.Limits) != nil ||
		after.ID != before.ID || after.Scope != before.Scope || after.Kind != before.Kind || after.Key != before.Key ||
		after.Status != before.Status || after.SourceAgentID != before.SourceAgentID || after.CreatedBy != before.CreatedBy ||
		after.IdempotencyKey != before.IdempotencyKey || !equivalentMemoryStoreTime(after.CreatedAt, before.CreatedAt) ||
		after.Version != request.Command.ExpectedVersion+1 || after.Content != request.Content ||
		!equivalentMemoryStoreTime(after.ValidFrom, request.ValidFrom) || !equivalentMemoryStoreTime(after.ValidUntil, request.ValidUntil) ||
		after.Importance != request.Importance || after.Confidence != request.Confidence ||
		!equivalentMemoryStoreTime(after.UpdatedAt, request.Command.Now) {
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
		return false
	}
	return true
}

func sameMemoryStableFields(left, right memorykit.Memory) bool {
	return left.ID == right.ID && left.Scope == right.Scope && left.Kind == right.Kind && left.Key == right.Key &&
		left.Content == right.Content && equivalentMemoryStoreTime(left.ValidFrom, right.ValidFrom) &&
		equivalentMemoryStoreTime(left.ValidUntil, right.ValidUntil) && left.Importance == right.Importance &&
		left.Confidence == right.Confidence && left.SourceAgentID == right.SourceAgentID && left.CreatedBy == right.CreatedBy &&
		left.IdempotencyKey == right.IdempotencyKey && equivalentMemoryStoreTime(left.CreatedAt, right.CreatedAt)
}

func sameMemorySnapshot(left, right memorykit.Memory) bool {
	return left.ID == right.ID && left.Scope == right.Scope && left.Kind == right.Kind && left.Key == right.Key &&
		left.Status == right.Status && left.Content == right.Content &&
		equivalentMemoryStoreTime(left.ValidFrom, right.ValidFrom) && equivalentMemoryStoreTime(left.ValidUntil, right.ValidUntil) &&
		left.Importance == right.Importance && left.Confidence == right.Confidence &&
		left.SourceAgentID == right.SourceAgentID && left.CreatedBy == right.CreatedBy && left.IdempotencyKey == right.IdempotencyKey &&
		left.Version == right.Version && equivalentMemoryStoreTime(left.CreatedAt, right.CreatedAt) &&
		equivalentMemoryStoreTime(left.UpdatedAt, right.UpdatedAt)
}

func validEraseResult(before, after memorykit.Memory, sources []memorykit.Source, command memorykit.VersionedCommand) bool {
	return after.ID == before.ID && after.Scope == before.Scope && after.Kind == before.Kind && after.Key == before.Key &&
		after.Status == memorykit.StatusInactive && after.Content == "" && len(sources) == 0 &&
		equivalentMemoryStoreTime(after.ValidFrom, before.ValidFrom) && equivalentMemoryStoreTime(after.ValidUntil, before.ValidUntil) &&
		after.Importance == before.Importance && after.Confidence == before.Confidence &&
		after.SourceAgentID == before.SourceAgentID && after.CreatedBy == before.CreatedBy && after.IdempotencyKey == before.IdempotencyKey &&
		after.Version == before.Version+1 && after.Version == command.ExpectedVersion+1 &&
		equivalentMemoryStoreTime(after.CreatedAt, before.CreatedAt) && equivalentMemoryStoreTime(after.UpdatedAt, command.Now)
}

func validEraseRevision(revision memorykit.Revision, erased memorykit.Memory, command memorykit.VersionedCommand) bool {
	return revision.Action == memorykit.RevisionErase && revision.ContentErased &&
		revision.MemoryID == command.ID && revision.Version == command.ExpectedVersion+1 &&
		revision.Actor == command.Actor && revision.Reason == command.Reason &&
		sameMemorySnapshot(revision.Snapshot, erased) && equivalentMemoryStoreTime(revision.CreatedAt, command.Now)
}

func equivalentMemoryStoreTime(left, right time.Time) bool {
	if left.IsZero() != right.IsZero() {
		return false
	}
	later, earlier := left, right
	if later.Before(earlier) {
		later, earlier = earlier, later
	}
	return later.Sub(earlier) <= time.Microsecond
}

func canonicalMemorySources(sources []memorykit.Source) []memorykit.Source {
	canonical := append([]memorykit.Source(nil), sources...)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Kind != canonical[j].Kind {
			return canonical[i].Kind < canonical[j].Kind
		}
		if canonical[i].Ref != canonical[j].Ref {
			return canonical[i].Ref < canonical[j].Ref
		}
		return canonical[i].EvidenceHash < canonical[j].EvidenceHash
	})
	return canonical
}

func sameMemorySourceSet(left, right []memorykit.Source) bool {
	if len(left) != len(right) {
		return false
	}
	canonicalLeft, canonicalRight := canonicalMemorySources(left), canonicalMemorySources(right)
	for index := range canonicalLeft {
		if canonicalLeft[index] != canonicalRight[index] {
			return false
		}
	}
	return true
}

func parseMemoryQuery(raw string, allowed map[string]struct{}) (url.Values, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	for key, entries := range values {
		if _, ok := allowed[key]; !ok || len(entries) != 1 {
			return nil, errors.New("invalid query parameter")
		}
	}
	return values, nil
}

func memoryQueryLimit(values url.Values, maximum int) (int, error) {
	entries, present := values["limit"]
	if !present {
		return maximum, nil
	}
	limit, err := strconv.Atoi(entries[0])
	if err != nil || limit <= 0 || limit > maximum {
		return 0, errors.New("invalid limit")
	}
	return limit, nil
}

func parseMemoryTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	return parseRequiredMemoryTime(raw)
}

func parseRequiredMemoryTime(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" || raw != strings.TrimSpace(raw) {
		return time.Time{}, errors.New("invalid time")
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func memorySourcesFromRequest(input []memorySourceRequest) []memorykit.Source {
	result := make([]memorykit.Source, len(input))
	for index, source := range input {
		result[index] = memorykit.Source{Kind: source.Kind, Ref: source.Ref, EvidenceHash: source.EvidenceHash}
	}
	return result
}

func memoryToResponse(memory memorykit.Memory, sources []memorykit.Source) memoryResponse {
	sources = canonicalMemorySources(sources)
	responseSources := make([]memorySourceResponse, len(sources))
	for index, source := range sources {
		responseSources[index] = memorySourceResponse{Kind: source.Kind, Ref: source.Ref, EvidenceHash: source.EvidenceHash}
	}
	return memoryResponse{
		ID: memory.ID, SubjectType: memory.Scope.SubjectType, SubjectID: memory.Scope.SubjectID,
		Kind: memory.Kind, Key: memory.Key, Status: memory.Status, Content: memory.Content,
		ValidFrom: formatMemoryTime(memory.ValidFrom), ValidUntil: formatMemoryTime(memory.ValidUntil),
		Importance: memory.Importance, Confidence: memory.Confidence,
		SourceAgentID: memory.SourceAgentID, CreatedBy: memory.CreatedBy, Sources: responseSources,
		Version: memory.Version, CreatedAt: formatMemoryTime(memory.CreatedAt), UpdatedAt: formatMemoryTime(memory.UpdatedAt),
	}
}

func memoryRevisionToResponse(revision memorykit.Revision) memoryRevisionResponse {
	return memoryRevisionResponse{
		MemoryID: revision.MemoryID, Version: revision.Version, Action: revision.Action,
		Actor: revision.Actor, Reason: revision.Reason,
		Snapshot: memoryToResponse(revision.Snapshot, nil), ContentErased: revision.ContentErased,
		CreatedAt: formatMemoryTime(revision.CreatedAt),
	}
}

func formatMemoryTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func validateMemorySnapshot(memory memorykit.Memory, limits memorykit.Limits) error {
	if memory.Version <= 0 || memory.CreatedAt.IsZero() || memory.UpdatedAt.IsZero() || memory.UpdatedAt.Before(memory.CreatedAt) {
		return errors.New("invalid memory snapshot")
	}
	content := memory.Content
	if content == "" && memory.Status == memorykit.StatusInactive {
		content = "[erased]"
	}
	return memorykit.ValidateCreateRequest(memorykit.CreateRequest{
		ID: memory.ID, Scope: memory.Scope, Kind: memory.Kind, Key: memory.Key, Status: memory.Status,
		Content: content, ValidFrom: memory.ValidFrom, ValidUntil: memory.ValidUntil,
		Importance: memory.Importance, Confidence: memory.Confidence,
		SourceAgentID: memory.SourceAgentID, Actor: memory.CreatedBy, IdempotencyKey: memory.IdempotencyKey,
	}, limits)
}

func validateHTTPCreateResult(memory memorykit.Memory, request memorykit.CreateRequest, limits memorykit.Limits) error {
	if validateMemorySnapshot(memory, limits) != nil || memory.ID != request.ID || memory.Scope != request.Scope ||
		memory.Kind != request.Kind || memory.Key != request.Key || memory.SourceAgentID != request.SourceAgentID ||
		memory.CreatedBy != request.Actor || memory.IdempotencyKey != request.IdempotencyKey ||
		!equivalentMemoryStoreTime(memory.CreatedAt, request.ValidFrom) {
		return errors.New("invalid memory create result")
	}
	if memory.Version == 1 {
		if memory.Status != memorykit.StatusActive || memory.Content != request.Content ||
			!equivalentMemoryStoreTime(memory.ValidFrom, request.ValidFrom) || !equivalentMemoryStoreTime(memory.ValidUntil, request.ValidUntil) ||
			memory.Importance != request.Importance || memory.Confidence != request.Confidence ||
			!equivalentMemoryStoreTime(memory.UpdatedAt, memory.CreatedAt) {
			return errors.New("invalid initial memory create result")
		}
		return nil
	}
	if request.IdempotencyKey == "" || memory.Content == "" ||
		(memory.Status != memorykit.StatusActive && memory.Status != memorykit.StatusInactive) {
		return errors.New("invalid memory create replay")
	}
	return nil
}

func validateMemoryRevision(revision memorykit.Revision, query memorykit.RevisionQuery, limits memorykit.Limits) error {
	if revision.MemoryID != query.MemoryID || revision.Version <= 0 || revision.Snapshot.ID != query.MemoryID ||
		revision.Snapshot.Scope != query.Scope || revision.Snapshot.Version != revision.Version || revision.CreatedAt.IsZero() ||
		!equivalentMemoryStoreTime(revision.CreatedAt, revision.Snapshot.UpdatedAt) {
		return errors.New("invalid memory revision")
	}
	if query.BeforeVersion > 0 && revision.Version >= query.BeforeVersion {
		return errors.New("invalid memory revision cursor result")
	}
	switch revision.Action {
	case memorykit.RevisionCreate, memorykit.RevisionCorrect:
	case memorykit.RevisionActivate:
		if revision.Snapshot.Status != memorykit.StatusActive {
			return errors.New("invalid activation revision")
		}
	case memorykit.RevisionSupersede, memorykit.RevisionForget, memorykit.RevisionDismiss:
		if revision.Snapshot.Status != memorykit.StatusInactive {
			return errors.New("invalid inactive revision")
		}
	case memorykit.RevisionErase:
		if revision.Snapshot.Status != memorykit.StatusInactive || !revision.ContentErased {
			return errors.New("invalid erase revision")
		}
	default:
		return errors.New("invalid memory revision action")
	}
	if revision.ContentErased {
		if revision.Snapshot.Content != "" {
			return errors.New("invalid erased memory revision")
		}
		erasedSnapshot := revision.Snapshot
		erasedSnapshot.Content = "[erased]"
		if err := validateMemorySnapshot(erasedSnapshot, limits); err != nil {
			return err
		}
	} else {
		if strings.TrimSpace(revision.Snapshot.Content) == "" {
			return errors.New("invalid blank memory revision")
		}
		if err := validateMemorySnapshot(revision.Snapshot, limits); err != nil {
			return err
		}
	}
	return memorykit.ValidateCommand(memorykit.VersionedCommand{
		Scope: query.Scope, ID: query.MemoryID, ExpectedVersion: revision.Version,
		Actor: revision.Actor, Reason: revision.Reason,
	}, limits)
}

func writeMemoryStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeMemoryError(w, http.StatusServiceUnavailable, "memory_unavailable", "project memory is temporarily unavailable")
	case errors.Is(err, errMemorySnapshotChanged), errors.Is(err, errMemoryInvalidStoreResult):
		writeMemoryError(w, http.StatusInternalServerError, "memory_integrity_error", "memory store returned invalid data")
	case errors.Is(err, errMemoryClockRollback):
		writeMemoryError(w, http.StatusInternalServerError, "memory_clock_error", "memory clock precedes stored data")
	case errors.Is(err, memorykit.ErrNotFound):
		writeMemoryError(w, http.StatusNotFound, "memory_not_found", "memory not found")
	case errors.Is(err, memorykit.ErrConflict):
		writeMemoryError(w, http.StatusConflict, "memory_conflict", "memory version conflict")
	case errors.Is(err, memorykit.ErrInvalidMemory), errors.Is(err, memorykit.ErrInvalidScope):
		writeMemoryError(w, http.StatusBadRequest, "invalid_request", "invalid memory request")
	case memorykit.IsRecoverable(err):
		writeMemoryError(w, http.StatusServiceUnavailable, "memory_unavailable", "project memory is temporarily unavailable")
	default:
		writeMemoryError(w, http.StatusInternalServerError, "memory_error", "project memory operation failed")
	}
}

func writeMemoryError(w http.ResponseWriter, status int, code, message string) {
	writeError(w, status, code, message)
}
