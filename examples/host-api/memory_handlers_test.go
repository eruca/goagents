package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/memorystore"
	"github.com/eruca/goagents/memorykit/pgstore"
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

func TestMemoryHandlersRequireExactJSONShapeAndPresence(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "duplicate top key", body: `{"kind":"fact","kind":"lesson","key":"json-1","content":"content","reason":"reason","importance":0,"confidence":0}`},
		{name: "wrong case top key", body: `{"Kind":"fact","key":"json-2","content":"content","reason":"reason","importance":0,"confidence":0}`},
		{name: "missing reason", body: `{"kind":"fact","key":"json-3","content":"content","importance":0,"confidence":0}`},
		{name: "missing importance", body: `{"kind":"fact","key":"json-4","content":"content","reason":"reason","confidence":0}`},
		{name: "missing confidence", body: `{"kind":"fact","key":"json-5","content":"content","reason":"reason","importance":0}`},
		{name: "duplicate source key", body: `{"kind":"fact","key":"json-6","content":"content","reason":"reason","importance":0,"confidence":0,"sources":[{"kind":"git","kind":"todo","ref":"ref"}]}`},
		{name: "wrong case source key", body: `{"kind":"fact","key":"json-7","content":"content","reason":"reason","importance":0,"confidence":0,"sources":[{"Kind":"git","ref":"ref"}]}`},
		{name: "missing source ref", body: `{"kind":"fact","key":"json-8","content":"content","reason":"reason","importance":0,"confidence":0,"sources":[{"kind":"git"}]}`},
		{name: "null sources", body: `{"kind":"fact","key":"json-9","content":"content","reason":"reason","importance":0,"confidence":0,"sources":null}`},
		{name: "trailing value", body: `{"kind":"fact","key":"json-10","content":"content","reason":"reason","importance":0,"confidence":0} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, store := newMemoryHandlerServer(t, validMemoryAuthorizer())
			response := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories", test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", response.Code, response.Body.String())
			}
			if response.Body.String() != "{\"error\":\"invalid_json\",\"message\":\"invalid memory request body\"}\n" {
				t.Fatalf("unsafe/noncanonical JSON error: %s", response.Body.String())
			}
			if len(store.calls) != 0 {
				t.Fatalf("store calls=%v, want none", store.calls)
			}
		})
	}

	server, _ := newMemoryHandlerServer(t, validMemoryAuthorizer())
	validZero := `{"kind":"fact","key":"json-zero","content":"content","reason":"","importance":0,"confidence":0}`
	response := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories", validZero)
	if response.Code != http.StatusCreated {
		t.Fatalf("explicit zero status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMemoryHandlersRejectNullScalarFields(t *testing.T) {
	id := uuid.NewString()
	create := `{"kind":"fact","key":"null-create","content":"content","valid_until":"2026-07-22T08:00:00Z","reason":"reason","idempotency_key":"request-1","importance":1,"confidence":1,"sources":[{"kind":"git","ref":"commit:1","evidence_hash":"hash:1"}]}`
	correct := `{"expected_version":1,"content":"corrected","valid_from":"2026-07-21T08:00:00Z","valid_until":"2026-07-22T08:00:00Z","reason":"reason","importance":1,"confidence":1,"sources":[{"kind":"git","ref":"commit:2","evidence_hash":"hash:2"}]}`
	versioned := `{"expected_version":1,"reason":"reason"}`
	tests := []struct {
		name, target, body string
	}{
		{name: "create reason", target: "/projects/project-a/memories", body: strings.Replace(create, `"reason":"reason"`, `"reason":null`, 1)},
		{name: "create importance", target: "/projects/project-a/memories", body: strings.Replace(create, `"importance":1`, `"importance":null`, 1)},
		{name: "create confidence", target: "/projects/project-a/memories", body: strings.Replace(create, `"confidence":1`, `"confidence":null`, 1)},
		{name: "create optional valid until", target: "/projects/project-a/memories", body: strings.Replace(create, `"valid_until":"2026-07-22T08:00:00Z"`, `"valid_until":null`, 1)},
		{name: "create optional idempotency", target: "/projects/project-a/memories", body: strings.Replace(create, `"idempotency_key":"request-1"`, `"idempotency_key":null`, 1)},
		{name: "source optional evidence hash", target: "/projects/project-a/memories", body: strings.Replace(create, `"evidence_hash":"hash:1"`, `"evidence_hash":null`, 1)},
		{name: "source required kind", target: "/projects/project-a/memories", body: strings.Replace(create, `"kind":"git"`, `"kind":null`, 1)},
		{name: "source required ref", target: "/projects/project-a/memories", body: strings.Replace(create, `"ref":"commit:1"`, `"ref":null`, 1)},
		{name: "correct reason", target: "/projects/project-a/memories/" + id + "/correct", body: strings.Replace(correct, `"reason":"reason"`, `"reason":null`, 1)},
		{name: "correct importance", target: "/projects/project-a/memories/" + id + "/correct", body: strings.Replace(correct, `"importance":1`, `"importance":null`, 1)},
		{name: "correct confidence", target: "/projects/project-a/memories/" + id + "/correct", body: strings.Replace(correct, `"confidence":1`, `"confidence":null`, 1)},
		{name: "correct optional valid until", target: "/projects/project-a/memories/" + id + "/correct", body: strings.Replace(correct, `"valid_until":"2026-07-22T08:00:00Z"`, `"valid_until":null`, 1)},
		{name: "versioned reason", target: "/projects/project-a/memories/" + id + "/forget", body: strings.Replace(versioned, `"reason":"reason"`, `"reason":null`, 1)},
		{name: "versioned expected version", target: "/projects/project-a/memories/" + id + "/forget", body: strings.Replace(versioned, `"expected_version":1`, `"expected_version":null`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, store := newMemoryHandlerServer(t, validMemoryAuthorizer())
			response := memoryRequest(t, server.Handler(), http.MethodPost, test.target, test.body)
			if response.Code != http.StatusBadRequest || response.Body.String() != "{\"error\":\"invalid_json\",\"message\":\"invalid memory request body\"}\n" {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if len(store.calls) != 0 {
				t.Fatalf("store calls=%v, want none", store.calls)
			}
		})
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
		{name: "canceled", err: context.Canceled, want: http.StatusServiceUnavailable},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusServiceUnavailable},
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

func TestMemoryHandlersMapSafeContextErrorsFromAuthorizationAndValidation(t *testing.T) {
	for _, contextErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(contextErr.Error(), func(t *testing.T) {
			authConfig := validMemoryRuntimeConfig(t, &recordingMemoryAuthorizer{err: contextErr})
			authResponse := memoryRequest(t, (&Server{memory: authConfig}).Handler(), http.MethodGet, "/projects/project-a/memories", "")
			if authResponse.Code != http.StatusServiceUnavailable {
				t.Fatalf("authorization status=%d body=%s", authResponse.Code, authResponse.Body.String())
			}

			validationConfig := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			validationConfig.ContentValidator = &acceptingMemoryContentValidator{err: contextErr}
			validationResponse := memoryRequest(t, (&Server{memory: validationConfig}).Handler(), http.MethodPost,
				"/projects/project-a/memories", validCreateMemoryBody("context-error", ""))
			if validationResponse.Code != http.StatusServiceUnavailable {
				t.Fatalf("validation status=%d body=%s", validationResponse.Code, validationResponse.Body.String())
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

func TestMemoryHandlersProveEraseBeforeReturningNoContent(t *testing.T) {
	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	recorder := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "noop-erase", "private content")
	config.Store = &noOpEraseMemoryStore{Store: recorder}

	response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodPost,
		"/projects/project-a/memories/"+id+"/erase", `{"expected_version":1,"reason":"privacy request"}`)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", response.Code, response.Body.String())
	}
}

func TestMemoryHandlersRejectMalformedRevisionSequenceAndTombstoneHistory(t *testing.T) {
	tests := []struct {
		name   string
		erase  bool
		mutate func([]memorykit.Revision)
	}{
		{name: "duplicate version", mutate: func(revisions []memorykit.Revision) { revisions[1] = revisions[0] }},
		{name: "created at mismatch", mutate: func(revisions []memorykit.Revision) { revisions[0].CreatedAt = revisions[0].CreatedAt.Add(time.Second) }},
		{name: "action status mismatch", mutate: func(revisions []memorykit.Revision) { revisions[0].Action = memorykit.RevisionDismiss }},
		{name: "create above version one", mutate: func(revisions []memorykit.Revision) { revisions[0].Action = memorykit.RevisionCreate }},
		{name: "tombstone has nonerased history", erase: true, mutate: func(revisions []memorykit.Revision) {
			revisions[len(revisions)-1].ContentErased = false
			revisions[len(revisions)-1].Snapshot.Content = "restored content"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			recorder := config.Store.(*recordingMemoryStore)
			scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
			id := seedMemory(t, recorder.Store, scope, memorykit.StatusCandidate, "bad-revisions-"+test.name, "content")
			if _, err := recorder.Store.Activate(context.Background(), memorykit.VersionedCommand{
				Scope: scope, ID: id, ExpectedVersion: 1, Actor: "reviewer", Reason: "approve",
				Now: time.Date(2026, 7, 21, 7, 30, 0, 0, time.UTC),
			}); err != nil {
				t.Fatal(err)
			}
			if test.erase {
				if err := recorder.Store.Erase(context.Background(), memorykit.VersionedCommand{
					Scope: scope, ID: id, ExpectedVersion: 2, Actor: "privacy-admin", Reason: "erase",
					Now: time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC),
				}); err != nil {
					t.Fatal(err)
				}
			}
			config.Store = &mutatingRevisionMemoryStore{Store: recorder, mutate: test.mutate}
			response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodGet,
				"/projects/project-a/memories/"+id+"/revisions", "")
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s, want 500", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "restored content") {
				t.Fatalf("malformed history leaked content: %s", response.Body.String())
			}
		})
	}

	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	recorder := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, recorder.Store, scope, memorykit.StatusCandidate, "missing-latest-revision", "content")
	if _, err := recorder.Store.Activate(context.Background(), memorykit.VersionedCommand{
		Scope: scope, ID: id, ExpectedVersion: 1, Actor: "reviewer", Reason: "approve",
		Now: time.Date(2026, 7, 21, 7, 30, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	config.Store = &omitLatestRevisionMemoryStore{Store: recorder}
	response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodGet,
		"/projects/project-a/memories/"+id+"/revisions", "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("missing latest status=%d body=%s, want 500", response.Code, response.Body.String())
	}
}

func TestMemoryHandlersRejectClockRollbackBeforeMutation(t *testing.T) {
	tests := []struct {
		status    memorykit.Status
		operation string
		body      string
	}{
		{status: memorykit.StatusCandidate, operation: "activate", body: `{"expected_version":1,"reason":"approve"}`},
		{status: memorykit.StatusCandidate, operation: "dismiss", body: `{"expected_version":1,"reason":"dismiss"}`},
		{status: memorykit.StatusActive, operation: "correct", body: `{"expected_version":1,"content":"replacement","valid_from":"2026-07-21T06:00:00Z","reason":"correct","importance":1,"confidence":1}`},
		{status: memorykit.StatusActive, operation: "forget", body: `{"expected_version":1,"reason":"forget"}`},
		{status: memorykit.StatusActive, operation: "erase", body: `{"expected_version":1,"reason":"erase"}`},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			config.Now = func() time.Time { return time.Date(2026, 7, 21, 6, 0, 0, 0, time.UTC) }
			recorder := config.Store.(*recordingMemoryStore)
			scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
			id := seedMemory(t, recorder.Store, scope, test.status, "rollback-"+test.operation, "content")
			recorder.calls = nil
			response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodPost,
				"/projects/project-a/memories/"+id+"/"+test.operation, test.body)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s, want 500", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), `"error":"memory_integrity_error"`) {
				t.Fatalf("noncanonical clock error: %s", response.Body.String())
			}
			for _, call := range recorder.calls {
				if call == test.operation {
					t.Fatalf("mutation called after clock rollback: %v", recorder.calls)
				}
			}
		})
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

func TestMemoryHandlersTreatSourcesAsCanonicalSet(t *testing.T) {
	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	ordered := &canonicalSourceOrderMemoryStore{Store: config.Store}
	config.Store = ordered
	server := &Server{memory: config}
	createBody := `{"kind":"fact","key":"source-set","content":"content","reason":"create","importance":1,"confidence":1,"sources":[{"kind":"todo","ref":"todo:2","evidence_hash":"hash:2"},{"kind":"git","ref":"commit:1","evidence_hash":"hash:1"}]}`
	created := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories", createBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	createdID := responseMemoryID(t, created)
	if got := responseSourceKinds(t, created); !equalStrings(got, []string{"git", "todo"}) {
		t.Fatalf("create source order=%v", got)
	}
	correctBody := `{"expected_version":1,"content":"corrected","valid_from":"2026-07-21T08:00:00Z","reason":"correct","importance":1,"confidence":1,"sources":[{"kind":"todo","ref":"todo:4","evidence_hash":"hash:4"},{"kind":"git","ref":"commit:3","evidence_hash":"hash:3"}]}`
	corrected := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories/"+createdID+"/correct", correctBody)
	if corrected.Code != http.StatusOK {
		t.Fatalf("correct status=%d body=%s", corrected.Code, corrected.Body.String())
	}
	if got := responseSourceKinds(t, corrected); !equalStrings(got, []string{"git", "todo"}) {
		t.Fatalf("correct source order=%v", got)
	}
	if ordered.sourcesCalls < 2 {
		t.Fatalf("Sources calls=%d, want persisted reads for create and correct", ordered.sourcesCalls)
	}
}

func TestMemoryHandlersCorrectUnorderedSourcesWithRealPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("MEMORYKIT_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("set MEMORYKIT_POSTGRES_TEST_DSN")
	}
	limits := memoryLimitsForTest()
	store, err := pgstore.Open(context.Background(), dsn, pgstore.Config{
		Limits: limits, EmbeddingProfileID: "host-handler-test", EmbeddingDimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	config := &memoryRuntimeConfig{
		Store: store,
		Authorizer: &recordingMemoryAuthorizer{identity: memoryIdentity{
			TenantID: "host-handler-" + uuid.NewString(), Subject: "user-a",
		}},
		ContentValidator: &acceptingMemoryContentValidator{}, Limits: limits,
		NewID:            func() string { return uuid.NewString() },
		Now:              func() time.Time { return time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC) },
		MaxHTTPBodyBytes: 4096,
	}
	server := &Server{memory: &memoryRuntime{memoryRuntimeConfig: *config}}
	created := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories",
		`{"kind":"fact","key":"pg-source-set","content":"content","reason":"create","importance":1,"confidence":1,"sources":[{"kind":"todo","ref":"todo:2","evidence_hash":"hash:2"},{"kind":"git","ref":"commit:1","evidence_hash":"hash:1"}]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	id := responseMemoryID(t, created)
	corrected := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories/"+id+"/correct",
		`{"expected_version":1,"content":"corrected","valid_from":"2026-07-21T08:00:00Z","reason":"correct","importance":1,"confidence":1,"sources":[{"kind":"todo","ref":"todo:4","evidence_hash":"hash:4"},{"kind":"git","ref":"commit:3","evidence_hash":"hash:3"}]}`)
	if corrected.Code != http.StatusOK || !equalStrings(responseSourceKinds(t, corrected), []string{"git", "todo"}) {
		t.Fatalf("correct status=%d body=%s", corrected.Code, corrected.Body.String())
	}
}

