package pgstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/eruca/goagents/memorykit"
)

const memoryColumns = `
	id, tenant_id, subject_type, subject_id, kind, memory_key, status, content,
	valid_from, valid_until, importance, confidence, source_agent_id, created_by,
	idempotency_key, version, created_at, updated_at`

type rowScanner interface {
	Scan(...any) error
}

// scanMemory is the single decoder for every memory query, so reads cannot drift
// when the persistence shape changes.
func scanMemory(row rowScanner) (memorykit.Memory, error) {
	return scanMemoryWithTail(row)
}

// scanMemoryWithTail keeps the canonical memory decoder reusable for queries
// that append computed columns, such as vector similarity.
func scanMemoryWithTail(row rowScanner, tail ...any) (memorykit.Memory, error) {
	var memory memorykit.Memory
	var validUntil sql.NullTime
	destinations := []any{
		&memory.ID,
		&memory.Scope.TenantID,
		&memory.Scope.SubjectType,
		&memory.Scope.SubjectID,
		&memory.Kind,
		&memory.Key,
		&memory.Status,
		&memory.Content,
		&memory.ValidFrom,
		&validUntil,
		&memory.Importance,
		&memory.Confidence,
		&memory.SourceAgentID,
		&memory.CreatedBy,
		&memory.IdempotencyKey,
		&memory.Version,
		&memory.CreatedAt,
		&memory.UpdatedAt,
	}
	destinations = append(destinations, tail...)
	err := row.Scan(destinations...)
	if err != nil {
		return memorykit.Memory{}, err
	}
	if validUntil.Valid {
		memory.ValidUntil = validUntil.Time.UTC()
	}
	memory.ValidFrom = memory.ValidFrom.UTC()
	memory.CreatedAt = memory.CreatedAt.UTC()
	memory.UpdatedAt = memory.UpdatedAt.UTC()
	return memory, nil
}

// revisionSnapshot is deliberately private. Its explicit tags are both the
// storage contract and the privacy-erasure contract; public structs must never
// be marshaled directly into revision JSONB.
type revisionSnapshot struct {
	ID                string                `json:"id"`
	TenantID          string                `json:"tenant_id"`
	SubjectType       memorykit.SubjectType `json:"subject_type"`
	SubjectID         string                `json:"subject_id"`
	Kind              memorykit.Kind        `json:"kind"`
	Key               string                `json:"key"`
	Status            memorykit.Status      `json:"status"`
	Content           string                `json:"content,omitempty"`
	ValidFrom         time.Time             `json:"valid_from"`
	ValidUntil        time.Time             `json:"valid_until,omitempty"`
	Importance        int                   `json:"importance"`
	Confidence        float64               `json:"confidence"`
	SourceAgentID     string                `json:"source_agent_id"`
	CreatedBy         string                `json:"created_by"`
	IdempotencyKey    string                `json:"idempotency_key"`
	Version           int64                 `json:"version"`
	CreatedAt         time.Time             `json:"created_at"`
	UpdatedAt         time.Time             `json:"updated_at"`
	CreateFingerprint string                `json:"create_fingerprint,omitempty"`
}

func snapshotFromMemory(memory memorykit.Memory, createFingerprint string) revisionSnapshot {
	return revisionSnapshot{
		ID:                memory.ID,
		TenantID:          memory.Scope.TenantID,
		SubjectType:       memory.Scope.SubjectType,
		SubjectID:         memory.Scope.SubjectID,
		Kind:              memory.Kind,
		Key:               memory.Key,
		Status:            memory.Status,
		Content:           memory.Content,
		ValidFrom:         memory.ValidFrom,
		ValidUntil:        memory.ValidUntil,
		Importance:        memory.Importance,
		Confidence:        memory.Confidence,
		SourceAgentID:     memory.SourceAgentID,
		CreatedBy:         memory.CreatedBy,
		IdempotencyKey:    memory.IdempotencyKey,
		Version:           memory.Version,
		CreatedAt:         memory.CreatedAt,
		UpdatedAt:         memory.UpdatedAt,
		CreateFingerprint: createFingerprint,
	}
}

func (snapshot revisionSnapshot) memory() memorykit.Memory {
	return memorykit.Memory{
		ID: snapshot.ID,
		Scope: memorykit.Scope{
			TenantID: snapshot.TenantID, SubjectType: snapshot.SubjectType, SubjectID: snapshot.SubjectID,
		},
		Kind: snapshot.Kind, Key: snapshot.Key, Status: snapshot.Status, Content: snapshot.Content,
		ValidFrom: snapshot.ValidFrom, ValidUntil: snapshot.ValidUntil,
		Importance: snapshot.Importance, Confidence: snapshot.Confidence,
		SourceAgentID: snapshot.SourceAgentID, CreatedBy: snapshot.CreatedBy,
		IdempotencyKey: snapshot.IdempotencyKey, Version: snapshot.Version,
		CreatedAt: snapshot.CreatedAt, UpdatedAt: snapshot.UpdatedAt,
	}
}

func encodeRevisionSnapshot(memory memorykit.Memory, createFingerprint string) ([]byte, error) {
	encoded, err := json.Marshal(snapshotFromMemory(memory, createFingerprint))
	if err != nil {
		return nil, fmt.Errorf("%w: encode revision snapshot", memorykit.ErrInvalidMemory)
	}
	return encoded, nil
}

func scanRevision(row rowScanner) (memorykit.Revision, error) {
	var revision memorykit.Revision
	var encoded []byte
	if err := row.Scan(
		&revision.MemoryID,
		&revision.Version,
		&revision.Action,
		&revision.Actor,
		&revision.Reason,
		&encoded,
		&revision.ContentErased,
		&revision.CreatedAt,
	); err != nil {
		return memorykit.Revision{}, err
	}
	revision.CreatedAt = revision.CreatedAt.UTC()
	var snapshot revisionSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return memorykit.Revision{}, err
	}
	revision.Snapshot = snapshot.memory()
	return revision, nil
}
