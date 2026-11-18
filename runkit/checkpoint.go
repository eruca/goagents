package runkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrCheckpointNotFound     = errors.New("approval checkpoint not found")
	ErrCheckpointNotClaimable = errors.New("approval checkpoint not claimable")
	ErrCheckpointLeaseOwner   = errors.New("approval checkpoint lease owner mismatch")
)

type CheckpointStatus string

const (
	CheckpointPending  CheckpointStatus = "pending"
	CheckpointLeased   CheckpointStatus = "leased"
	CheckpointConsumed CheckpointStatus = "consumed"
	CheckpointRejected CheckpointStatus = "rejected"
	CheckpointFailed   CheckpointStatus = "failed"
	CheckpointExpired  CheckpointStatus = "expired"
)

// ApprovalCheckpoint stores opaque host-encrypted state. Runkit never decrypts
// Ciphertext, and stores drop it once the checkpoint reaches a terminal status
// (consumed, rejected, failed, or expired): it holds the paused run's tool
// arguments and is never decrypted again; the approval audit is kept.
type ApprovalCheckpoint struct {
	ID             string
	RunID          string
	TenantID       string
	DefinitionHash string
	Ciphertext     []byte
	Status         CheckpointStatus
	FailureCode    string
	LeaseOwner     string
	LeaseUntil     time.Time
	ExpiresAt      time.Time
	Approval       *ApprovalAudit
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ApprovalAudit records the host identity that allowed or rejected a checkpoint.
type ApprovalAudit struct {
	ApproverID string
	Approved   bool
	AuditRef   string
	ReasonCode string
	DecidedAt  time.Time
}

type ApprovalLeaseRequest struct {
	CheckpointID   string
	TenantID       string
	DefinitionHash string
	ApproverID     string
	AuditRef       string
	ReasonCode     string
	LeaseOwner     string
	LeaseDuration  time.Duration
	Now            time.Time
}

type CheckpointLeaseCompletion struct {
	CheckpointID string
	TenantID     string
	LeaseOwner   string
	FailureCode  string
	Now          time.Time
}

// PendingCheckpointFailure identifies one pending checkpoint without exposing
// or decrypting its opaque payload.
type PendingCheckpointFailure struct {
	CheckpointID   string
	RunID          string
	TenantID       string
	DefinitionHash string
	FailureCode    string
	Now            time.Time
}

// PendingCheckpointFailureStore is an optional capability for atomically
// failing an exact pending checkpoint during controlled host shutdown.
type PendingCheckpointFailureStore interface {
	FailPendingCheckpoint(context.Context, PendingCheckpointFailure) error
}

// CheckpointStore owns atomic state transitions for opaque approval checkpoints.
//
// Every implementation reports the same errors, which
// storetest.RunCheckpointStoreConformance checks: a duplicate CreateCheckpoint
// is ErrCheckpointNotClaimable; a checkpoint that does not exist for the
// tenant is ErrCheckpointNotFound; completing or failing a lease the caller
// does not hold is ErrCheckpointLeaseOwner.
type CheckpointStore interface {
	CreateCheckpoint(ctx context.Context, checkpoint ApprovalCheckpoint) error
	GetCheckpoint(ctx context.Context, checkpointID, tenantID string) (ApprovalCheckpoint, error)
	ApproveAndLease(ctx context.Context, request ApprovalLeaseRequest) (ApprovalCheckpoint, error)
	RejectCheckpoint(ctx context.Context, request ApprovalLeaseRequest) error
	CompleteLease(ctx context.Context, completion CheckpointLeaseCompletion) error
	FailLease(ctx context.Context, completion CheckpointLeaseCompletion) error
	ExpireCheckpoints(ctx context.Context, now time.Time) (int, error)
}

type MemoryCheckpointStore struct {
	mu          sync.Mutex
	checkpoints map[string]ApprovalCheckpoint
}

func NewMemoryCheckpointStore() *MemoryCheckpointStore {
	return &MemoryCheckpointStore{checkpoints: make(map[string]ApprovalCheckpoint)}
}

func (s *MemoryCheckpointStore) CreateCheckpoint(ctx context.Context, checkpoint ApprovalCheckpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	now := time.Now().UTC()
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = now
	}
	checkpoint.UpdatedAt = now
	checkpoint.Status = CheckpointPending
	checkpoint.FailureCode = ""
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.checkpoints[checkpoint.ID]; exists {
		return fmt.Errorf("%w: %s", ErrCheckpointNotClaimable, checkpoint.ID)
	}
	s.checkpoints[checkpoint.ID] = cloneApprovalCheckpoint(checkpoint)
	return nil
}