func TestMemoryHandlersRetryStableGetWithoutReturningOldContent(t *testing.T) {
	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	recorder := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "stable-get", "old content")
	old, err := recorder.Store.Get(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.Content = "new content"
	newer.Version = 2
	newer.UpdatedAt = old.UpdatedAt.Add(time.Minute)
	config.Store = &scriptedSnapshotMemoryStore{Store: recorder, getResults: []memorykit.Memory{old, newer, newer, newer}}
	server := &Server{memory: config}

	response := memoryRequest(t, server.Handler(), http.MethodGet, "/projects/project-a/memories/"+id, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "new content") || strings.Contains(response.Body.String(), "old content") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMemoryHandlersFailClosedWhenGetNeverStabilizes(t *testing.T) {
	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	recorder := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "unstable-get", "v1 content")
	v1, err := recorder.Store.Get(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	v2, v3 := v1, v1
	v2.Content, v2.Version, v2.UpdatedAt = "v2 content", 2, v1.UpdatedAt.Add(time.Minute)
	v3.Content, v3.Version, v3.UpdatedAt = "v3 content", 3, v1.UpdatedAt.Add(2*time.Minute)
	config.Store = &scriptedSnapshotMemoryStore{Store: recorder, getResults: []memorykit.Memory{v1, v2, v2, v3}}
	server := &Server{memory: config}

	response := memoryRequest(t, server.Handler(), http.MethodGet, "/projects/project-a/memories/"+id, "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "v1 content") || strings.Contains(response.Body.String(), "v2 content") || strings.Contains(response.Body.String(), "v3 content") {
		t.Fatalf("unstable response leaked content: %s", response.Body.String())
	}
}

func TestMemoryHandlersReauthorizeEachGetAttempt(t *testing.T) {
	authorizer := &capabilityMemoryAuthorizer{identity: memoryIdentity{TenantID: "tenant-a", Subject: "user-a"}, deny: map[memoryCapability]error{}}
	config := validMemoryRuntimeConfig(t, authorizer)
	recorder := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, recorder.Store, scope, memorykit.StatusCandidate, "reauthorize", "candidate content")
	candidate, err := recorder.Store.Get(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	active := candidate
	active.Status, active.Version, active.UpdatedAt = memorykit.StatusActive, 2, candidate.UpdatedAt.Add(time.Minute)
	config.Store = &scriptedSnapshotMemoryStore{Store: recorder, getResults: []memorykit.Memory{candidate, active, active, active}}
	server := &Server{memory: config}

	response := memoryRequest(t, server.Handler(), http.MethodGet, "/projects/project-a/memories/"+id, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	want := []memoryCapability{memoryRead, memoryReview, memoryRead}
	if !equalCapabilities(authorizer.capabilities, want) {
		t.Fatalf("capabilities=%v want=%v", authorizer.capabilities, want)
	}
}

func TestMemoryHandlersRetryStableListAndValidateOrder(t *testing.T) {
	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	recorder := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "stable-list", "old list content")
	old, err := recorder.Store.Get(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.Content, newer.Version, newer.UpdatedAt = "new list content", 2, old.UpdatedAt.Add(time.Minute)
	config.Store = &scriptedSnapshotMemoryStore{
		Store: recorder, listResults: [][]memorykit.Memory{{old}, {newer}},
		getResults: []memorykit.Memory{newer, newer},
	}
	server := &Server{memory: config}
	response := memoryRequest(t, server.Handler(), http.MethodGet, "/projects/project-a/memories", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "new list content") || strings.Contains(response.Body.String(), "old list content") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	firstID := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "ordered-a", "a")
	secondID := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "ordered-b", "b")
	first, _ := recorder.Store.Get(context.Background(), scope, firstID)
	second, _ := recorder.Store.Get(context.Background(), scope, secondID)
	first.UpdatedAt = second.UpdatedAt.Add(-time.Minute)
	badOrderConfig := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	badOrderConfig.Store = &scriptedSnapshotMemoryStore{Store: recorder, listResults: [][]memorykit.Memory{{first, second}}}
	badOrder := memoryRequest(t, (&Server{memory: badOrderConfig}).Handler(), http.MethodGet, "/projects/project-a/memories", "")
	if badOrder.Code != http.StatusInternalServerError {
		t.Fatalf("bad order status=%d body=%s", badOrder.Code, badOrder.Body.String())
	}
}

