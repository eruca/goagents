package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/memorystore"
	"github.com/google/uuid"
)

func TestMemoryHandlersUseExactCapabilitiesAndLifecycle(t *testing.T) {
	authorizer := validMemoryAuthorizer()
	server, store := newMemoryHandlerServer(t, authorizer)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	activeID := seedMemory(t, store.Store, scope, memorykit.StatusActive, "active-key", "active content")
	candidateID := seedMemory(t, store.Store, scope, memorykit.StatusCandidate, "candidate-key", "candidate content")
	dismissID := seedMemory(t, store.Store, scope, memorykit.StatusCandidate, "dismiss-key", "dismiss content")

	assertRequestCapability(t, server, authorizer, store, http.MethodGet, "/projects/project-a/memories", "", http.StatusOK, memoryRead)
	if got := storeLastListStatus(t, store); got != memorykit.StatusActive {
		t.Fatalf("default list status = %q, want active", got)
	}
	assertRequestCapability(t, server, authorizer, store, http.MethodGet, "/projects/project-a/memories?status=candidate", "", http.StatusOK, memoryReview)
	activeGet := assertRequestCapability(t, server, authorizer, store, http.MethodGet, "/projects/project-a/memories/"+activeID, "", http.StatusOK, memoryRead)
	if !strings.Contains(activeGet.Body.String(), "active content") || !strings.Contains(activeGet.Body.String(), "sha256:active-key") {
		t.Fatalf("active GET omitted content/source: %s", activeGet.Body.String())
	}
	assertRequestCapabilities(t, server, authorizer, store, http.MethodGet, "/projects/project-a/memories/"+candidateID, "", http.StatusOK, []memoryCapability{memoryRead, memoryReview})
	assertRequestCapability(t, server, authorizer, store, http.MethodGet, "/projects/project-a/memories/"+activeID+"/revisions", "", http.StatusOK, memoryReview)

	created := assertRequestCapability(t, server, authorizer, store, http.MethodPost, "/projects/project-a/memories", validCreateMemoryBody("created-key", "request-1"), http.StatusCreated, memoryWriteExplicit)
	createdID := responseMemoryID(t, created)
	assertRequestCapability(t, server, authorizer, store, http.MethodPost, "/projects/project-a/memories/"+candidateID+"/activate", `{"expected_version":1,"reason":"approved"}`, http.StatusOK, memoryReview)
	assertRequestCapability(t, server, authorizer, store, http.MethodPost, "/projects/project-a/memories/"+dismissID+"/dismiss", `{"expected_version":1,"reason":"rejected"}`, http.StatusOK, memoryReview)
	correctBody := `{"expected_version":1,"content":"corrected content","valid_from":"2026-07-21T08:00:00Z","reason":"updated","importance":2,"confidence":0.8,"sources":[{"kind":"git","ref":"commit:def","evidence_hash":"sha256:def"}]}`
	assertRequestCapability(t, server, authorizer, store, http.MethodPost, "/projects/project-a/memories/"+createdID+"/correct", correctBody, http.StatusOK, memoryWriteExplicit)
	assertRequestCapability(t, server, authorizer, store, http.MethodPost, "/projects/project-a/memories/"+createdID+"/forget", `{"expected_version":2,"reason":"obsolete"}`, http.StatusOK, memoryWriteExplicit)
	assertRequestCapability(t, server, authorizer, store, http.MethodPost, "/projects/project-a/memories/"+createdID+"/erase", `{"expected_version":3,"reason":"privacy request"}`, http.StatusNoContent, memoryErase)
}

func TestMemoryHandlersRequireReviewBeforeReturningNonActive(t *testing.T) {
	authorizer := &capabilityMemoryAuthorizer{
		identity: memoryIdentity{TenantID: "tenant-a", Subject: "user-a"},
		deny:     map[memoryCapability]error{memoryReview: errMemoryDeniedForTest},
	}
	server, store := newMemoryHandlerServer(t, authorizer)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, store.Store, scope, memorykit.StatusCandidate, "secret-key", "must not leak")

	response := memoryRequest(t, server.Handler(), http.MethodGet, "/projects/project-a/memories/"+id, "")
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want 403", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "must not leak") {
		t.Fatalf("unauthorized response leaked content: %s", response.Body.String())
	}
	want := []memoryCapability{memoryRead, memoryReview}
	if !equalCapabilities(authorizer.capabilities, want) {
		t.Fatalf("capabilities=%v want=%v", authorizer.capabilities, want)
	}
}