func (s *MemoryCheckpointStore) GetCheckpoint(ctx context.Context, checkpointID, tenantID string) (ApprovalCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return ApprovalCheckpoint{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, ok := s.checkpoints[checkpointID]
	if !ok || checkpoint.TenantID != tenantID {
		return ApprovalCheckpoint{}, ErrCheckpointNotFound
	}
	return cloneApprovalCheckpoint(checkpoint), nil
}

func (s *MemoryCheckpointStore) ApproveAndLease(ctx context.Context, request ApprovalLeaseRequest) (ApprovalCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return ApprovalCheckpoint{}, err
	}
	if err := validateLeaseRequest(request); err != nil {
		return ApprovalCheckpoint{}, err
	}
	now := requestNow(request.Now)
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, ok := s.checkpoints[request.CheckpointID]
	if !ok || !claimable(checkpoint, request, now) {
		return ApprovalCheckpoint{}, ErrCheckpointNotClaimable
	}
	checkpoint.Status = CheckpointLeased
	checkpoint.LeaseOwner = request.LeaseOwner
	checkpoint.LeaseUntil = now.Add(request.LeaseDuration)
	checkpoint.Approval = &ApprovalAudit{
		ApproverID: request.ApproverID,
		Approved:   true,
		AuditRef:   request.AuditRef,
		ReasonCode: request.ReasonCode,
		DecidedAt:  now,
	}
	checkpoint.UpdatedAt = now
	s.checkpoints[checkpoint.ID] = cloneApprovalCheckpoint(checkpoint)
	return cloneApprovalCheckpoint(checkpoint), nil
}

func (s *MemoryCheckpointStore) RejectCheckpoint(ctx context.Context, request ApprovalLeaseRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateResolutionRequest(request); err != nil {
		return err
	}
	now := requestNow(request.Now)
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, ok := s.checkpoints[request.CheckpointID]
	if !ok || checkpoint.TenantID != request.TenantID || checkpoint.DefinitionHash != request.DefinitionHash || checkpoint.Status != CheckpointPending || !checkpoint.ExpiresAt.After(now) {
		return ErrCheckpointNotClaimable
	}
	checkpoint.Status = CheckpointRejected
	checkpoint.Ciphertext = nil
	checkpoint.Approval = &ApprovalAudit{ApproverID: request.ApproverID, AuditRef: request.AuditRef, ReasonCode: request.ReasonCode, DecidedAt: now}
	checkpoint.UpdatedAt = now
	s.checkpoints[checkpoint.ID] = cloneApprovalCheckpoint(checkpoint)
	return nil
}

func (s *MemoryCheckpointStore) CompleteLease(ctx context.Context, completion CheckpointLeaseCompletion) error {
	return s.finishLease(ctx, completion, CheckpointConsumed)
}

func (s *MemoryCheckpointStore) FailLease(ctx context.Context, completion CheckpointLeaseCompletion) error {
	return s.finishLease(ctx, completion, CheckpointFailed)
}

func (s *MemoryCheckpointStore) FailPendingCheckpoint(ctx context.Context, request PendingCheckpointFailure) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePendingCheckpointFailure(request); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, ok := s.checkpoints[request.CheckpointID]
	if !ok ||
		checkpoint.RunID != request.RunID ||
		checkpoint.TenantID != request.TenantID ||
		checkpoint.DefinitionHash != request.DefinitionHash {
		return ErrCheckpointNotClaimable
	}
	if checkpoint.Status == CheckpointFailed && checkpoint.FailureCode == request.FailureCode {
		return nil
	}
	if checkpoint.Status != CheckpointPending {
		return ErrCheckpointNotClaimable
	}
	checkpoint.Status = CheckpointFailed
	checkpoint.Ciphertext = nil
	checkpoint.FailureCode = request.FailureCode
	checkpoint.LeaseOwner = ""
	checkpoint.LeaseUntil = time.Time{}
	checkpoint.UpdatedAt = requestNow(request.Now)
	s.checkpoints[checkpoint.ID] = cloneApprovalCheckpoint(checkpoint)
	return nil
}

