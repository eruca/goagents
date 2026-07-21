package main

import (
	"context"
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

var errMemoryDeniedForTest = errors.New("memory authorization denied")

type recordingMemoryAuthorizer struct {
	identity memoryIdentity
	err      error
	calls    []memoryAuthorizationCall
}

type memoryAuthorizationCall struct {
	authorization string
	projectID     string
	capability    memoryCapability
}

func (a *recordingMemoryAuthorizer) AuthorizeMemory(_ context.Context, authorization, projectID string, capability memoryCapability) (memoryIdentity, error) {
	a.calls = append(a.calls, memoryAuthorizationCall{authorization: authorization, projectID: projectID, capability: capability})
	if a.err != nil {
		return memoryIdentity{}, a.err
	}
	return a.identity, nil
}

type recordingMemoryStore struct {
	*memorystore.Store
	calls       []string
	listQueries []memorykit.ListQuery
}

func (s *recordingMemoryStore) record(call string) { s.calls = append(s.calls, call) }

func (s *recordingMemoryStore) Create(ctx context.Context, request memorykit.CreateRequest) (memorykit.Memory, error) {
	s.record("create")
	return s.Store.Create(ctx, request)
}

func (s *recordingMemoryStore) Get(ctx context.Context, scope memorykit.Scope, id string) (memorykit.Memory, error) {
	s.record("get")
	return s.Store.Get(ctx, scope, id)
}

func (s *recordingMemoryStore) Sources(ctx context.Context, scope memorykit.Scope, id string) ([]memorykit.Source, error) {
	s.record("sources")
	return s.Store.Sources(ctx, scope, id)
}

func (s *recordingMemoryStore) List(ctx context.Context, query memorykit.ListQuery) ([]memorykit.Memory, error) {
	s.record("list")
	s.listQueries = append(s.listQueries, query)
	return s.Store.List(ctx, query)
}

func (s *recordingMemoryStore) Activate(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	s.record("activate")
	return s.Store.Activate(ctx, command)
}

func (s *recordingMemoryStore) Correct(ctx context.Context, request memorykit.CorrectRequest) (memorykit.Memory, error) {
	s.record("correct")
	return s.Store.Correct(ctx, request)
}

func (s *recordingMemoryStore) Dismiss(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	s.record("dismiss")
	return s.Store.Dismiss(ctx, command)
}

func (s *recordingMemoryStore) Forget(ctx context.Context, command memorykit.VersionedCommand) (memorykit.Memory, error) {
	s.record("forget")
	return s.Store.Forget(ctx, command)
}

func (s *recordingMemoryStore) Erase(ctx context.Context, command memorykit.VersionedCommand) error {
	s.record("erase")
	return s.Store.Erase(ctx, command)
}

func (s *recordingMemoryStore) Revisions(ctx context.Context, query memorykit.RevisionQuery) ([]memorykit.Revision, error) {
	s.record("revisions")
	return s.Store.Revisions(ctx, query)
}

type acceptingMemoryContentValidator struct {
	err   error
	calls []string
}

func (v *acceptingMemoryContentValidator) ValidateMemoryContent(_ context.Context, content string) error {
	v.calls = append(v.calls, content)
	return v.err
}

func TestMemoryAuthorizationFailsClosedBeforeStore(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		projectID     string
		authorizer    *recordingMemoryAuthorizer
		wantStatus    int
	}{
		{name: "blank bearer", projectID: "project-a", authorizer: validMemoryAuthorizer(), wantStatus: http.StatusUnauthorized},
		{name: "blank project", authorization: "Bearer token", authorizer: validMemoryAuthorizer(), wantStatus: http.StatusForbidden},
		{name: "wrong project", authorization: "Bearer token", projectID: "project-b", authorizer: &recordingMemoryAuthorizer{err: errMemoryDeniedForTest}, wantStatus: http.StatusForbidden},
		{name: "wrong capability", authorization: "Bearer token", projectID: "project-a", authorizer: &recordingMemoryAuthorizer{err: errMemoryDeniedForTest}, wantStatus: http.StatusForbidden},
		{name: "blank tenant", authorization: "Bearer token", projectID: "project-a", authorizer: &recordingMemoryAuthorizer{identity: memoryIdentity{Subject: "user-a"}}, wantStatus: http.StatusForbidden},
		{name: "blank subject", authorization: "Bearer token", projectID: "project-a", authorizer: &recordingMemoryAuthorizer{identity: memoryIdentity{TenantID: "tenant-a"}}, wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, store := newMemoryHandlerServer(t, test.authorizer)
			request := httptest.NewRequest(http.MethodGet, "/projects/project-a/memories", nil)
			request.SetPathValue("projectID", test.projectID)
			request.Header.Set("Authorization", test.authorization)
			response := httptest.NewRecorder()

			server.handleListMemories(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if len(store.calls) != 0 {
				t.Fatalf("store calls = %v, want none", store.calls)
			}
		})
	}
}

