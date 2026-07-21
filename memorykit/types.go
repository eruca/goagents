package memorykit

import "time"

type SubjectType string

const (
	SubjectProject SubjectType = "project"
	SubjectUser    SubjectType = "user"
)

type Kind string

const (
	KindFact       Kind = "fact"
	KindDecision   Kind = "decision"
	KindConstraint Kind = "constraint"
	KindLesson     Kind = "lesson"
)

type Status string

const (
	StatusCandidate Status = "candidate"
	StatusActive    Status = "active"
	StatusInactive  Status = "inactive"
)

type Scope struct {
	TenantID    string
	SubjectType SubjectType
	SubjectID   string
}

type Memory struct {
	ID, Key, Content, SourceAgentID, CreatedBy, IdempotencyKey string
	Scope                                                      Scope
	Kind                                                       Kind
	Status                                                     Status
	ValidFrom, ValidUntil                                      time.Time
	Importance                                                 int
	Confidence                                                 float64
	Version                                                    int64
	CreatedAt, UpdatedAt                                       time.Time
}

type Source struct{ Kind, Ref, EvidenceHash string }

type RevisionAction string

const (
	RevisionCreate    RevisionAction = "create"
	RevisionActivate  RevisionAction = "activate"
	RevisionCorrect   RevisionAction = "correct"
	RevisionSupersede RevisionAction = "supersede"
	RevisionForget    RevisionAction = "forget"
	RevisionErase     RevisionAction = "erase"
	RevisionDismiss   RevisionAction = "dismiss"
)

type Revision struct {
	MemoryID      string
	Version       int64
	Action        RevisionAction
	Actor, Reason string
	Snapshot      Memory
	ContentErased bool
	CreatedAt     time.Time
}

type Limits struct {
	Version             string
	MaxKeyRunes         int
	MaxContentRunes     int
	MaxMetadataRunes    int
	MaxSourcesPerMemory int
	MaxListItems        int
}

type CreateRequest struct {
	ID                                           string
	Scope                                        Scope
	Kind                                         Kind
	Key                                          string
	Status                                       Status
	Content                                      string
	ValidFrom, ValidUntil                        time.Time
	Importance                                   int
	Confidence                                   float64
	SourceAgentID, Actor, Reason, IdempotencyKey string
	Sources                                      []Source
	Now                                          time.Time
}

type VersionedCommand struct {
	Scope           Scope
	ID              string
	ExpectedVersion int64
	Actor, Reason   string
	Now             time.Time
}

type CorrectRequest struct {
	Command               VersionedCommand
	Content               string
	ValidFrom, ValidUntil time.Time
	Importance            int
	Confidence            float64
	Sources               []Source
}

type ListQuery struct {
	Scope  Scope
	Kind   Kind
	Status Status
	Key    string
	Limit  int
}

type RevisionQuery struct {
	Scope         Scope
	MemoryID      string
	BeforeVersion int64
	Limit         int
}
