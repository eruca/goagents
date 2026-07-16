// Package goagentapproval connects goagent approval interruptions to runkit's
// encrypted, single-lease checkpoint store.
package goagentapproval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/eruca/goagents/runkit"
)

var (
	ErrInvalidAdapter           = errors.New("invalid goagent approval adapter")
	ErrInvalidPendingCheckpoint = errors.New("invalid pending approval checkpoint")
	ErrInvalidNextCheckpoint    = errors.New("invalid next approval checkpoint")
	ErrCheckpointEncrypt        = errors.New("approval checkpoint encryption failed")
	ErrCheckpointDecrypt        = errors.New("approval checkpoint decryption failed")
	ErrCheckpointDecode         = errors.New("approval checkpoint decoding failed")
	ErrNextCheckpointPersist    = errors.New("next approval checkpoint persistence failed")
	ErrLeaseCompletion          = errors.New("approval checkpoint lease completion failed")
	ErrResumeResultMissing      = errors.New("agent resume returned no result")
)

// Cipher is implemented by the host's encryption boundary. It owns key
// selection, encryption, and decryption; aad authenticates checkpoint identity
// while runkit receives only opaque bytes.
type Cipher interface {
	Encrypt(ctx context.Context, plaintext, aad []byte) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext, aad []byte) ([]byte, error)
}

// Resumer is satisfied by agentcore.Agent. Keeping this interface narrow lets
// hosts test approval handling without a live model or tool runtime.
type Resumer interface {
	ResumeDetailed(ctx context.Context, checkpoint agentcore.RunCheckpoint, resolutions []agentcore.ToolApprovalResolution) (*agentcore.RunResult, error)
}

// Adapter persists interrupted goagent runs and resumes a single leased
// checkpoint after an authenticated host has recorded approval.
type Adapter struct {
	store   runkit.CheckpointStore
	cipher  Cipher
	resumer Resumer
}

func New(store runkit.CheckpointStore, cipher Cipher, resumer Resumer) (*Adapter, error) {
	if store == nil || cipher == nil || resumer == nil {
		return nil, ErrInvalidAdapter
	}
	return &Adapter{store: store, cipher: cipher, resumer: resumer}, nil
}

// PendingCheckpoint supplies host-owned identity and retention metadata for
// a newly interrupted run. The adapter encrypts Checkpoint before storage.
type PendingCheckpoint struct {
	ID             string
	TenantID       string
	DefinitionHash string
	ExpiresAt      time.Time
	Checkpoint     agentcore.RunCheckpoint
}

// NextCheckpoint is reserved before a resume. It is consumed only if that
// resume interrupts again, allowing the adapter to persist the new checkpoint
// before it consumes the old lease.
type NextCheckpoint struct {
	ID        string
	ExpiresAt time.Time
}

// ResumeRequest contains a previously authorized lease request, exact tool
// decisions, and a host-generated destination for a possible next pause.
type ResumeRequest struct {
	Approval    runkit.ApprovalLeaseRequest
	Resolutions []agentcore.ToolApprovalResolution
	Next        NextCheckpoint
}

// SavePending encrypts and stores an approval interruption. Call it after an
// agent returns agentcore.ErrApprovalPending and before exposing it to an
// operator approval queue.
func (a *Adapter) SavePending(ctx context.Context, pending PendingCheckpoint) error {
	if err := validatePending(pending); err != nil {
		return err
	}
	payload, err := json.Marshal(pending.Checkpoint)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPendingCheckpoint, err)
	}
	ciphertext, err := a.cipher.Encrypt(ctx, payload, checkpointAAD(pending.ID, pending.TenantID, pending.DefinitionHash))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCheckpointEncrypt, err)
	}
	if len(ciphertext) == 0 {
		return ErrCheckpointEncrypt
	}
	return a.store.CreateCheckpoint(ctx, runkit.ApprovalCheckpoint{
		ID:             pending.ID,
		RunID:          pending.Checkpoint.RunID,
		TenantID:       pending.TenantID,
		DefinitionHash: pending.DefinitionHash,
		Ciphertext:     ciphertext,
		ExpiresAt:      pending.ExpiresAt,
	})
}

// Reject records a terminal rejection without decrypting or executing the
// paused agent state.
func (a *Adapter) Reject(ctx context.Context, request runkit.ApprovalLeaseRequest) error {
	return a.store.RejectCheckpoint(ctx, request)
}