func TestMemoryHandlersReturnConflictAfterConcurrentMutationPreRead(t *testing.T) {
	tests := []struct {
		operation string
		body      string
	}{
		{operation: "correct", body: `{"expected_version":1,"content":"replacement","valid_from":"2026-07-21T08:00:00Z","reason":"correct","importance":1,"confidence":1}`},
		{operation: "forget", body: `{"expected_version":1,"reason":"forget"}`},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			recorder := config.Store.(*recordingMemoryStore)
			scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
			id := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "pre-race-"+test.operation, "v1 content")
			v1, err := recorder.Store.Get(context.Background(), scope, id)
			if err != nil {
				t.Fatal(err)
			}
			v2 := v1
			v2.Content, v2.Version, v2.UpdatedAt = "v2 content", 2, v1.UpdatedAt.Add(time.Minute)
			config.Store = &scriptedSnapshotMemoryStore{Store: recorder, getResults: []memorykit.Memory{v1, v2, v2, v2}}
			response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodPost,
				"/projects/project-a/memories/"+id+"/"+test.operation, test.body)
			if response.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409", response.Code, response.Body.String())
			}
			for _, call := range recorder.calls {
				if call == test.operation {
					t.Fatalf("mutation called after advanced pre-read: %v", recorder.calls)
				}
			}
		})
	}

	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	recorder := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "continuous-pre-race", "v1")
	v1, _ := recorder.Store.Get(context.Background(), scope, id)
	v2, v3 := v1, v1
	v2.Content, v2.Version, v2.UpdatedAt = "v2", 2, v1.UpdatedAt.Add(time.Minute)
	v3.Content, v3.Version, v3.UpdatedAt = "v3", 3, v1.UpdatedAt.Add(2*time.Minute)
	config.Store = &scriptedSnapshotMemoryStore{Store: recorder, getResults: []memorykit.Memory{v1, v2, v2, v3}}
	response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodPost,
		"/projects/project-a/memories/"+id+"/forget", `{"expected_version":1,"reason":"forget"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("continuous status=%d body=%s, want 409", response.Code, response.Body.String())
	}
}

func TestMemoryHandlersVerifyMutationSourcesAndStableSnapshot(t *testing.T) {
	tests := []struct {
		name, operation string
		status          memorykit.Status
		body            string
	}{
		{name: "activate sources", operation: "activate", status: memorykit.StatusCandidate, body: `{"expected_version":1,"reason":"approve"}`},
		{name: "dismiss sources", operation: "dismiss", status: memorykit.StatusCandidate, body: `{"expected_version":1,"reason":"dismiss"}`},
		{name: "forget sources", operation: "forget", status: memorykit.StatusActive, body: `{"expected_version":1,"reason":"forget"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			recorder := config.Store.(*recordingMemoryStore)
			scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
			id := seedMemory(t, recorder.Store, scope, test.status, "mutation-"+test.operation, "content")
			config.Store = &postMutationTamperMemoryStore{Store: recorder, operation: test.operation, tamperSources: true}
			response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodPost, "/projects/project-a/memories/"+id+"/"+test.operation, test.body)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s, want 500", response.Code, response.Body.String())
			}
		})
	}

	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	recorder := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, recorder.Store, scope, memorykit.StatusCandidate, "mutation-snapshot", "content")
	config.Store = &postMutationTamperMemoryStore{Store: recorder, operation: "activate", tamperSnapshot: true}
	response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodPost, "/projects/project-a/memories/"+id+"/activate", `{"expected_version":1,"reason":"approve"}`)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("snapshot status=%d body=%s, want 500", response.Code, response.Body.String())
	}
}

