package memorykit

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Validate checks that a scope identifies exactly one supported subject.
func (s Scope) Validate() error {
	if strings.TrimSpace(s.TenantID) == "" || strings.TrimSpace(s.SubjectID) == "" {
		return fmt.Errorf("%w: tenant and subject identity are required", ErrInvalidScope)
	}
	if s.SubjectType != SubjectProject && s.SubjectType != SubjectUser {
		return fmt.Errorf("%w: unsupported subject type %q", ErrInvalidScope, s.SubjectType)
	}
	return nil
}

// Validate checks that callers explicitly configure every project-memory-v1 limit.
func (l Limits) Validate() error {
	if strings.TrimSpace(l.Version) == "" || l.MaxKeyRunes <= 0 || l.MaxContentRunes <= 0 ||
		l.MaxMetadataRunes <= 0 || l.MaxSourcesPerMemory <= 0 || l.MaxListItems <= 0 {
		return fmt.Errorf("%w: all limits and version are required", ErrInvalidMemory)
	}
	return nil
}

func (k Kind) IsValid() bool {
	return k == KindFact || k == KindDecision || k == KindConstraint || k == KindLesson
}

func (s Status) IsValid() bool {
	return s == StatusCandidate || s == StatusActive || s == StatusInactive
}

// ValidateMemoryID accepts only the canonical lowercase UUID text form. Storage
// implementations must not normalize alternate representations implicitly.
func ValidateMemoryID(id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return fmt.Errorf("%w: memory ID must be a canonical lowercase UUID", ErrInvalidMemory)
	}
	return nil
}

func ValidateCreateRequest(request CreateRequest, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if err := ValidateMemoryID(request.ID); err != nil {
		return err
	}
	if !request.Kind.IsValid() || !request.Status.IsValid() ||
		!validKey(request.Key, limits.MaxKeyRunes) || !validContent(request.Content, limits.MaxContentRunes) ||
		!validTimeRange(request.ValidFrom, request.ValidUntil) || !validImportance(request.Importance) ||
		!validConfidence(request.Confidence) ||
		!validMetadata(request.SourceAgentID, limits) || !validMetadata(request.Actor, limits) ||
		!validMetadata(request.Reason, limits) || !validMetadata(request.IdempotencyKey, limits) {
		return fmt.Errorf("%w: invalid create request", ErrInvalidMemory)
	}
	return ValidateSources(request.Sources, limits)
}

func ValidateCorrectRequest(request CorrectRequest, limits Limits) error {
	if err := ValidateCommand(request.Command, limits); err != nil {
		return err
	}
	if !validContent(request.Content, limits.MaxContentRunes) ||
		!validTimeRange(request.ValidFrom, request.ValidUntil) || !validImportance(request.Importance) ||
		!validConfidence(request.Confidence) {
		return fmt.Errorf("%w: invalid correction request", ErrInvalidMemory)
	}
	return ValidateSources(request.Sources, limits)
}

func ValidateCommand(command VersionedCommand, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if err := command.Scope.Validate(); err != nil {
		return err
	}
	if err := ValidateMemoryID(command.ID); err != nil {
		return err
	}
	if command.ExpectedVersion <= 0 ||
		!validMetadata(command.Actor, limits) || !validMetadata(command.Reason, limits) {
		return fmt.Errorf("%w: invalid versioned command", ErrInvalidMemory)
	}
	return nil
}

func ValidateListQuery(query ListQuery, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if err := query.Scope.Validate(); err != nil {
		return err
	}
	if (query.Kind != "" && !query.Kind.IsValid()) || (query.Status != "" && !query.Status.IsValid()) ||
		(query.Key != "" && !validKey(query.Key, limits.MaxKeyRunes)) || query.Limit <= 0 || query.Limit > limits.MaxListItems {
		return fmt.Errorf("%w: invalid list query", ErrInvalidMemory)
	}
	return nil
}

func ValidateRevisionQuery(query RevisionQuery, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if err := query.Scope.Validate(); err != nil {
		return err
	}
	if err := ValidateMemoryID(query.MemoryID); err != nil {
		return err
	}
	if query.BeforeVersion < 0 ||
		query.Limit <= 0 || query.Limit > limits.MaxListItems {
		return fmt.Errorf("%w: invalid revision query", ErrInvalidMemory)
	}
	return nil
}

func ValidateSources(sources []Source, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if len(sources) > limits.MaxSourcesPerMemory {
		return fmt.Errorf("%w: too many sources", ErrInvalidMemory)
	}
	type sourceIdentity struct{ kind, ref string }
	seen := make(map[sourceIdentity]struct{}, len(sources))
	for _, source := range sources {
		if !validMetadata(source.Kind, limits) || !validMetadata(source.Ref, limits) ||
			!validMetadata(source.EvidenceHash, limits) {
			return fmt.Errorf("%w: invalid source", ErrInvalidMemory)
		}
		identity := sourceIdentity{kind: source.Kind, ref: source.Ref}
		if _, exists := seen[identity]; exists {
			return fmt.Errorf("%w: duplicate source", ErrInvalidMemory)
		}
		seen[identity] = struct{}{}
	}
	return nil
}

func validKey(key string, maxRunes int) bool {
	if key == "" || key != strings.TrimSpace(key) || utf8.RuneCountInString(key) > maxRunes {
		return false
	}
	for _, r := range key {
		if r <= 0x1f || r == 0x7f {
			return false
		}
	}
	return true
}

func validContent(content string, maxRunes int) bool {
	return strings.TrimSpace(content) != "" && !strings.ContainsRune(content, 0) && utf8.RuneCountInString(content) <= maxRunes
}

func validTimeRange(from, until time.Time) bool {
	return until.IsZero() || until.After(from)
}

func validImportance(importance int) bool { return importance >= 0 && importance <= 100 }

func validConfidence(confidence float64) bool { return confidence >= 0 && confidence <= 1 }

func validMetadata(value string, limits Limits) bool {
	return utf8.RuneCountInString(value) <= limits.MaxMetadataRunes
}
