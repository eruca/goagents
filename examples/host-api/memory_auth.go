package main

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/eruca/goagents/memorykit"
)

type memoryCapability string

const (
	memoryRead          memoryCapability = "memory.read"
	memoryWriteExplicit memoryCapability = "memory.write_explicit"
	memoryReview        memoryCapability = "memory.review"
	memoryErase         memoryCapability = "memory.erase"
)

type memoryIdentity struct {
	TenantID string
	Subject  string
}

type MemoryAuthorizer interface {
	AuthorizeMemory(context.Context, string, string, memoryCapability) (memoryIdentity, error)
}

type memoryRuntimeConfig struct {
	Store              memorykit.Store
	Authorizer         MemoryAuthorizer
	ContentValidator   memorykit.ContentValidator
	Limits             memorykit.Limits
	AutoRecall         *memorykit.Recaller
	DeepRecall         *memorykit.Recaller
	EmbeddingWorker    *memorykit.EmbeddingWorker
	ExtractionWorker   *memorykit.ExtractionWorker
	NewID              func() string
	Now                func() time.Time
	MaxHTTPBodyBytes   int64
	EmbeddingInterval  time.Duration
	ExtractionInterval time.Duration
}

func validateMemoryManagementConfig(config *memoryRuntimeConfig) error {
	if config == nil {
		return nil
	}
	if nilMemoryDependency(config.Store) || nilMemoryDependency(config.Authorizer) ||
		nilMemoryDependency(config.ContentValidator) || config.NewID == nil || config.Now == nil ||
		config.MaxHTTPBodyBytes <= 0 {
		return fmt.Errorf("invalid memory management configuration")
	}
	if err := config.Limits.Validate(); err != nil {
		return fmt.Errorf("invalid memory management configuration: %w", err)
	}
	return nil
}

func nilMemoryDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// authorizeProjectMemory is the only Host path that constructs a memory Scope.
// Tenant and actor come from the verified authorizer result; project comes only
// from the trusted route selected by net/http.
func (s *Server) authorizeProjectMemory(w http.ResponseWriter, r *http.Request, capability memoryCapability) (memorykit.Scope, memoryIdentity, bool) {
	if s.memory == nil || nilMemoryDependency(s.memory.Authorizer) {
		writeMemoryError(w, http.StatusServiceUnavailable, "memory_unavailable", "project memory is unavailable")
		return memorykit.Scope{}, memoryIdentity{}, false
	}
	authorization := r.Header.Get("Authorization")
	if strings.TrimSpace(authorization) == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeMemoryError(w, http.StatusUnauthorized, "unauthorized", "memory authentication required")
		return memorykit.Scope{}, memoryIdentity{}, false
	}
	projectID := r.PathValue("projectID")
	if strings.TrimSpace(projectID) == "" {
		writeMemoryError(w, http.StatusForbidden, "forbidden", "project memory access denied")
		return memorykit.Scope{}, memoryIdentity{}, false
	}
	identity, err := s.memory.Authorizer.AuthorizeMemory(r.Context(), authorization, projectID, capability)
	if err != nil || strings.TrimSpace(identity.TenantID) == "" || strings.TrimSpace(identity.Subject) == "" {
		writeMemoryError(w, http.StatusForbidden, "forbidden", "project memory access denied")
		return memorykit.Scope{}, memoryIdentity{}, false
	}
	scope := memorykit.Scope{
		TenantID:    identity.TenantID,
		SubjectType: memorykit.SubjectProject,
		SubjectID:   projectID,
	}
	if scope.SubjectType != memorykit.SubjectProject || scope.Validate() != nil {
		writeMemoryError(w, http.StatusForbidden, "forbidden", "project memory access denied")
		return memorykit.Scope{}, memoryIdentity{}, false
	}
	return scope, identity, true
}