// ApproveAndResume atomically leases a pending checkpoint, resumes it once,
// and closes the lease. A resumed pause is saved before the old lease is
// consumed. Any failure after lease acquisition marks the old lease failed,
// never making it available for automatic replay.
func (a *Adapter) ApproveAndResume(ctx context.Context, request ResumeRequest) (*agentcore.RunResult, error) {
	if err := validateNextCheckpoint(request.Next, request.Approval); err != nil {
		return nil, err
	}
	leased, err := a.store.ApproveAndLease(ctx, request.Approval)
	if err != nil {
		return nil, err
	}
	checkpoint, err := a.decryptCheckpoint(ctx, leased)
	if err != nil {
		return a.failLease(ctx, leased, request.Approval, "checkpoint_decode_failed", nil, err)
	}
	result, resumeErr := a.resumer.ResumeDetailed(ctx, checkpoint, request.Resolutions)
	if result == nil && resumeErr == nil {
		return a.failLease(ctx, leased, request.Approval, "resume_result_missing", nil, ErrResumeResultMissing)
	}
	if errors.Is(resumeErr, agentcore.ErrApprovalPending) {
		if result == nil || result.Interruption == nil {
			return a.failLease(ctx, leased, request.Approval, "resume_interruption_missing", result, ErrNextCheckpointPersist)
		}
		next := PendingCheckpoint{
			ID:             request.Next.ID,
			TenantID:       leased.TenantID,
			DefinitionHash: leased.DefinitionHash,
			ExpiresAt:      request.Next.ExpiresAt,
			Checkpoint:     result.Interruption.Checkpoint,
		}
		if err := a.SavePending(ctx, next); err != nil {
			return a.failLease(ctx, leased, request.Approval, "next_checkpoint_persist_failed", result, fmt.Errorf("%w: %v", ErrNextCheckpointPersist, err))
		}
		if err := a.completeLease(ctx, leased, request.Approval); err != nil {
			return result, err
		}
		return result, resumeErr
	}
	if resumeErr != nil {
		return a.failLease(ctx, leased, request.Approval, "agent_resume_failed", result, resumeErr)
	}
	if err := a.completeLease(ctx, leased, request.Approval); err != nil {
		return result, err
	}
	return result, nil
}

func (a *Adapter) decryptCheckpoint(ctx context.Context, checkpoint runkit.ApprovalCheckpoint) (agentcore.RunCheckpoint, error) {
	payload, err := a.cipher.Decrypt(ctx, checkpoint.Ciphertext, checkpointAAD(checkpoint.ID, checkpoint.TenantID, checkpoint.DefinitionHash))
	if err != nil {
		return agentcore.RunCheckpoint{}, fmt.Errorf("%w: %v", ErrCheckpointDecrypt, err)
	}
	var runCheckpoint agentcore.RunCheckpoint
	if err := json.Unmarshal(payload, &runCheckpoint); err != nil {
		return agentcore.RunCheckpoint{}, fmt.Errorf("%w: %v", ErrCheckpointDecode, err)
	}
	return runCheckpoint, nil
}

func checkpointAAD(checkpointID, tenantID, definitionHash string) []byte {
	encoded, _ := json.Marshal(struct {
		CheckpointID   string `json:"checkpoint_id"`
		TenantID       string `json:"tenant_id"`
		DefinitionHash string `json:"definition_hash"`
	}{
		CheckpointID:   checkpointID,
		TenantID:       tenantID,
		DefinitionHash: definitionHash,
	})
	return encoded
}

func (a *Adapter) completeLease(ctx context.Context, checkpoint runkit.ApprovalCheckpoint, request runkit.ApprovalLeaseRequest) error {
	if err := a.store.CompleteLease(ctx, runkit.CheckpointLeaseCompletion{
		CheckpointID: checkpoint.ID,
		TenantID:     checkpoint.TenantID,
		LeaseOwner:   request.LeaseOwner,
		Now:          request.Now,
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrLeaseCompletion, err)
	}
	return nil
}

func (a *Adapter) failLease(ctx context.Context, checkpoint runkit.ApprovalCheckpoint, request runkit.ApprovalLeaseRequest, failureCode string, result *agentcore.RunResult, cause error) (*agentcore.RunResult, error) {
	if err := a.store.FailLease(ctx, runkit.CheckpointLeaseCompletion{
		CheckpointID: checkpoint.ID,
		TenantID:     checkpoint.TenantID,
		LeaseOwner:   request.LeaseOwner,
		FailureCode:  failureCode,
		Now:          request.Now,
	}); err != nil {
		return result, fmt.Errorf("%w: %v", ErrLeaseCompletion, err)
	}
	return result, cause
}

func validatePending(pending PendingCheckpoint) error {
	if strings.TrimSpace(pending.ID) == "" || strings.TrimSpace(pending.TenantID) == "" || strings.TrimSpace(pending.DefinitionHash) == "" || pending.ExpiresAt.IsZero() || strings.TrimSpace(pending.Checkpoint.RunID) == "" || len(pending.Checkpoint.PendingCalls) == 0 {
		return ErrInvalidPendingCheckpoint
	}
	return nil
}

func validateNextCheckpoint(next NextCheckpoint, approval runkit.ApprovalLeaseRequest) error {
	if strings.TrimSpace(next.ID) == "" || next.ExpiresAt.IsZero() || next.ID == approval.CheckpointID {
		return ErrInvalidNextCheckpoint
	}
	now := approval.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !next.ExpiresAt.After(now) {
		return ErrInvalidNextCheckpoint
	}
	return nil
}