func TestMemoryHandlersReturnStableAdvancedSnapshotAfterMutation(t *testing.T) {
	tests := []struct {
		operation string
		body      string
	}{
		{operation: "correct", body: `{"expected_version":1,"content":"requested correction","valid_from":"2026-07-21T08:00:00Z","reason":"correct","importance":1,"confidence":1,"sources":[{"kind":"git","ref":"commit:requested","evidence_hash":"hash:requested"}]}`},
		{operation: "forget", body: `{"expected_version":1,"reason":"forget"}`},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			recorder := config.Store.(*recordingMemoryStore)
			scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
			id := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "post-race-"+test.operation, "original")
			config.Store = &postMutationAdvanceMemoryStore{Store: recorder, operation: test.operation}
			response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodPost,
				"/projects/project-a/memories/"+id+"/"+test.operation, test.body)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"version":3`) ||
				!strings.Contains(response.Body.String(), "concurrent correction") || !strings.Contains(response.Body.String(), "commit:advanced") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "commit:requested") {
				t.Fatalf("response used stale requested sources: %s", response.Body.String())
			}
		})
	}
}

func TestMemoryHandlersRejectUnreachableAdvancedStatusAfterMutation(t *testing.T) {
	tests := []struct {
		operation      string
		initialStatus  memorykit.Status
		advancedStatus memorykit.Status
		body           string
	}{
		{operation: "create", advancedStatus: memorykit.StatusCandidate, body: validCreateMemoryBody("unreachable-create", "")},
		{operation: "activate", initialStatus: memorykit.StatusCandidate, advancedStatus: memorykit.StatusCandidate, body: `{"expected_version":1,"reason":"activate"}`},
		{operation: "forget", initialStatus: memorykit.StatusActive, advancedStatus: memorykit.StatusActive, body: `{"expected_version":1,"reason":"forget"}`},
		{operation: "dismiss", initialStatus: memorykit.StatusCandidate, advancedStatus: memorykit.StatusCandidate, body: `{"expected_version":1,"reason":"dismiss"}`},
		{operation: "correct", initialStatus: memorykit.StatusInactive, advancedStatus: memorykit.StatusActive, body: `{"expected_version":1,"content":"corrected","valid_from":"2026-07-21T08:00:00Z","reason":"correct","importance":1,"confidence":1}`},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			recorder := config.Store.(*recordingMemoryStore)
			target := "/projects/project-a/memories"
			if test.operation != "create" {
				scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
				id := seedMemory(t, recorder.Store, scope, test.initialStatus, "unreachable-"+test.operation, "content")
				target += "/" + id + "/" + test.operation
			}
			config.Store = &postMutationAdvanceMemoryStore{
				Store: recorder, operation: test.operation, advancedStatus: test.advancedStatus,
			}
			response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodPost, target, test.body)
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"error":"memory_integrity_error"`) {
				t.Fatalf("status=%d body=%s, want integrity 500", response.Code, response.Body.String())
			}
		})
	}
}

