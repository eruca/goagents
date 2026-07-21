package memorykit

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
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

func ValidateCreateRequest(request CreateRequest, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(request.ID) == "" || !request.Kind.IsValid() || !request.Status.IsValid() ||
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
	if strings.TrimSpace(command.ID) == "" || command.ExpectedVersion <= 0 ||
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
	if strings.TrimSpace(query.MemoryID) == "" || query.BeforeVersion < 0 ||
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
	for _, source := range sources {
		if !validMetadata(source.Kind, limits) || !validMetadata(source.Ref, limits) ||
			!validMetadata(source.EvidenceHash, limits) {
			return fmt.Errorf("%w: invalid source", ErrInvalidMemory)
		}
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
