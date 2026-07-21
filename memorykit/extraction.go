package memorykit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CandidateDraft is untrusted extracted content that has not been persisted or activated.
type CandidateDraft struct {
	Kind       Kind      `json:"kind"`
	Key        string    `json:"key"`
	Content    string    `json:"content"`
	ValidFrom  time.Time `json:"valid_from,omitempty"`
	ValidUntil time.Time `json:"valid_until,omitempty"`
	Importance int       `json:"importance,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
}

type ExtractionRequest struct {
	Scope                            Scope
	Source                           Source
	SourceAgentID, ExtractorID, Text string
	Now                              time.Time
}

type CandidateExtractor interface {
	Extract(context.Context, ExtractionRequest) ([]CandidateDraft, error)
}

type ContentValidator interface {
	ValidateMemoryContent(context.Context, string) error
}

// ValidateCandidateDraft delegates field validation to the create contract while
// fixing the lifecycle state to candidate and using validation-only identities.
func ValidateCandidateDraft(draft CandidateDraft, limits Limits) error {
	return ValidateCreateRequest(CreateRequest{
		ID:         "00000000-0000-4000-8000-000000000001",
		Scope:      Scope{TenantID: "validation", SubjectType: SubjectProject, SubjectID: "validation"},
		Kind:       draft.Kind,
		Key:        draft.Key,
		Status:     StatusCandidate,
		Content:    draft.Content,
		ValidFrom:  draft.ValidFrom,
		ValidUntil: draft.ValidUntil,
		Importance: draft.Importance,
		Confidence: draft.Confidence,
	}, limits)
}

type ExtractionJobStatus string

const (
	ExtractionPending   ExtractionJobStatus = "pending"
	ExtractionLeased    ExtractionJobStatus = "leased"
	ExtractionCompleted ExtractionJobStatus = "completed"
	ExtractionFailed    ExtractionJobStatus = "failed"
)

type ExtractionJob struct {
	ID            string
	Scope         Scope
	Source        Source
	SourceAgentID string
	ExtractorID   string
	Status        ExtractionJobStatus
	Attempts      int
	LeaseOwner    string
	LeaseUntil    time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type ExtractionJobStore interface {
	EnqueueExtraction(context.Context, ExtractionJob) error
	ClaimExtraction(context.Context, string, time.Duration, int, time.Time) (ExtractionJob, error)
	CompleteExtraction(context.Context, string, string, int, time.Time) error
	FailExtraction(context.Context, string, string, int, string, int, time.Time) error
}

type SourceReader interface {
	ReadSource(context.Context, Source) (string, error)
}

type ExtractionWorkerConfig struct {
	Jobs            ExtractionJobStore
	Memories        LifecycleStore
	Reader          SourceReader
	Extractor       CandidateExtractor
	ValidateContent ContentValidator
	Limits          Limits
	WorkerID        string
	LeaseDuration   time.Duration
	MaxAttempts     int
	Now             func() time.Time
}

func (c ExtractionWorkerConfig) Validate() error {
	if err := c.Limits.Validate(); err != nil {
		return err
	}
	if nilInterface(c.Jobs) || nilInterface(c.Memories) || nilInterface(c.Reader) ||
		nilInterface(c.Extractor) || nilInterface(c.ValidateContent) || c.Now == nil ||
		strings.TrimSpace(c.WorkerID) == "" || c.WorkerID != strings.TrimSpace(c.WorkerID) ||
		!validMetadata(c.WorkerID, c.Limits) || c.LeaseDuration < time.Microsecond ||
		c.MaxAttempts <= 0 || c.MaxAttempts > math.MaxInt32 {
		return fmt.Errorf("%w: invalid extraction worker configuration", ErrInvalidMemory)
	}
	return nil
}

type ExtractionWorker struct {
	jobs            ExtractionJobStore
	memories        LifecycleStore
	reader          SourceReader
	extractor       CandidateExtractor
	validateContent ContentValidator
	limits          Limits
	workerID        string
	leaseDuration   time.Duration
	maxAttempts     int
	now             func() time.Time
}

func NewExtractionWorker(config ExtractionWorkerConfig) (*ExtractionWorker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &ExtractionWorker{
		jobs: config.Jobs, memories: config.Memories, reader: config.Reader,
		extractor: config.Extractor, validateContent: config.ValidateContent,
		limits: config.Limits, workerID: config.WorkerID, leaseDuration: config.LeaseDuration,
		maxAttempts: config.MaxAttempts, now: config.Now,
	}, nil
}

// RunOnce owns at most one lease. Model-derived output is always persisted as
// candidate, regardless of confidence or extractor behavior.
func (w *ExtractionWorker) RunOnce(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	claimNow := w.now()
	leaseUntil := claimNow.Add(w.leaseDuration)
	if claimNow.IsZero() || !leaseUntil.After(claimNow) || leaseUntil.Unix() < claimNow.Unix() {
		return false, fmt.Errorf("%w: invalid extraction worker clock", ErrInvalidMemory)
	}
	job, err := w.jobs.ClaimExtraction(ctx, w.workerID, w.leaseDuration, w.maxAttempts, claimNow)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if err == nil {
			return true, w.fail(ctx, job, "source_read_failed", ctxErr, claimNow)
		}
		return false, ctxErr
	}
	if errors.Is(err, ErrNoExtractionJob) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if err := validateClaimedExtractionJob(job, w.workerID, w.maxAttempts, claimNow, w.limits); err != nil {
		return true, w.fail(ctx, job, "candidate_validate_failed", err, claimNow)
	}
	text, err := w.reader.ReadSource(ctx, job.Source)
	if err = contextError(ctx, err); err != nil {
		return true, w.fail(ctx, job, "source_read_failed", err, claimNow)
	}
	drafts, err := w.extractor.Extract(ctx, ExtractionRequest{
		Scope: job.Scope, Source: job.Source, SourceAgentID: job.SourceAgentID,
		ExtractorID: job.ExtractorID, Text: text, Now: claimNow,
	})
	if err = contextError(ctx, err); err != nil {
		return true, w.fail(ctx, job, "candidate_extract_failed", err, claimNow)
	}
	if len(drafts) > w.limits.MaxListItems {
		return true, w.fail(ctx, job, "candidate_validate_failed",
			fmt.Errorf("%w: extracted candidate count exceeds limit", ErrInvalidMemory), claimNow)
	}
	jobNamespace, err := uuid.Parse(job.ID)
	if err != nil {
		return true, w.fail(ctx, job, "candidate_validate_failed", ErrInvalidMemory, claimNow)
	}

	for index, draft := range drafts {
		if err := ValidateCandidateDraft(draft, w.limits); err != nil {
			return true, w.fail(ctx, job, "candidate_validate_failed", err, claimNow)
		}
		if err := contextError(ctx, w.validateContent.ValidateMemoryContent(ctx, draft.Content)); err != nil {
			return true, w.fail(ctx, job, "candidate_validate_failed", err, claimNow)
		}
		candidateID := uuid.NewSHA1(jobNamespace, []byte(fmt.Sprintf("candidate:%d", index))).String()
		if err := ValidateMemoryID(candidateID); err != nil {
			return true, w.fail(ctx, job, "candidate_validate_failed", err, claimNow)
		}
		request := CreateRequest{
			ID: candidateID, Scope: job.Scope, Kind: draft.Kind, Key: draft.Key,
			Status: StatusCandidate, Content: draft.Content,
			ValidFrom: draft.ValidFrom, ValidUntil: draft.ValidUntil,
			Importance: draft.Importance, Confidence: draft.Confidence,
			SourceAgentID: job.SourceAgentID, Actor: "extractor:" + job.ExtractorID,
			Reason:         "model-derived candidate",
			IdempotencyKey: fmt.Sprintf("extraction:%s:%d", job.ID, index),
			Sources:        []Source{job.Source}, Now: claimNow,
		}
		if err := ValidateCreateRequest(request, w.limits); err != nil {
			return true, w.fail(ctx, job, "candidate_validate_failed", err, claimNow)
		}
		writeNow, leaseErr := w.leaseTime(job, claimNow)
		if leaseErr != nil {
			return true, w.failAt(ctx, job, "candidate_write_failed", leaseErr, writeNow)
		}
		created, err := w.memories.Create(ctx, request)
		if err = contextError(ctx, err); err != nil {
			return true, w.fail(ctx, job, "candidate_write_failed", err, writeNow)
		}
		if err := validateCreateResult(created, request, w.limits, allowReviewedCandidateLifecycle); err != nil {
			return true, w.fail(ctx, job, "candidate_write_failed", err, writeNow)
		}
	}

	completeNow, clockErr := w.leaseTime(job, claimNow)
	if clockErr != nil {
		return true, w.failAt(ctx, job, "candidate_write_failed", clockErr, completeNow)
	}
	err = w.jobs.CompleteExtraction(ctx, job.ID, w.workerID, job.Attempts, completeNow)
	if err = contextError(ctx, err); err != nil {
		return true, w.fail(ctx, job, "candidate_write_failed", err, completeNow)
	}
	return true, nil
}

func (w *ExtractionWorker) fail(
	ctx context.Context,
	job ExtractionJob,
	code string,
	workErr error,
	notBefore time.Time,
) error {
	transitionNow, clockErr := w.transitionTime(notBefore)
	if clockErr != nil {
		workErr = errors.Join(workErr, clockErr)
	}
	return w.failAt(ctx, job, code, workErr, transitionNow)
}

func (w *ExtractionWorker) failAt(
	ctx context.Context,
	job ExtractionJob,
	code string,
	workErr error,
	transitionNow time.Time,
) error {
	transitionErr := w.jobs.FailExtraction(ctx, job.ID, w.workerID, job.Attempts, code, w.maxAttempts, transitionNow)
	if transitionErr != nil {
		return errors.Join(workErr, transitionErr)
	}
	return workErr
}

func (w *ExtractionWorker) transitionTime(notBefore time.Time) (time.Time, error) {
	now := w.now()
	if now.IsZero() || now.Before(notBefore) {
		return notBefore, fmt.Errorf("%w: invalid extraction worker clock", ErrInvalidMemory)
	}
	return now, nil
}

func (w *ExtractionWorker) leaseTime(job ExtractionJob, notBefore time.Time) (time.Time, error) {
	now, err := w.transitionTime(notBefore)
	if err != nil {
		return now, err
	}
	if !now.Before(job.LeaseUntil) {
		return now, fmt.Errorf("%w: extraction lease expired", ErrConflict)
	}
	return now, nil
}

func validateClaimedExtractionJob(
	job ExtractionJob,
	workerID string,
	maxAttempts int,
	claimNow time.Time,
	limits Limits,
) error {
	if err := ValidateMemoryID(job.ID); err != nil {
		return err
	}
	if err := job.Scope.Validate(); err != nil {
		return err
	}
	if err := ValidateSources([]Source{job.Source}, limits); err != nil {
		return err
	}
	if job.Status != ExtractionLeased || job.LeaseOwner != workerID || job.Attempts <= 0 ||
		job.Attempts > maxAttempts || !job.LeaseUntil.After(claimNow) || job.CreatedAt.IsZero() || job.UpdatedAt.IsZero() ||
		job.CreatedAt.After(job.UpdatedAt) || job.UpdatedAt.After(claimNow) ||
		strings.TrimSpace(job.Source.Kind) == "" || strings.TrimSpace(job.Source.Ref) == "" ||
		job.SourceAgentID != strings.TrimSpace(job.SourceAgentID) ||
		strings.TrimSpace(job.ExtractorID) == "" || job.ExtractorID != strings.TrimSpace(job.ExtractorID) ||
		!validMetadata(job.SourceAgentID, limits) || !validMetadata(job.ExtractorID, limits) ||
		!validMetadata("extractor:"+job.ExtractorID, limits) {
		return fmt.Errorf("%w: invalid claimed extraction job", ErrInvalidMemory)
	}
	return nil
}

func contextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

type TrustedProjection struct {
	Create  CreateRequest
	EventID string
}

// ProjectTrusted maps an already-approved deterministic event directly to an
// active memory. It never invokes a model or converts candidate state.
func ProjectTrusted(
	ctx context.Context,
	store LifecycleStore,
	validator ContentValidator,
	limits Limits,
	projection TrustedProjection,
) (Memory, error) {
	if err := ctx.Err(); err != nil {
		return Memory{}, err
	}
	if err := limits.Validate(); err != nil {
		return Memory{}, err
	}
	if nilInterface(store) || nilInterface(validator) || projection.Create.Status != StatusActive ||
		strings.TrimSpace(projection.EventID) == "" {
		return Memory{}, fmt.Errorf("%w: invalid trusted projection", ErrInvalidMemory)
	}
	request := projection.Create
	request.Status = StatusActive
	request.IdempotencyKey = "trusted-event:" + projection.EventID
	if request.Now.IsZero() {
		return Memory{}, fmt.Errorf("%w: trusted projection clock is required", ErrInvalidMemory)
	}
	if err := ValidateCreateRequest(request, limits); err != nil {
		return Memory{}, err
	}
	if err := validator.ValidateMemoryContent(ctx, request.Content); err != nil {
		return Memory{}, contextError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return Memory{}, err
	}
	created, err := store.Create(ctx, request)
	if err = contextError(ctx, err); err != nil {
		return Memory{}, err
	}
	if err := validateCreateResult(created, request, limits, allowActiveOnly); err != nil {
		return Memory{}, err
	}
	return created, nil
}

type replayStatusPolicy func(Status) bool

func allowReviewedCandidateLifecycle(status Status) bool {
	return status == StatusCandidate || status == StatusActive || status == StatusInactive
}

func allowActiveOnly(status Status) bool { return status == StatusActive }

func validateCreateResult(
	memory Memory,
	request CreateRequest,
	limits Limits,
	allowReplay replayStatusPolicy,
) error {
	if memory.Version <= 0 || memory.ID != request.ID || memory.Scope != request.Scope ||
		memory.Kind != request.Kind || memory.Key != request.Key || memory.CreatedBy != request.Actor ||
		memory.SourceAgentID != request.SourceAgentID || memory.IdempotencyKey != request.IdempotencyKey ||
		memory.CreatedAt.IsZero() || memory.UpdatedAt.IsZero() || memory.UpdatedAt.Before(memory.CreatedAt) ||
		memory.CreatedAt.Sub(request.Now) > time.Microsecond {
		return ErrInvalidRecallResult
	}
	if memory.Version == 1 {
		if memory.Status != request.Status || memory.Content != request.Content ||
			!equivalentStoreTime(memory.ValidFrom, request.ValidFrom) ||
			!equivalentStoreTime(memory.ValidUntil, request.ValidUntil) ||
			memory.Importance != request.Importance || memory.Confidence != request.Confidence ||
			!equivalentStoreTime(memory.UpdatedAt, memory.CreatedAt) {
			return ErrInvalidRecallResult
		}
	} else if allowReplay == nil || !allowReplay(memory.Status) {
		return ErrInvalidRecallResult
	}
	if err := ValidateCreateRequest(CreateRequest{
		ID: memory.ID, Scope: memory.Scope, Kind: memory.Kind, Key: memory.Key,
		Status: memory.Status, Content: memory.Content,
		ValidFrom: memory.ValidFrom, ValidUntil: memory.ValidUntil,
		Importance: memory.Importance, Confidence: memory.Confidence,
		SourceAgentID: memory.SourceAgentID, Actor: memory.CreatedBy,
		IdempotencyKey: memory.IdempotencyKey,
	}, limits); err != nil {
		return ErrInvalidRecallResult
	}
	return nil
}

func equivalentStoreTime(left, right time.Time) bool {
	if left.IsZero() != right.IsZero() {
		return false
	}
	later, earlier := left, right
	if later.Before(earlier) {
		later, earlier = earlier, later
	}
	return later.Sub(earlier) <= time.Microsecond
}