func TestMemoryHandlersRejectUnreachableAdvancedStatusDuringMutationPreRead(t *testing.T) {
	config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	recorder := config.Store.(*recordingMemoryStore)
	scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
	id := seedMemory(t, recorder.Store, scope, memorykit.StatusActive, "unreachable-pre-read", "content")
	active, err := recorder.Store.Get(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	candidate := active
	candidate.Status, candidate.Version, candidate.UpdatedAt = memorykit.StatusCandidate, 2, active.UpdatedAt.Add(time.Minute)
	config.Store = &scriptedSnapshotMemoryStore{Store: recorder, getResults: []memorykit.Memory{active, candidate, candidate, candidate}}
	response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodPost,
		"/projects/project-a/memories/"+id+"/correct",
		`{"expected_version":1,"content":"corrected","valid_from":"2026-07-21T08:00:00Z","reason":"correct","importance":1,"confidence":1}`)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"error":"memory_integrity_error"`) {
		t.Fatalf("status=%d body=%s, want integrity 500", response.Code, response.Body.String())
	}
}

func TestMemoryHandlersRejectNonInactiveAdvancedErasePostcondition(t *testing.T) {
	for _, advancedStatus := range []memorykit.Status{memorykit.StatusActive, memorykit.StatusCandidate} {
		t.Run(string(advancedStatus), func(t *testing.T) {
			config := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
			recorder := config.Store.(*recordingMemoryStore)
			scope := memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"}
			id := seedMemory(t, recorder.Store, scope, memorykit.StatusCandidate, "erase-post-race-"+string(advancedStatus), "private")
			v1, err := recorder.Store.Get(context.Background(), scope, id)
			if err != nil {
				t.Fatal(err)
			}
			v3 := v1
			v3.Content, v3.Status, v3.Version, v3.UpdatedAt = "restored concurrently", advancedStatus, 3, v1.UpdatedAt.Add(2*time.Minute)
			config.Store = &scriptedSnapshotMemoryStore{Store: recorder, getResults: []memorykit.Memory{v1, v1, v3, v3}}
			response := memoryRequest(t, (&Server{memory: config}).Handler(), http.MethodPost,
				"/projects/project-a/memories/"+id+"/erase", `{"expected_version":1,"reason":"erase"}`)
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"error":"memory_integrity_error"`) {
				t.Fatalf("status=%d body=%s, want integrity 500", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "restored concurrently") {
				t.Fatalf("integrity response leaked content: %s", response.Body.String())
			}
		})
	}
}

