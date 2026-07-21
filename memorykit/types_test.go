package memorykit

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestScopeValidateAllowsProjectAndRejectsMissingIdentity(t *testing.T) {
	valid := Scope{TenantID: "tenant-1", SubjectType: SubjectProject, SubjectID: "project-1"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate valid scope: %v", err)
	}
	for _, scope := range []Scope{
		{},
		{TenantID: "tenant-1", SubjectType: SubjectProject},
		{TenantID: "tenant-1", SubjectType: "workspace", SubjectID: "project-1"},
	} {
		if err := scope.Validate(); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("Validate(%+v) = %v, want ErrInvalidScope", scope, err)
		}
	}
}

func TestValidateCreateRequestRejectsUnsupportedKindAndOversizeContent(t *testing.T) {
	limits := Limits{
		Version:     "project-memory-v1",
		MaxKeyRunes: 32, MaxContentRunes: 64, MaxMetadataRunes: 128,
		MaxSourcesPerMemory: 4, MaxListItems: 50,
	}
	base := CreateRequest{
		ID:    "11111111-1111-1111-1111-111111111111",
		Scope: Scope{TenantID: "tenant-1", SubjectType: SubjectProject, SubjectID: "project-1"},
		Kind:  KindFact, Key: "build.test_command", Status: StatusActive,
		Content: "Run go test ./...", Actor: "user-1", Reason: "explicit request",
		ValidFrom: time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC),
	}
	if err := ValidateCreateRequest(base, limits); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	badKind := base
	badKind.Kind = "progress"
	if err := ValidateCreateRequest(badKind, limits); !errors.Is(err, ErrInvalidMemory) {
		t.Fatalf("bad kind error = %v, want ErrInvalidMemory", err)
	}
	tooLong := base
	tooLong.Content = string(make([]rune, limits.MaxContentRunes+1))
	if err := ValidateCreateRequest(tooLong, limits); !errors.Is(err, ErrInvalidMemory) {
		t.Fatalf("oversize error = %v, want ErrInvalidMemory", err)
	}
	badSource := base
	badSource.Sources = []Source{{Kind: "artifact", Ref: strings.Repeat("r", limits.MaxMetadataRunes+1)}}
	if err := ValidateCreateRequest(badSource, limits); !errors.Is(err, ErrInvalidMemory) {
		t.Fatalf("oversize source error = %v, want ErrInvalidMemory", err)
	}
}