func TestMemoryHandlersRejectUnknownAndUnboundedInput(t *testing.T) {
	server, store := newMemoryHandlerServer(t, validMemoryAuthorizer())
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, store.Store, scope, memorykit.StatusActive, "query-key", "query content")
	tests := []struct {
		name, method, target, body string
	}{
		{name: "unknown list query", method: http.MethodGet, target: "/projects/project-a/memories?cursor=x"},
		{name: "duplicate list query", method: http.MethodGet, target: "/projects/project-a/memories?limit=1&limit=2"},
		{name: "blank status cannot broaden list", method: http.MethodGet, target: "/projects/project-a/memories?status="},
		{name: "zero list limit", method: http.MethodGet, target: "/projects/project-a/memories?limit=0"},
		{name: "oversize list limit", method: http.MethodGet, target: "/projects/project-a/memories?limit=11"},
		{name: "unknown revision query", method: http.MethodGet, target: "/projects/project-a/memories/" + id + "/revisions?status=active"},
		{name: "zero revision cursor", method: http.MethodGet, target: "/projects/project-a/memories/" + id + "/revisions?before_version=0"},
		{name: "duplicate revision cursor", method: http.MethodGet, target: "/projects/project-a/memories/" + id + "/revisions?before_version=2&before_version=3"},
		{name: "unknown body field", method: http.MethodPost, target: "/projects/project-a/memories/" + id + "/forget", body: `{"expected_version":1,"reason":"x","extra":true}`},
		{name: "zero activate version", method: http.MethodPost, target: "/projects/project-a/memories/" + id + "/activate", body: `{"expected_version":0,"reason":"x"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store.calls = nil
			response := memoryRequest(t, server.Handler(), test.method, test.target, test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
			}
			if len(store.calls) != 0 {
				t.Fatalf("store calls=%v, want none", store.calls)
			}
		})
	}

	server.memory.MaxHTTPBodyBytes = 32
	store.calls = nil
	response := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories", validCreateMemoryBody("large", ""))
	if response.Code != http.StatusBadRequest || len(store.calls) != 0 {
		t.Fatalf("oversize body status=%d calls=%v body=%s", response.Code, store.calls, response.Body.String())
	}
}

func TestMemoryHandlersMapSafeErrors(t *testing.T) {
	secret := "postgres password=private"
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "conflict", err: errors.Join(memorykit.ErrConflict, errors.New(secret)), want: http.StatusConflict},
		{name: "not found", err: errors.Join(memorykit.ErrNotFound, errors.New(secret)), want: http.StatusNotFound},
		{name: "invalid", err: errors.Join(memorykit.ErrInvalidMemory, errors.New(secret)), want: http.StatusBadRequest},
		{name: "recoverable", err: &memorykit.BackendError{Op: "create", Recoverable: true, Err: errors.New(secret)}, want: http.StatusServiceUnavailable},
		{name: "other", err: errors.New(secret), want: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			config.Store = &createErrorMemoryStore{Store: config.Store, err: test.err}
			server := &Server{memory: config}
			response := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories", validCreateMemoryBody("error-key", ""))
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), test.want)
			}
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("response leaked store error: %s", response.Body.String())
			}
		})
	}
}

func TestMemoryHandlersValidateCreateCorrectAndActivateContent(t *testing.T) {
	validationErr := errors.New("secret credential rejected")
	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	validator := &acceptingMemoryContentValidator{err: validationErr}
	config.ContentValidator = validator
	server := &Server{memory: config}
	store := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	candidateID := seedMemory(t, store.Store, scope, memorykit.StatusCandidate, "candidate", "candidate content")
	activeID := seedMemory(t, store.Store, scope, memorykit.StatusActive, "active", "active content")
	tests := []struct{ name, target, body string }{
		{name: "create", target: "/projects/project-a/memories", body: validCreateMemoryBody("blocked", "")},
		{name: "correct", target: "/projects/project-a/memories/" + activeID + "/correct", body: `{"expected_version":1,"content":"blocked content","valid_from":"2026-07-21T08:00:00Z","reason":"x","importance":1,"confidence":1}`},
		{name: "activate", target: "/projects/project-a/memories/" + candidateID + "/activate", body: `{"expected_version":1,"reason":"x"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store.calls = nil
			response := memoryRequest(t, server.Handler(), http.MethodPost, test.target, test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "credential") || strings.Contains(response.Body.String(), "blocked content") || strings.Contains(response.Body.String(), "candidate content") {
				t.Fatalf("response leaked content/validator error: %s", response.Body.String())
			}
		})
	}
}