func TestMemoryHandlersCreateIdempotencyUsesScopeBoundDeterministicID(t *testing.T) {
	limits := memoryLimitsForTest()
	base, err := memorystore.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	shared := &recordingMemoryStore{Store: base}
	newConfig := func(identity memoryIdentity, now time.Time) *memoryRuntime {
		return &memoryRuntime{memoryRuntimeConfig: memoryRuntimeConfig{
			Store:            shared,
			Authorizer:       &recordingMemoryAuthorizer{identity: identity},
			ContentValidator: &acceptingMemoryContentValidator{}, Limits: limits,
			NewID: func() string { return uuid.NewString() }, Now: func() time.Time { return now }, MaxHTTPBodyBytes: 4096,
		}}
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

type noOpEraseMemoryStore struct{ memorykit.Store }

func (*noOpEraseMemoryStore) Erase(context.Context, memorykit.VersionedCommand) error { return nil }

type mutatingRevisionMemoryStore struct {
	memorykit.Store
	mutate func([]memorykit.Revision)
}

type omitLatestRevisionMemoryStore struct{ memorykit.Store }

func (s *omitLatestRevisionMemoryStore) Revisions(ctx context.Context, query memorykit.RevisionQuery) ([]memorykit.Revision, error) {
	revisions, err := s.Store.Revisions(ctx, query)
	if err == nil && len(revisions) > 0 {
		return revisions[1:], nil
	}
	return revisions, err
}

func (s *mutatingRevisionMemoryStore) Revisions(ctx context.Context, query memorykit.RevisionQuery) ([]memorykit.Revision, error) {
	revisions, err := s.Store.Revisions(ctx, query)
	if err == nil && len(revisions) > 1 {
		s.mutate(revisions)
	}
	return revisions, err
}

type wrongCorrectSourcesMemoryStore struct{ memorykit.Store }

func (s *wrongCorrectSourcesMemoryStore) Sources(context.Context, memorykit.Scope, string) ([]memorykit.Source, error) {
	return []memorykit.Source{{Kind: "git", Ref: "commit:wrong", EvidenceHash: "sha256:wrong"}}, nil
}

type canonicalSourceOrderMemoryStore struct {
	memorykit.Store
	sourcesCalls int
}

func (s *canonicalSourceOrderMemoryStore) Sources(ctx context.Context, scope memorykit.Scope, id string) ([]memorykit.Source, error) {
	s.sourcesCalls++
	sources, err := s.Store.Sources(ctx, scope, id)
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Kind != sources[j].Kind {
			return sources[i].Kind < sources[j].Kind
		}
		if sources[i].Ref != sources[j].Ref {
			return sources[i].Ref < sources[j].Ref
		}
		return sources[i].EvidenceHash < sources[j].EvidenceHash
	})
	return sources, err
}