func TestMemoryAuthorizationRejectsIdentityFieldsInBody(t *testing.T) {
	server, store := newMemoryHandlerServer(t, validMemoryAuthorizer())
	for _, field := range []string{"tenant_id", "subject_id"} {
		t.Run(field, func(t *testing.T) {
			store.calls = nil
			body := `{"kind":"fact","key":"build.command","content":"run go test","reason":"explicit","importance":1,"confidence":1,"` + field + `":"attacker"}`
			response := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories", body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", response.Code, response.Body.String())
			}
			if len(store.calls) != 0 {
				t.Fatalf("store calls = %v, want none", store.calls)
			}
		})
	}
}

func TestMemoryAuthorizationBuildsOnlyProjectScope(t *testing.T) {
	authorizer := validMemoryAuthorizer()
	server, store := newMemoryHandlerServer(t, authorizer)
	response := memoryRequest(t, server.Handler(), http.MethodPost, "/projects/project-a/memories", validCreateMemoryBody("scope-key", ""))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	memories, err := store.Store.List(context.Background(), memorykit.ListQuery{
		Scope:  memorykit.Scope{TenantID: "tenant-a", SubjectType: memorykit.SubjectProject, SubjectID: "project-a"},
		Status: memorykit.StatusActive, Limit: memoryLimitsForTest().MaxListItems,
	})
	if err != nil || len(memories) != 1 || memories[0].CreatedBy != "user-a" {
		t.Fatalf("stored memories = %#v, err=%v", memories, err)
	}
}

func TestMemoryAuthorizationNewServerRejectsIncompleteSecurityConfig(t *testing.T) {
	valid := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	tests := []struct {
		name   string
		mutate func(*memoryRuntimeConfig)
	}{
		{name: "store", mutate: func(c *memoryRuntimeConfig) { c.Store = nil }},
		{name: "authorizer", mutate: func(c *memoryRuntimeConfig) { c.Authorizer = nil }},
		{name: "validator", mutate: func(c *memoryRuntimeConfig) { c.ContentValidator = nil }},
		{name: "limits", mutate: func(c *memoryRuntimeConfig) { c.Limits = memorykit.Limits{} }},
		{name: "new id", mutate: func(c *memoryRuntimeConfig) { c.NewID = nil }},
		{name: "clock", mutate: func(c *memoryRuntimeConfig) { c.Now = nil }},
		{name: "body limit", mutate: func(c *memoryRuntimeConfig) { c.MaxHTTPBodyBytes = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := *valid
			test.mutate(&copy)
			server, err := NewServer(Config{RuntimeHome: t.TempDir(), Memory: &copy})
			if err == nil {
				_ = server.Close(context.Background())
				t.Fatal("NewServer error = nil")
			}
		})
	}
}

func TestMemoryAuthorizationNewServerRegistersOnlyConfiguredRoutes(t *testing.T) {
	configured := validMemoryRuntimeConfig(t, validMemoryAuthorizer())
	server, err := NewServer(Config{RuntimeHome: t.TempDir(), Memory: configured})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close(context.Background()) }()
	configured.Authorizer = nil // NewServer owns a validated top-level copy.
	response := memoryRequest(t, server.Handler(), http.MethodGet, "/projects/project-a/memories", "")
	if response.Code != http.StatusOK {
		t.Fatalf("configured route status=%d body=%s", response.Code, response.Body.String())
	}

	disabled, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disabled.Close(context.Background()) }()
	disabledResponse := memoryRequest(t, disabled.Handler(), http.MethodGet, "/projects/project-a/memories", "")
	if disabledResponse.Code != http.StatusNotFound {
		t.Fatalf("disabled memory route status=%d, want 404", disabledResponse.Code)
	}
}

func newMemoryHandlerServer(t *testing.T, authorizer MemoryAuthorizer) (*Server, *recordingMemoryStore) {
	t.Helper()
	config := validMemoryRuntimeConfig(t, authorizer)
	return &Server{memory: config}, config.Store.(*recordingMemoryStore)
}

func validMemoryRuntimeConfig(t *testing.T, authorizer MemoryAuthorizer) *memoryRuntimeConfig {
	t.Helper()
	base, err := memorystore.New(memoryLimitsForTest())
	if err != nil {
		t.Fatal(err)
	}
	return &memoryRuntimeConfig{
		Store: &recordingMemoryStore{Store: base}, Authorizer: authorizer,
		ContentValidator: &acceptingMemoryContentValidator{}, Limits: memoryLimitsForTest(),
		NewID:            func() string { return uuid.NewString() },
		Now:              func() time.Time { return time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC) },
		MaxHTTPBodyBytes: 4096,
	}
}

func validMemoryAuthorizer() *recordingMemoryAuthorizer {
	return &recordingMemoryAuthorizer{identity: memoryIdentity{TenantID: "tenant-a", Subject: "user-a"}}
}

func memoryLimitsForTest() memorykit.Limits {
	return memorykit.Limits{Version: "test-v1", MaxKeyRunes: 128, MaxContentRunes: 1024, MaxMetadataRunes: 256, MaxSourcesPerMemory: 4, MaxListItems: 10}
}

func memoryRequest(t *testing.T, handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func validCreateMemoryBody(key, idempotencyKey string) string {
	return `{"kind":"fact","key":"` + key + `","content":"run go test","reason":"explicit","importance":1,"confidence":1,"idempotency_key":"` + idempotencyKey + `","sources":[{"kind":"git","ref":"commit:abc","evidence_hash":"sha256:abc"}]}`
}