func TestMemoryHandlersRejectUnchangedMutationResults(t *testing.T) {
	tests := []struct {
		name       string
		status     memorykit.Status
		operation  string
		capability memoryCapability
		body       string
	}{
		{name: "activate", status: memorykit.StatusCandidate, operation: "activate", capability: memoryReview, body: `{"expected_version":1,"reason":"approve"}`},
		{name: "dismiss", status: memorykit.StatusCandidate, operation: "dismiss", capability: memoryReview, body: `{"expected_version":1,"reason":"dismiss"}`},
		{name: "forget", status: memorykit.StatusActive, operation: "forget", capability: memoryWriteExplicit, body: `{"expected_version":1,"reason":"forget"}`},
		{name: "correct", status: memorykit.StatusActive, operation: "correct", capability: memoryWriteExplicit, body: `{"expected_version":1,"content":"replacement","valid_from":"2026-07-21T08:00:00Z","reason":"correct","importance":2,"confidence":0.8}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			recorder := config.Store.(*recordingMemoryStore)
			scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
			id := seedMemory(t, recorder.Store, scope, test.status, "unchanged-"+test.name, "original")
			config.Store = &unchangedMutationMemoryStore{Store: recorder, operation: test.operation}
			server := &Server{memory: config}
			response := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories/"+id+"/"+test.operation, test.body)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s, want 500", response.Code, response.Body.String())
			}
		})
	}
}

func TestMemoryHandlersReturnErasedRevisionHistory(t *testing.T) {
	server, store := newMemoryHandlerServer(t, validMemoryAuthorizer())
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, store.Store, scope, memorykit.StatusActive, "erase-history", "private content")
	if err := store.Store.Erase(context.Background(), memorykit.VersionedCommand{
		Scope: scope, ID: id, ExpectedVersion: 1, Actor: "privacy-admin", Reason: "privacy request",
		Now: time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	response := memoryRequest(t, server.Handler(), http.MethodGet, "/projects/project-a/memories/"+id+"/revisions", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "private content") {
		t.Fatalf("erased history leaked content: %s", response.Body.String())
	}

	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	config.Store = &malformedRevisionMemoryStore{Store: config.Store}
	malformedServer := &Server{memory: config}
	malformedID := seedMemory(t, config.Store.(*malformedRevisionMemoryStore).Store.(*recordingMemoryStore).Store, scope, memorykit.StatusActive, "bad-history", "content")
	malformed := memoryRequest(t, malformedServer.Handler(), http.MethodGet, "/projects/project-a/memories/"+malformedID+"/revisions", "")
	if malformed.Code != http.StatusInternalServerError {
		t.Fatalf("malformed status=%d body=%s, want 500", malformed.Code, malformed.Body.String())
	}
}

func TestMemoryHandlersRejectMalformedCreateResults(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*memorykit.Memory)
	}{
		{name: "candidate", mutate: func(memory *memorykit.Memory) { memory.Status = memorykit.StatusCandidate }},
		{name: "wrong content", mutate: func(memory *memorykit.Memory) { memory.Content = "wrong" }},
		{name: "wrong creator", mutate: func(memory *memorykit.Memory) { memory.CreatedBy = "other" }},
		{name: "wrong idempotency", mutate: func(memory *memorykit.Memory) { memory.IdempotencyKey = "other" }},
		{name: "wrong version", mutate: func(memory *memorykit.Memory) { memory.Version = 2; memory.Status = memorykit.StatusCandidate }},
		{name: "foreign scope", mutate: func(memory *memorykit.Memory) { memory.Scope.SubjectID = "project-other" }},
		{name: "wrong timestamp", mutate: func(memory *memorykit.Memory) {
			memory.CreatedAt = memory.CreatedAt.Add(time.Hour)
			memory.UpdatedAt = memory.CreatedAt
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			config.Store = &malformedCreateResultMemoryStore{Store: config.Store, mutate: test.mutate}
			server := &Server{memory: config}
			response := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories", validCreateMemoryBody("malformed-"+test.name, ""))
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s, want 500", response.Code, response.Body.String())
			}
		})
	}
}

func TestMemoryHandlersCorrectReturnsOnlyPersistedSources(t *testing.T) {
	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	recorder := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "correct-sources", "original")
	config.Store = &wrongCorrectSourcesMemoryStore{Store: recorder}
	server := &Server{memory: config}
	body := `{"expected_version":1,"content":"corrected","valid_from":"2026-07-21T08:00:00Z","reason":"correct","importance":1,"confidence":1,"sources":[{"kind":"git","ref":"commit:expected","evidence_hash":"sha256:expected"}]}`
	response := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories/"+id+"/correct", body)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "commit:expected") || strings.Contains(response.Body.String(), "commit:wrong") {
		t.Fatalf("integrity response leaked sources: %s", response.Body.String())
	}
}

func TestMemoryHandlersCreateIdempotencyUsesScopeBoundDeterministicID(t *testing.T) {
	limits := memoryLimitsForTest()
	base, err := memorystore.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	shared := &recordingMemoryStore{Store: base}
	newConfig := func(identity memoryIdentity, now time.Time) *memoryRuntimeConfig {
		return &memoryRuntimeConfig{
			Store:            shared,
			Authorizer:       &recordingMemoryAuthorizer{identity: identity},
			ContentValidator: &acceptingMemoryContentValidator{}, Limits: limits,
			NewID: func() string { return uuid.NewString() }, Now: func() time.Time { return now }, MaxHTTPBodyBytes: 4096,
		}
	}
	firstServer := &Server{memory: newConfig(memoryIdentity{TenantID: "tenant-a", Subject: "user-a"}, time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC))}
	secondServer := &Server{memory: newConfig(memoryIdentity{TenantID: "tenant-a", Subject: "user-a"}, time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC))}
	body := validCreateMemoryBody("stable-key", "http-request-1")
	first := memoryRequest(t, firstServer.Handler(), http.MethodPost, "/projects/project-a/memories", body)
	second := memoryRequest(t, secondServer.Handler(), http.MethodPost, "/projects/project-a/memories", body)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("replay statuses=%d/%d bodies=%s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	firstID, secondID := responseMemoryID(t, first), responseMemoryID(t, second)
	if firstID != secondID {
		t.Fatalf("replay IDs=%q/%q", firstID, secondID)
	}
	revisions, err := base.Revisions(context.Background(), memorykit.RevisionQuery{
		Scope:    memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"},
		MemoryID: firstID, Limit: limits.MaxListItems,
	})
	if err != nil || len(revisions) != 1 {
		t.Fatalf("revisions=%#v err=%v, want one", revisions, err)
	}

	changed := strings.Replace(body, "run go test", "run go test ./...", 1)
	conflict := memoryRequest(t, secondServer.Handler(), http.MethodPost, "/projects/project-a/memories", changed)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("changed replay status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	changedReason := strings.Replace(body, `"reason":"explicit"`, `"reason":"different"`, 1)
	if response := memoryRequest(t, secondServer.Handler(), http.MethodPost, "/projects/project-a/memories", changedReason); response.Code != http.StatusConflict {
		t.Fatalf("changed reason status=%d body=%s", response.Code, response.Body.String())
	}
	changedSource := strings.Replace(body, "commit:abc", "commit:different", 1)
	if response := memoryRequest(t, secondServer.Handler(), http.MethodPost, "/projects/project-a/memories", changedSource); response.Code != http.StatusConflict {
		t.Fatalf("changed source status=%d body=%s", response.Code, response.Body.String())
	}

	corrected, err := base.Correct(context.Background(), memorykit.CorrectRequest{
		Command: memorykit.VersionedCommand{
			Scope: memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"},
			ID:    firstID, ExpectedVersion: 1, Actor: "user-a", Reason: "later correction",
			Now: time.Date(2026, 7, 21, 9, 30, 0, 0, time.UTC),
		},
		Content: "corrected after create", ValidFrom: time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC),
		Importance: 2, Confidence: 0.9,
		Sources: []memorykit.Source{{Kind: "git", Ref: "commit:corrected", EvidenceHash: "sha256:corrected"}},
	})
	if err != nil || corrected.Version != 2 {
		t.Fatalf("Correct()=%#v err=%v", corrected, err)
	}
	afterCorrection := memoryRequest(t, secondServer.Handler(), http.MethodPost, "/projects/project-a/memories", body)
	if afterCorrection.Code != http.StatusCreated || !strings.Contains(afterCorrection.Body.String(), "corrected after create") || !strings.Contains(afterCorrection.Body.String(), "commit:corrected") {
		t.Fatalf("post-correction replay status=%d body=%s", afterCorrection.Code, afterCorrection.Body.String())
	}
	revisions, err = base.Revisions(context.Background(), memorykit.RevisionQuery{
		Scope:    memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"},
		MemoryID: firstID, Limit: limits.MaxListItems,
	})
	if err != nil || len(revisions) != 2 {
		t.Fatalf("post-correction revisions=%#v err=%v, want create+correct", revisions, err)
	}
	forgotten, err := base.Forget(context.Background(), memorykit.VersionedCommand{
		Scope: memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"},
		ID:    firstID, ExpectedVersion: 2, Actor: "user-a", Reason: "later forget",
		Now: time.Date(2026, 7, 21, 9, 45, 0, 0, time.UTC),
	})
	if err != nil || forgotten.Status != memorykit.StatusInactive || forgotten.Version != 3 {
		t.Fatalf("Forget()=%#v err=%v", forgotten, err)
	}
	inactiveReplay := memoryRequest(t, secondServer.Handler(), http.MethodPost, "/projects/project-a/memories", body)
	if inactiveReplay.Code != http.StatusCreated || !strings.Contains(inactiveReplay.Body.String(), `"status":"inactive"`) {
		t.Fatalf("inactive replay status=%d body=%s", inactiveReplay.Code, inactiveReplay.Body.String())
	}
	if err := base.Erase(context.Background(), memorykit.VersionedCommand{
		Scope: memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"},
		ID:    firstID, ExpectedVersion: 3, Actor: "privacy-admin", Reason: "erase",
		Now: time.Date(2026, 7, 21, 9, 50, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	erasedReplay := memoryRequest(t, secondServer.Handler(), http.MethodPost, "/projects/project-a/memories", body)
	if erasedReplay.Code != http.StatusConflict {
		t.Fatalf("erased replay status=%d body=%s", erasedReplay.Code, erasedReplay.Body.String())
	}

	otherTenant := &Server{memory: newConfig(memoryIdentity{TenantID: "tenant-b", Subject: "user-b"}, time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC))}
	other := memoryRequest(t, otherTenant.Handler(), http.MethodPost, "/projects/project-a/memories", body)
	if other.Code != http.StatusCreated || responseMemoryID(t, other) == firstID {
		t.Fatalf("other scope status=%d id=%q first=%q", other.Code, responseMemoryID(t, other), firstID)
	}
}

type capabilityMemoryAuthorizer struct {
	identity     memoryIdentity
	deny         map[memoryCapability]error
	capabilities []memoryCapability
}

func (a *capabilityMemoryAuthorizer) AuthorizeMemory(_ context.Context, _ string, _ string, capability memoryCapability) (memoryIdentity, error) {
	a.capabilities = append(a.capabilities, capability)
	if err := a.deny[capability]; err != nil {
		return memoryIdentity{}, err
	}
	return a.identity, nil
}

type createErrorMemoryStore struct {
	memorykit.Store
	err error
}

type unchangedMutationMemoryStore struct {
	memorykit.Store
	operation string
}

type malformedCreateResultMemoryStore struct {
	memorykit.Store
	mutate func(*memorykit.Memory)
}

func (s *malformedCreateResultMemoryStore) Create(ctx context.Context, request memorykit.CreateRequest) (memorykit.Memory, error) {
	memory, err := s.Store.Create(ctx, request)
	if err == nil {
		s.mutate(&memory)
	}
	return memory, err
}

type malformedRevisionMemoryStore struct{ memorykit.Store }

func (s *malformedRevisionMemoryStore) Revisions(ctx context.Context, query memorykit.RevisionQuery) ([]memorykit.Revision, error) {
	revisions, err := s.Store.Revisions(ctx, query)
	if err == nil && len(revisions) > 0 {
		revisions[0].ContentErased = false
		revisions[0].Snapshot.Content = ""
	}
	return revisions, err
}

type wrongCorrectSourcesMemoryStore struct{ memorykit.Store }

func (s *wrongCorrectSourcesMemoryStore) Sources(context.Context, memorykit.Scope, string) ([]memorykit.Source, error) {
	return []memorykit.Source{{Kind: "git", Ref: "commit:wrong", EvidenceHash: "sha256:wrong"}}, nil
}

func (s *unchangedMutationMemoryStore) unchanged(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	return s.Store.Get(ctx, command.Scope, command.ID)
}

func (s *unchangedMutationMemoryStore) Activate(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	if s.operation == "activate" {
		return s.unchanged(ctx, command)
	}
	return s.Store.Activate(ctx, command)
}

func (s *unchangedMutationMemoryStore) Dismiss(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	if s.operation == "dismiss" {
		return s.unchanged(ctx, command)
	}
	return s.Store.Dismiss(ctx, command)
}

func (s *unchangedMutationMemoryStore) Forget(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	if s.operation == "forget" {
		return s.unchanged(ctx, command)
	}
	return s.Store.Forget(ctx, command)
}

func (s *unchangedMutationMemoryStore) Correct(ctx context.Context, request memorykit.CorrectRequest) (memorykit.Memory, error) {
	if s.operation == "correct" {
		return s.unchanged(ctx, request.Command)
	}
	return s.Store.Correct(ctx, request)
}

func (s *createErrorMemoryStore) Create(context.Context, memorykit.CreateRequest) (memorykit.Memory, error) {
	return memorykit.Memory{}, s.err
}

func seedMemory(t *testing.T, store *memorystore.Store, scope memorykit.Scope, status memorykit.Status, key, content string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := store.Create(context.Background(), memorykit.CreateRequest{
		ID: id, Scope: scope, Kind: memorykit.KindFact, Key: key, Status: status, Content: content,
		ValidFrom: time.Date(2026, 7, 21, 7, 0, 0, 0, time.UTC), Importance: 1, Confidence: 1,
		Actor: "seed-user", Reason: "seed", Sources: []memorykit.Source{{Kind: "git", Ref: "commit:" + key, EvidenceHash: "sha256:" + key}},
		Now: time.Date(2026, 7, 21, 7, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func assertRequestCapability(t *testing.T, server *Server, authorizer *recordingMemoryAuthorizer, store *recordingMemoryStore, method, target, body string, status int, capability memoryCapability) *httptest.ResponseRecorder {
	t.Helper()
	return assertRequestCapabilities(t, server, authorizer, store, method, target, body, status, []memoryCapability{capability})
}

func assertRequestCapabilities(t *testing.T, server *Server, authorizer *recordingMemoryAuthorizer, store *recordingMemoryStore, method, target, body string, status int, capabilities []memoryCapability) *httptest.ResponseRecorder {
	t.Helper()
	authorizer.calls = nil
	store.calls = nil
	response := memoryRequest(t, server.Handler(), method, target, body)
	if response.Code != status {
		t.Fatalf("%s %s status=%d body=%s, want %d", method, target, response.Code, response.Body.String(), status)
	}
	got := make([]memoryCapability, len(authorizer.calls))
	for index := range authorizer.calls {
		got[index] = authorizer.calls[index].capability
	}
	if !equalCapabilities(got, capabilities) {
		t.Fatalf("%s %s capabilities=%v want=%v", method, target, got, capabilities)
	}
	return response
}

func equalCapabilities(got, want []memoryCapability) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func responseMemoryID(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var decoded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || decoded.ID == "" {
		t.Fatalf("decode memory ID: %v body=%s", err, response.Body.String())
	}
	return decoded.ID
}

func storeLastListStatus(t *testing.T, store *recordingMemoryStore) memorykit.Status {
	t.Helper()
	if len(store.listQueries) == 0 {
		t.Fatal("List was not called")
	}
	return store.listQueries[len(store.listQueries)-1].Status
}