type scriptedSnapshotMemoryStore struct {
	memorykit.Store
	getResults  []memorykit.Memory
	getCalls    int
	listResults [][]memorykit.Memory
	listCalls   int
}

func (s *scriptedSnapshotMemoryStore) Get(ctx context.Context, scope memorykit.Scope, id string) (memorykit.Memory, error) {
	if len(s.getResults) == 0 {
		return s.Store.Get(ctx, scope, id)
	}
	index := min(s.getCalls, len(s.getResults)-1)
	s.getCalls++
	return s.getResults[index], nil
}

func (s *scriptedSnapshotMemoryStore) List(ctx context.Context, query memorykit.ListQuery) ([]memorykit.Memory, error) {
	if len(s.listResults) == 0 {
		return s.Store.List(ctx, query)
	}
	index := min(s.listCalls, len(s.listResults)-1)
	s.listCalls++
	return append([]memorykit.Memory(nil), s.listResults[index]...), nil
}

type postMutationTamperMemoryStore struct {
	memorykit.Store
	operation      string
	mutated        bool
	tamperSources  bool
	tamperSnapshot bool
}

type postMutationAdvanceMemoryStore struct {
	memorykit.Store
	operation       string
	advancedStatus  memorykit.Status
	advanced        memorykit.Memory
	advancedSources []memorykit.Source
}