func (s *MemoryCheckpointStore) finishLease(ctx context.Context, completion CheckpointLeaseCompletion, status CheckpointStatus) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(completion.CheckpointID) == "" || strings.TrimSpace(completion.TenantID) == "" || strings.TrimSpace(completion.LeaseOwner) == "" {
		return fmt.Errorf("checkpoint id, tenant id, and lease owner are required")
	}
	if status == CheckpointFailed && strings.TrimSpace(completion.FailureCode) == "" {
		return fmt.Errorf("failure code is required when failing a checkpoint lease")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, ok := s.checkpoints[completion.CheckpointID]
	if !ok || checkpoint.TenantID != completion.TenantID {
		return ErrCheckpointNotFound
	}
	if checkpoint.Status != CheckpointLeased || checkpoint.LeaseOwner != completion.LeaseOwner {
		return ErrCheckpointLeaseOwner
	}
	checkpoint.Status = status
	checkpoint.Ciphertext = nil
	if status == CheckpointFailed {
		checkpoint.FailureCode = completion.FailureCode
	} else {
		checkpoint.FailureCode = ""
	}
	checkpoint.LeaseOwner = ""
	checkpoint.LeaseUntil = time.Time{}
	checkpoint.UpdatedAt = requestNow(completion.Now)
	s.checkpoints[checkpoint.ID] = cloneApprovalCheckpoint(checkpoint)
	return nil
}

func (s *MemoryCheckpointStore) ExpireCheckpoints(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	now = requestNow(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	expired := 0
	for id, checkpoint := range s.checkpoints {
		if checkpointExpired(checkpoint, now) {
			checkpoint.Status = CheckpointExpired
			checkpoint.Ciphertext = nil
			checkpoint.LeaseOwner = ""
			checkpoint.LeaseUntil = time.Time{}
			checkpoint.UpdatedAt = now
			s.checkpoints[id] = cloneApprovalCheckpoint(checkpoint)
			expired++
		}
	}
	return expired, nil
}

// checkpointExpired reports whether ExpireCheckpoints expires checkpoint. A
// pending checkpoint expires at ExpiresAt. A leased one also waits for its
// lease to end: the approved tool may be running, and its holder must still
// be able to complete or fail the lease. A leased checkpoint whose holder
// crashed expires once both have passed, and is never handed out again.
func checkpointExpired(checkpoint ApprovalCheckpoint, now time.Time) bool {
	if checkpoint.ExpiresAt.After(now) {
		return false
	}
	switch checkpoint.Status {
	case CheckpointPending:
		return true
	case CheckpointLeased:
		return !checkpoint.LeaseUntil.After(now)
	default:
		return false
	}
}

func validateCheckpoint(checkpoint ApprovalCheckpoint) error {
	if strings.TrimSpace(checkpoint.ID) == "" || strings.TrimSpace(checkpoint.RunID) == "" || strings.TrimSpace(checkpoint.TenantID) == "" || strings.TrimSpace(checkpoint.DefinitionHash) == "" || len(checkpoint.Ciphertext) == 0 || checkpoint.ExpiresAt.IsZero() {
		return fmt.Errorf("checkpoint id, run id, tenant id, definition hash, ciphertext, and expiry are required")
	}
	return nil
}

func validatePendingCheckpointFailure(request PendingCheckpointFailure) error {
	if strings.TrimSpace(request.CheckpointID) == "" ||
		strings.TrimSpace(request.RunID) == "" ||
		strings.TrimSpace(request.TenantID) == "" ||
		strings.TrimSpace(request.DefinitionHash) == "" ||
		strings.TrimSpace(request.FailureCode) == "" {
		return fmt.Errorf("checkpoint id, run id, tenant id, definition hash, and failure code are required")
	}
	return nil
}

func validateResolutionRequest(request ApprovalLeaseRequest) error {
	if strings.TrimSpace(request.CheckpointID) == "" || strings.TrimSpace(request.TenantID) == "" || strings.TrimSpace(request.DefinitionHash) == "" || strings.TrimSpace(request.ApproverID) == "" || strings.TrimSpace(request.AuditRef) == "" {
		return fmt.Errorf("checkpoint id, tenant id, definition hash, approver id, and audit ref are required")
	}
	return nil
}

func validateLeaseRequest(request ApprovalLeaseRequest) error {
	if err := validateResolutionRequest(request); err != nil {
		return err
	}
	if strings.TrimSpace(request.LeaseOwner) == "" || request.LeaseDuration <= 0 {
		return fmt.Errorf("lease owner and positive lease duration are required")
	}
	return nil
}

func claimable(checkpoint ApprovalCheckpoint, request ApprovalLeaseRequest, now time.Time) bool {
	return checkpoint.TenantID == request.TenantID && checkpoint.DefinitionHash == request.DefinitionHash && checkpoint.Status == CheckpointPending && checkpoint.ExpiresAt.After(now)
}

func requestNow(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func cloneApprovalCheckpoint(checkpoint ApprovalCheckpoint) ApprovalCheckpoint {
	checkpoint.Ciphertext = append([]byte(nil), checkpoint.Ciphertext...)
	if checkpoint.Approval != nil {
		audit := *checkpoint.Approval
		checkpoint.Approval = &audit
	}
	return checkpoint
}