func (s *postMutationAdvanceMemoryStore) advance(memory memorykit.Memory) {
	memory.Content = "concurrent correction"
	memory.Version++
	memory.UpdatedAt = memory.UpdatedAt.Add(time.Minute)
	if s.advancedStatus != "" {
		memory.Status = s.advancedStatus
	}
	s.advanced = memory
	s.advancedSources = []memorykit.Source{{Kind: "git", Ref: "commit:advanced", EvidenceHash: "hash:advanced"}}
}

func (s *postMutationAdvanceMemoryStore) Create(ctx context.Context, request memorykit.CreateRequest) (memorykit.Memory, error) {
	memory, err := s.Store.Create(ctx, request)
	if err == nil && s.operation == "create" {
		s.advance(memory)
	}
	return memory, err
}

func (s *postMutationAdvanceMemoryStore) Activate(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	memory, err := s.Store.Activate(ctx, command)
	if err == nil && s.operation == "activate" {
		s.advance(memory)
	}
	return memory, err
}

func (s *postMutationAdvanceMemoryStore) Correct(ctx context.Context, request memorykit.CorrectRequest) (memorykit.Memory, error) {
	memory, err := s.Store.Correct(ctx, request)
	if err == nil && s.operation == "correct" {
		s.advance(memory)
	}
	return memory, err
}

func (s *postMutationAdvanceMemoryStore) Forget(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	memory, err := s.Store.Forget(ctx, command)
	if err == nil && s.operation == "forget" {
		s.advance(memory)
	}
	return memory, err
}

func (s *postMutationAdvanceMemoryStore) Dismiss(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	memory, err := s.Store.Dismiss(ctx, command)
	if err == nil && s.operation == "dismiss" {
		s.advance(memory)
	}
	return memory, err
}

func (s *postMutationAdvanceMemoryStore) Get(ctx context.Context, scope memorykit.Scope, id string) (memorykit.Memory, error) {
	if s.advanced.ID != "" {
		return s.advanced, nil
	}
	return s.Store.Get(ctx, scope, id)
}

func (s *postMutationAdvanceMemoryStore) Sources(ctx context.Context, scope memorykit.Scope, id string) ([]memorykit.Source, error) {
	if s.advanced.ID != "" {
		return append([]memorykit.Source(nil), s.advancedSources...), nil
	}
	return s.Store.Sources(ctx, scope, id)
}

func (s *postMutationTamperMemoryStore) Activate(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	memory, err := s.Store.Activate(ctx, command)
	if err == nil && s.operation == "activate" {
		s.mutated = true
	}
	return memory, err
}

func (s *postMutationTamperMemoryStore) Dismiss(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	memory, err := s.Store.Dismiss(ctx, command)
	if err == nil && s.operation == "dismiss" {
		s.mutated = true
	}
	return memory, err
}

func (s *postMutationTamperMemoryStore) Forget(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	memory, err := s.Store.Forget(ctx, command)
	if err == nil && s.operation == "forget" {
		s.mutated = true
	}
	return memory, err
}

func (s *postMutationTamperMemoryStore) Sources(ctx context.Context, scope memorykit.Scope, id string) ([]memorykit.Source, error) {
	if s.mutated && s.tamperSources {
		return []memorykit.Source{{Kind: "git", Ref: "commit:changed", EvidenceHash: "hash:changed"}}, nil
	}
	return s.Store.Sources(ctx, scope, id)
}

func (s *postMutationTamperMemoryStore) Get(ctx context.Context, scope memorykit.Scope, id string) (memorykit.Memory, error) {
	memory, err := s.Store.Get(ctx, scope, id)
	if err == nil && s.mutated && s.tamperSnapshot {
		memory.Content = "concurrent content"
		memory.UpdatedAt = memory.UpdatedAt.Add(time.Minute)
	}
	return memory, err
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

func responseSourceKinds(t *testing.T, response *httptest.ResponseRecorder) []string {
	t.Helper()
	var decoded struct {
		Sources []struct {
			Kind string `json:"kind"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := make([]string, len(decoded.Sources))
	for index := range decoded.Sources {
		result[index] = decoded.Sources[index].Kind
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func storeLastListStatus(t *testing.T, store *recordingMemoryStore) memorykit.Status {
	t.Helper()
	if len(store.listQueries) == 0 {
		t.Fatal("List was not called")
	}
	return store.listQueries[len(store.listQueries)-1].Status
}
