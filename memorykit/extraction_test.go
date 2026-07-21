package memorykit

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidateCandidateDraftReusesCreateValidation(t *testing.T) {
	limits := extractionTestLimits()
	valid := CandidateDraft{
		Kind: KindLesson, Key: "testing.pgvector", Content: "Run the real integration gate",
		ValidFrom: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC), Importance: 10, Confidence: 0.82,
	}
	if err := ValidateCandidateDraft(valid, limits); err != nil {
		t.Fatalf("ValidateCandidateDraft(valid) = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*CandidateDraft)
	}{
		{name: "kind", mutate: func(d *CandidateDraft) { d.Kind = Kind("other") }},
		{name: "key", mutate: func(d *CandidateDraft) { d.Key = "" }},
		{name: "content", mutate: func(d *CandidateDraft) { d.Content = "012345678901234567890123456789012345678901234567890" }},
		{name: "time range", mutate: func(d *CandidateDraft) { d.ValidUntil = d.ValidFrom }},
		{name: "importance", mutate: func(d *CandidateDraft) { d.Importance = 101 }},
		{name: "confidence", mutate: func(d *CandidateDraft) { d.Confidence = math.NaN() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			draft := valid
			test.mutate(&draft)
			if err := ValidateCandidateDraft(draft, limits); !errors.Is(err, ErrInvalidMemory) {
				t.Fatalf("ValidateCandidateDraft() = %v, want ErrInvalidMemory", err)
			}
		})
	}
}

func extractionTestLimits() Limits {
	return Limits{
		Version: "test-v1", MaxKeyRunes: 40, MaxContentRunes: 50,
		MaxMetadataRunes: 100, MaxSourcesPerMemory: 4, MaxListItems: 10,
	}
}

var _ CandidateExtractor = candidateExtractorFunc(nil)

type candidateExtractorFunc func(context.Context, ExtractionRequest) ([]CandidateDraft, error)

func (f candidateExtractorFunc) Extract(ctx context.Context, request ExtractionRequest) ([]CandidateDraft, error) {
	return f(ctx, request)
}

func TestNewExtractionWorkerRejectsIncompleteConfiguration(t *testing.T) {
	valid := extractionWorkerTestConfig()
	tests := []struct {
		name   string
		mutate func(*ExtractionWorkerConfig)
	}{
		{name: "jobs", mutate: func(c *ExtractionWorkerConfig) { c.Jobs = nil }},
		{name: "memories", mutate: func(c *ExtractionWorkerConfig) { c.Memories = nil }},
		{name: "reader", mutate: func(c *ExtractionWorkerConfig) { c.Reader = nil }},
		{name: "extractor", mutate: func(c *ExtractionWorkerConfig) { c.Extractor = nil }},
		{name: "content validator", mutate: func(c *ExtractionWorkerConfig) { c.ValidateContent = nil }},
		{name: "limits", mutate: func(c *ExtractionWorkerConfig) { c.Limits = Limits{} }},
		{name: "worker ID", mutate: func(c *ExtractionWorkerConfig) { c.WorkerID = " " }},
		{name: "oversized worker ID", mutate: func(c *ExtractionWorkerConfig) {
			c.WorkerID = strings.Repeat("w", c.Limits.MaxMetadataRunes+1)
		}},
		{name: "lease duration", mutate: func(c *ExtractionWorkerConfig) { c.LeaseDuration = 0 }},
		{name: "sub-microsecond lease", mutate: func(c *ExtractionWorkerConfig) { c.LeaseDuration = time.Nanosecond }},
		{name: "max attempts", mutate: func(c *ExtractionWorkerConfig) { c.MaxAttempts = 0 }},
		{name: "max attempts overflow", mutate: func(c *ExtractionWorkerConfig) { c.MaxAttempts = int(math.MaxInt32) + 1 }},
		{name: "clock", mutate: func(c *ExtractionWorkerConfig) { c.Now = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			worker, err := NewExtractionWorker(config)
			if worker != nil || !errors.Is(err, ErrInvalidMemory) {
				t.Fatalf("NewExtractionWorker() = %#v, %v, want nil ErrInvalidMemory", worker, err)
			}
		})
	}
}

func TestExtractionWorkerStopsBeforeCandidateWriteWhenLeaseExpires(t *testing.T) {
	claimNow := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	job := extractionWorkerTestJob()
	clockCalls := 0
	jobs := &extractionJobStoreStub{claimJob: job}
	memories := &extractionLifecycleStoreStub{}
	config := extractionWorkerTestConfig()
	config.Jobs, config.Memories = jobs, memories
	config.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
		return []CandidateDraft{{Kind: KindLesson, Key: "lease.fence", Content: "never write after lease expiry"}}, nil
	})
	config.Now = func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return claimNow
		}
		return job.LeaseUntil
	}
	worker, err := NewExtractionWorker(config)
	if err != nil {
		t.Fatal(err)
	}

	worked, err := worker.RunOnce(context.Background())
	if !worked || err == nil {
		t.Fatalf("RunOnce() = %v, %v, want true and lease error", worked, err)
	}
	if len(memories.creates) != 0 || jobs.completed {
		t.Fatalf("expired lease wrote/completed = %d/%v", len(memories.creates), jobs.completed)
	}
	if jobs.failAttempt != job.Attempts {
		t.Fatalf("FailExtraction attempt = %d, want %d", jobs.failAttempt, job.Attempts)
	}
}

func TestExtractionWorkerDoesNotCompleteAtLeaseDeadline(t *testing.T) {
	job := extractionWorkerTestJob()
	clockCalls := 0
	transitionErr := errors.New("expired fail transition")
	jobs := &extractionJobStoreStub{claimJob: job, failErr: transitionErr}
	config := extractionWorkerTestConfig()
	config.Jobs = jobs
	config.Now = func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return job.UpdatedAt
		}
		return job.LeaseUntil
	}
	worker, err := NewExtractionWorker(config)
	if err != nil {
		t.Fatal(err)
	}

	worked, err := worker.RunOnce(context.Background())
	if !worked || !errors.Is(err, ErrConflict) || !errors.Is(err, transitionErr) {
		t.Fatalf("RunOnce() = %v, %v, want true with lease and transition errors", worked, err)
	}
	if jobs.completed || jobs.completeAttempt != 0 || jobs.failAttempt != job.Attempts {
		t.Fatalf("deadline transitions = complete:%v completeAttempt:%d failAttempt:%d",
			jobs.completed, jobs.completeAttempt, jobs.failAttempt)
	}
	if !jobs.failedAt.Equal(job.LeaseUntil) {
		t.Fatalf("FailExtraction now = %v, want fresh lease deadline %v", jobs.failedAt, job.LeaseUntil)
	}
}

func TestExtractionWorkerRejectsInvalidClockBeforeClaim(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
	}{
		{name: "zero", now: time.Time{}},
		{name: "lease overflow", now: time.Unix(int64(^uint64(0)>>1), 0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobs := &extractionJobStoreStub{}
			config := extractionWorkerTestConfig()
			config.Jobs = jobs
			config.Now = func() time.Time { return test.now }
			worker, err := NewExtractionWorker(config)
			if err != nil {
				t.Fatal(err)
			}
			worked, err := worker.RunOnce(context.Background())
			if worked || !errors.Is(err, ErrInvalidMemory) {
				t.Fatalf("RunOnce() = %v, %v, want false ErrInvalidMemory", worked, err)
			}
			if jobs.claimCalls != 0 {
				t.Fatalf("ClaimExtraction calls = %d, want 0", jobs.claimCalls)
			}
		})
	}
}

func TestExtractionWorkerCreatesCandidatesThenCompletesLease(t *testing.T) {
	job := extractionWorkerTestJob()
	jobs := &extractionJobStoreStub{claimJob: job}
	memories := &extractionLifecycleStoreStub{}
	config := extractionWorkerTestConfig()
	config.Jobs = jobs
	config.Memories = memories
	config.Reader = sourceReaderFunc(func(_ context.Context, source Source) (string, error) {
		if source != job.Source {
			t.Fatalf("ReadSource() source = %#v, want %#v", source, job.Source)
		}
		return "approved terminal output", nil
	})
	config.Extractor = candidateExtractorFunc(func(_ context.Context, request ExtractionRequest) ([]CandidateDraft, error) {
		if request.Scope != job.Scope || request.Source != job.Source || request.SourceAgentID != job.SourceAgentID ||
			request.ExtractorID != job.ExtractorID || request.Text != "approved terminal output" {
			t.Fatalf("Extract() request = %#v", request)
		}
		return []CandidateDraft{
			{Kind: KindLesson, Key: "testing.pgvector", Content: "Run the real integration gate", Confidence: 1},
			{Kind: KindDecision, Key: "release.strategy", Content: "Use the conservative release gate", Confidence: .99},
		}, nil
	})
	worker, err := NewExtractionWorker(config)
	if err != nil {
		t.Fatal(err)
	}

	worked, err := worker.RunOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("RunOnce() = %v, %v, want true, nil", worked, err)
	}
	if !jobs.completed || jobs.failedCode != "" {
		t.Fatalf("job transition complete/failed = %v/%q", jobs.completed, jobs.failedCode)
	}
	if jobs.completeAttempt != job.Attempts {
		t.Fatalf("CompleteExtraction attempt = %d, want %d", jobs.completeAttempt, job.Attempts)
	}
	if len(memories.creates) != 2 {
		t.Fatalf("Create() calls = %d, want 2", len(memories.creates))
	}
	for index, request := range memories.creates {
		wantID := uuid.NewSHA1(uuid.MustParse(job.ID), []byte("candidate:"+string(rune('0'+index)))).String()
		if request.ID != wantID {
			t.Errorf("Create[%d].ID = %q, want %q", index, request.ID, wantID)
		}
		if request.Status != StatusCandidate {
			t.Errorf("Create[%d].Status = %q, want candidate", index, request.Status)
		}
		if request.IdempotencyKey != "extraction:"+job.ID+":"+string(rune('0'+index)) ||
			request.SourceAgentID != job.SourceAgentID || request.Actor != "extractor:"+job.ExtractorID ||
			request.Reason != "model-derived candidate" || !reflect.DeepEqual(request.Sources, []Source{job.Source}) {
			t.Errorf("Create[%d] provenance = %#v", index, request)
		}
	}
}

func TestExtractionWorkerNoJobIsIdle(t *testing.T) {
	config := extractionWorkerTestConfig()
	config.Jobs = &extractionJobStoreStub{claimErr: ErrNoExtractionJob}
	worker, err := NewExtractionWorker(config)
	if err != nil {
		t.Fatal(err)
	}
	worked, err := worker.RunOnce(context.Background())
	if err != nil || worked {
		t.Fatalf("RunOnce() = %v, %v, want false, nil", worked, err)
	}
}

func TestExtractionWorkerFailsLeaseBeforeReturningWorkError(t *testing.T) {
	readErr := errors.New("private source payload must not be persisted")
	jobs := &extractionJobStoreStub{claimJob: extractionWorkerTestJob()}
	config := extractionWorkerTestConfig()
	config.Jobs = jobs
	config.Reader = sourceReaderFunc(func(context.Context, Source) (string, error) { return "", readErr })
	worker, err := NewExtractionWorker(config)
	if err != nil {
		t.Fatal(err)
	}

	worked, err := worker.RunOnce(context.Background())
	if !worked || !errors.Is(err, readErr) {
		t.Fatalf("RunOnce() = %v, %v, want true and source error", worked, err)
	}
	if jobs.failedCode != "source_read_failed" || jobs.completed {
		t.Fatalf("job transition failed/complete = %q/%v", jobs.failedCode, jobs.completed)
	}
	if jobs.failAttempt != jobs.claimJob.Attempts {
		t.Fatalf("FailExtraction attempt = %d, want %d", jobs.failAttempt, jobs.claimJob.Attempts)
	}
	if strings.Contains(jobs.failedCode, "private") {
		t.Fatalf("failure code leaked raw error: %q", jobs.failedCode)
	}
}

func TestExtractionWorkerUsesContentFreeFailureCodeForEachStage(t *testing.T) {
	stageErr := errors.New("raw private dependency error")
	tests := []struct {
		name     string
		wantCode string
		mutate   func(*ExtractionWorkerConfig)
	}{
		{name: "extract", wantCode: "candidate_extract_failed", mutate: func(c *ExtractionWorkerConfig) {
			c.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
				return nil, stageErr
			})
		}},
		{name: "draft validation", wantCode: "candidate_validate_failed", mutate: func(c *ExtractionWorkerConfig) {
			c.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
				return []CandidateDraft{{Kind: Kind("malicious"), Key: "key", Content: "content"}}, nil
			})
		}},
		{name: "content validation", wantCode: "candidate_validate_failed", mutate: func(c *ExtractionWorkerConfig) {
			c.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
				return []CandidateDraft{{Kind: KindLesson, Key: "key", Content: "content"}}, nil
			})
			c.ValidateContent = &contentValidatorStub{err: stageErr}
		}},
		{name: "write", wantCode: "candidate_write_failed", mutate: func(c *ExtractionWorkerConfig) {
			c.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
				return []CandidateDraft{{Kind: KindLesson, Key: "key", Content: "content"}}, nil
			})
			c.Memories = &extractionLifecycleStoreStub{createErr: stageErr}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobs := &extractionJobStoreStub{claimJob: extractionWorkerTestJob()}
			config := extractionWorkerTestConfig()
			config.Jobs = jobs
			test.mutate(&config)
			worker, err := NewExtractionWorker(config)
			if err != nil {
				t.Fatal(err)
			}
			worked, err := worker.RunOnce(context.Background())
			if !worked || err == nil {
				t.Fatalf("RunOnce() = %v, %v, want true and error", worked, err)
			}
			if jobs.failedCode != test.wantCode || strings.Contains(jobs.failedCode, "private") {
				t.Fatalf("failure code = %q, want %q", jobs.failedCode, test.wantCode)
			}
		})
	}
}

func TestExtractionWorkerRejectsMalformedCreateResultBeforeCompletion(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Memory)
	}{
		{name: "foreign scope", mutate: func(memory *Memory) { memory.Scope.SubjectID = "project-foreign" }},
		{name: "version one active", mutate: func(memory *Memory) { memory.Status = StatusActive }},
		{name: "zero version", mutate: func(memory *Memory) { memory.Version = 0 }},
		{name: "erased replay", mutate: func(memory *Memory) {
			memory.Version, memory.Status, memory.Content = 2, StatusInactive, ""
			memory.UpdatedAt = memory.UpdatedAt.Add(time.Second)
		}},
		{name: "wrong provenance", mutate: func(memory *Memory) {
			memory.Version, memory.CreatedBy = 2, "other-actor"
			memory.UpdatedAt = memory.UpdatedAt.Add(time.Second)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobs := &extractionJobStoreStub{claimJob: extractionWorkerTestJob()}
			memories := &extractionLifecycleStoreStub{mutateResult: test.mutate}
			config := extractionWorkerTestConfig()
			config.Jobs, config.Memories = jobs, memories
			config.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
				return []CandidateDraft{{Kind: KindLesson, Key: "return.integrity", Content: "validate store output"}}, nil
			})
			worker, err := NewExtractionWorker(config)
			if err != nil {
				t.Fatal(err)
			}

			worked, err := worker.RunOnce(context.Background())
			if !worked || !errors.Is(err, ErrInvalidRecallResult) {
				t.Fatalf("RunOnce() = %v, %v, want true ErrInvalidRecallResult", worked, err)
			}
			if jobs.completed || jobs.failedCode != "candidate_write_failed" {
				t.Fatalf("malformed result transition = complete:%v failure:%q", jobs.completed, jobs.failedCode)
			}
		})
	}
}

func TestExtractionWorkerAcceptsValidReviewedLifecycleReplay(t *testing.T) {
	for _, status := range []Status{StatusCandidate, StatusActive, StatusInactive} {
		t.Run(string(status), func(t *testing.T) {
			jobs := &extractionJobStoreStub{claimJob: extractionWorkerTestJob()}
			memories := &extractionLifecycleStoreStub{mutateResult: func(memory *Memory) {
				memory.Version, memory.Status = 2, status
				memory.Content = "reviewed lifecycle content"
				memory.UpdatedAt = memory.UpdatedAt.Add(time.Second)
			}}
			config := extractionWorkerTestConfig()
			config.Jobs, config.Memories = jobs, memories
			config.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
				return []CandidateDraft{{Kind: KindLesson, Key: "return.lifecycle", Content: "original candidate"}}, nil
			})
			worker, err := NewExtractionWorker(config)
			if err != nil {
				t.Fatal(err)
			}
			worked, err := worker.RunOnce(context.Background())
			if err != nil || !worked || !jobs.completed {
				t.Fatalf("RunOnce() = %v, %v, completed=%v", worked, err, jobs.completed)
			}
		})
	}
}

func TestExtractionWorkerJoinsFailureTransitionError(t *testing.T) {
	workErr := errors.New("extract failed")
	transitionErr := errors.New("fail transition failed")
	jobs := &extractionJobStoreStub{claimJob: extractionWorkerTestJob(), failErr: transitionErr}
	config := extractionWorkerTestConfig()
	config.Jobs = jobs
	config.Extractor = candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) {
		return nil, workErr
	})
	worker, err := NewExtractionWorker(config)
	if err != nil {
		t.Fatal(err)
	}

	worked, err := worker.RunOnce(context.Background())
	if !worked || !errors.Is(err, workErr) || !errors.Is(err, transitionErr) {
		t.Fatalf("RunOnce() = %v, %v, want joined work and transition errors", worked, err)
	}
}

func TestExtractionWorkerPreservesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := &extractionJobStoreStub{claimJob: extractionWorkerTestJob()}
	config := extractionWorkerTestConfig()
	config.Jobs = jobs
	config.Reader = sourceReaderFunc(func(context.Context, Source) (string, error) {
		cancel()
		return "", errors.New("reader wrapper")
	})
	worker, err := NewExtractionWorker(config)
	if err != nil {
		t.Fatal(err)
	}

	worked, err := worker.RunOnce(ctx)
	if !worked || !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce() = %v, %v, want true and context.Canceled", worked, err)
	}
	if jobs.failedCode != "source_read_failed" {
		t.Fatalf("FailExtraction code = %q, want source_read_failed", jobs.failedCode)
	}
}

func TestExtractionWorkerTransitionsLeaseWhenClaimCompletesDuringCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := &extractionJobStoreStub{
		claimJob:  extractionWorkerTestJob(),
		claimHook: cancel,
	}
	config := extractionWorkerTestConfig()
	config.Jobs = jobs
	worker, err := NewExtractionWorker(config)
	if err != nil {
		t.Fatal(err)
	}

	worked, err := worker.RunOnce(ctx)
	if !worked || !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce() = %v, %v, want true and context.Canceled", worked, err)
	}
	if jobs.failedCode != "source_read_failed" {
		t.Fatalf("FailExtraction code = %q, want source_read_failed", jobs.failedCode)
	}
}

func TestExtractionWorkerInvalidCompletionClockFailsLeaseWithLastValidTime(t *testing.T) {
	claimNow := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	clockCalls := 0
	jobs := &extractionJobStoreStub{claimJob: extractionWorkerTestJob()}
	config := extractionWorkerTestConfig()
	config.Jobs = jobs
	config.Now = func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return claimNow
		}
		return time.Time{}
	}
	worker, err := NewExtractionWorker(config)
	if err != nil {
		t.Fatal(err)
	}

	worked, err := worker.RunOnce(context.Background())
	if !worked || !errors.Is(err, ErrInvalidMemory) {
		t.Fatalf("RunOnce() = %v, %v, want true ErrInvalidMemory", worked, err)
	}
	if jobs.completed || jobs.failedCode != "candidate_write_failed" || !jobs.failedAt.Equal(claimNow) {
		t.Fatalf("invalid clock transition = complete:%v code:%q at:%v", jobs.completed, jobs.failedCode, jobs.failedAt)
	}
}

func TestExtractionWorkerRejectsOversizedClaimedProvenance(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ExtractionJob)
	}{
		{name: "source agent", mutate: func(job *ExtractionJob) {
			job.SourceAgentID = strings.Repeat("a", extractionTestLimits().MaxMetadataRunes+1)
		}},
		{name: "extractor actor prefix", mutate: func(job *ExtractionJob) {
			job.ExtractorID = strings.Repeat("e", extractionTestLimits().MaxMetadataRunes-len("extractor:")+1)
		}},
		{name: "expired lease", mutate: func(job *ExtractionJob) { job.LeaseUntil = job.UpdatedAt }},
		{name: "created after updated", mutate: func(job *ExtractionJob) {
			job.CreatedAt = job.UpdatedAt.Add(time.Second)
		}},
		{name: "updated after claim", mutate: func(job *ExtractionJob) {
			job.UpdatedAt = job.UpdatedAt.Add(time.Second)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := extractionWorkerTestJob()
			test.mutate(&job)
			jobs := &extractionJobStoreStub{claimJob: job}
			memories := &extractionLifecycleStoreStub{}
			config := extractionWorkerTestConfig()
			config.Jobs, config.Memories = jobs, memories
			worker, err := NewExtractionWorker(config)
			if err != nil {
				t.Fatal(err)
			}
			worked, err := worker.RunOnce(context.Background())
			if !worked || !errors.Is(err, ErrInvalidMemory) || jobs.failedCode != "candidate_validate_failed" {
				t.Fatalf("RunOnce() = %v, %v, failure=%q", worked, err, jobs.failedCode)
			}
			if len(memories.creates) != 0 {
				t.Fatal("worker persisted malformed claimed provenance")
			}
		})
	}
}

func TestProjectTrustedRequiresActiveValidatedEvent(t *testing.T) {
	validator := &contentValidatorStub{}
	store := &extractionLifecycleStoreStub{}
	projection := trustedProjectionTestValue()

	created, err := ProjectTrusted(context.Background(), store, validator, extractionTestLimits(), projection)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != StatusActive || len(store.creates) != 1 || validator.calls != 1 {
		t.Fatalf("ProjectTrusted() = %#v, creates=%d validator=%d", created, len(store.creates), validator.calls)
	}
	if store.creates[0].IdempotencyKey != "trusted-event:event-42" {
		t.Fatalf("trusted idempotency = %q", store.creates[0].IdempotencyKey)
	}

	projection.Create.Status = StatusCandidate
	if _, err := ProjectTrusted(context.Background(), store, validator, extractionTestLimits(), projection); !errors.Is(err, ErrInvalidMemory) {
		t.Fatalf("candidate ProjectTrusted() error = %v, want ErrInvalidMemory", err)
	}
	projection.Create.Status = StatusActive
	projection.EventID = " \t"
	if _, err := ProjectTrusted(context.Background(), store, validator, extractionTestLimits(), projection); !errors.Is(err, ErrInvalidMemory) {
		t.Fatalf("blank event ProjectTrusted() error = %v, want ErrInvalidMemory", err)
	}
	projection.EventID = "event-42"
	if _, err := ProjectTrusted(context.Background(), store, validator, Limits{}, projection); !errors.Is(err, ErrInvalidMemory) {
		t.Fatalf("invalid limits ProjectTrusted() error = %v, want ErrInvalidMemory", err)
	}
	projection.Create.Now = time.Time{}
	createsBefore := len(store.creates)
	if _, err := ProjectTrusted(context.Background(), store, validator, extractionTestLimits(), projection); !errors.Is(err, ErrInvalidMemory) {
		t.Fatalf("zero clock ProjectTrusted() error = %v, want ErrInvalidMemory", err)
	}
	if len(store.creates) != createsBefore {
		t.Fatal("ProjectTrusted wrote with zero clock")
	}
}

func TestProjectTrustedPreservesValidatorAndContextErrors(t *testing.T) {
	projection := trustedProjectionTestValue()
	validationErr := errors.New("sensitive content")
	validator := &contentValidatorStub{err: validationErr}
	store := &extractionLifecycleStoreStub{}
	if _, err := ProjectTrusted(context.Background(), store, validator, extractionTestLimits(), projection); !errors.Is(err, validationErr) {
		t.Fatalf("ProjectTrusted() validation error = %v", err)
	}
	if len(store.creates) != 0 {
		t.Fatal("ProjectTrusted wrote rejected content")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	validator.err = nil
	if _, err := ProjectTrusted(ctx, store, validator, extractionTestLimits(), projection); !errors.Is(err, context.Canceled) {
		t.Fatalf("ProjectTrusted() cancellation = %v, want context.Canceled", err)
	}
}

func TestProjectTrustedRejectsMalformedStoreResult(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Memory)
	}{
		{name: "candidate", mutate: func(memory *Memory) { memory.Status = StatusCandidate }},
		{name: "candidate replay", mutate: func(memory *Memory) {
			memory.Version, memory.Status = 2, StatusCandidate
			memory.UpdatedAt = memory.UpdatedAt.Add(time.Second)
		}},
		{name: "foreign scope", mutate: func(memory *Memory) { memory.Scope.SubjectID = "project-foreign" }},
		{name: "zero version", mutate: func(memory *Memory) { memory.Version = 0 }},
		{name: "inactive replay", mutate: func(memory *Memory) {
			memory.Version, memory.Status = 2, StatusInactive
			memory.UpdatedAt = memory.UpdatedAt.Add(time.Second)
		}},
		{name: "version one valid from extreme past", mutate: func(memory *Memory) {
			memory.ValidFrom = time.Date(100, time.January, 1, 0, 0, 0, 0, time.UTC)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &extractionLifecycleStoreStub{mutateResult: test.mutate}
			_, err := ProjectTrusted(context.Background(), store, &contentValidatorStub{}, extractionTestLimits(), trustedProjectionTestValue())
			if !errors.Is(err, ErrInvalidRecallResult) {
				t.Fatalf("ProjectTrusted() error = %v, want ErrInvalidRecallResult", err)
			}
		})
	}
}

func TestProjectTrustedAcceptsMicrosecondCanonicalTimeAndActiveReplay(t *testing.T) {
	projection := trustedProjectionTestValue()
	store := &extractionLifecycleStoreStub{mutateResult: func(memory *Memory) {
		memory.ValidFrom = memory.ValidFrom.Add(-time.Microsecond)
		memory.CreatedAt = memory.CreatedAt.Add(time.Microsecond)
		memory.UpdatedAt = memory.UpdatedAt.Add(time.Microsecond)
	}}
	if _, err := ProjectTrusted(context.Background(), store, &contentValidatorStub{}, extractionTestLimits(), projection); err != nil {
		t.Fatalf("microsecond canonical ProjectTrusted() = %v", err)
	}

	store.mutateResult = func(memory *Memory) {
		memory.Version = 2
		memory.Content = "corrected active trusted content"
		memory.UpdatedAt = memory.UpdatedAt.Add(time.Second)
	}
	if _, err := ProjectTrusted(context.Background(), store, &contentValidatorStub{}, extractionTestLimits(), projection); err != nil {
		t.Fatalf("active replay ProjectTrusted() = %v", err)
	}
}

func extractionWorkerTestConfig() ExtractionWorkerConfig {
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	return ExtractionWorkerConfig{
		Jobs:            &extractionJobStoreStub{claimJob: extractionWorkerTestJob()},
		Memories:        &extractionLifecycleStoreStub{},
		Reader:          sourceReaderFunc(func(context.Context, Source) (string, error) { return "source", nil }),
		Extractor:       candidateExtractorFunc(func(context.Context, ExtractionRequest) ([]CandidateDraft, error) { return nil, nil }),
		ValidateContent: &contentValidatorStub{}, Limits: extractionTestLimits(), WorkerID: "worker-1",
		LeaseDuration: time.Minute, MaxAttempts: 3,
		Now: func() time.Time { return now },
	}
}

func extractionWorkerTestJob() ExtractionJob {
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	return ExtractionJob{
		ID:            "91111111-1111-4111-8111-111111111111",
		Scope:         Scope{TenantID: "tenant-1", SubjectType: SubjectProject, SubjectID: "project-1"},
		Source:        Source{Kind: "artifact", Ref: "artifact:run-1", EvidenceHash: "sha256:source"},
		SourceAgentID: "agent-1", ExtractorID: "extractor-v1", Status: ExtractionLeased,
		Attempts: 1, LeaseOwner: "worker-1", LeaseUntil: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
	}
}

func trustedProjectionTestValue() TrustedProjection {
	now := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	return TrustedProjection{EventID: "event-42", Create: CreateRequest{
		ID:    "b1111111-1111-4111-8111-111111111111",
		Scope: Scope{TenantID: "tenant-1", SubjectType: SubjectProject, SubjectID: "project-1"},
		Kind:  KindConstraint, Key: "runtime.limit", Status: StatusActive,
		Content: "Use bounded memory output", ValidFrom: now, Importance: 80, Confidence: 1,
		Actor: "trusted-projector", Reason: "approved event", Now: now,
	}}
}

type sourceReaderFunc func(context.Context, Source) (string, error)

func (f sourceReaderFunc) ReadSource(ctx context.Context, source Source) (string, error) {
	return f(ctx, source)
}

type contentValidatorStub struct {
	calls int
	err   error
}

func (v *contentValidatorStub) ValidateMemoryContent(ctx context.Context, _ string) error {
	v.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	return v.err
}

type extractionJobStoreStub struct {
	claimJob                   ExtractionJob
	claimErr, failErr          error
	claimHook                  func()
	claimCalls                 int
	completed                  bool
	failedCode                 string
	failedAt                   time.Time
	completeAttempt            int
	failAttempt                int
	completeID, completeWorker string
}

func (*extractionJobStoreStub) EnqueueExtraction(context.Context, ExtractionJob) error { return nil }
func (s *extractionJobStoreStub) ClaimExtraction(context.Context, string, time.Duration, int, time.Time) (ExtractionJob, error) {
	s.claimCalls++
	if s.claimHook != nil {
		s.claimHook()
	}
	return s.claimJob, s.claimErr
}
func (s *extractionJobStoreStub) CompleteExtraction(_ context.Context, id, worker string, expectedAttempt int, _ time.Time) error {
	s.completed, s.completeID, s.completeWorker, s.completeAttempt = true, id, worker, expectedAttempt
	return nil
}
func (s *extractionJobStoreStub) FailExtraction(_ context.Context, _, _ string, expectedAttempt int, code string, _ int, now time.Time) error {
	s.failedCode, s.failedAt, s.failAttempt = code, now, expectedAttempt
	return s.failErr
}

type extractionLifecycleStoreStub struct {
	creates      []CreateRequest
	createErr    error
	mutateResult func(*Memory)
}

func (s *extractionLifecycleStoreStub) Create(_ context.Context, request CreateRequest) (Memory, error) {
	s.creates = append(s.creates, request)
	if s.createErr != nil {
		return Memory{}, s.createErr
	}
	memory := Memory{
		ID: request.ID, Scope: request.Scope, Kind: request.Kind, Key: request.Key, Status: request.Status,
		Content: request.Content, ValidFrom: request.ValidFrom, ValidUntil: request.ValidUntil,
		Importance: request.Importance, Confidence: request.Confidence,
		SourceAgentID: request.SourceAgentID, CreatedBy: request.Actor,
		IdempotencyKey: request.IdempotencyKey, Version: 1,
		CreatedAt: request.Now, UpdatedAt: request.Now,
	}
	if s.mutateResult != nil {
		s.mutateResult(&memory)
	}
	return memory, nil
}
func (*extractionLifecycleStoreStub) Get(context.Context, Scope, string) (Memory, error) {
	return Memory{}, ErrNotFound
}
func (*extractionLifecycleStoreStub) Sources(context.Context, Scope, string) ([]Source, error) {
	return nil, nil
}
func (*extractionLifecycleStoreStub) List(context.Context, ListQuery) ([]Memory, error) {
	return nil, nil
}
func (*extractionLifecycleStoreStub) Activate(context.Context, VersionedCommand) (Memory, error) {
	return Memory{}, ErrNotFound
}
func (*extractionLifecycleStoreStub) Correct(context.Context, CorrectRequest) (Memory, error) {
	return Memory{}, ErrNotFound
}
func (*extractionLifecycleStoreStub) Dismiss(context.Context, VersionedCommand) (Memory, error) {
	return Memory{}, ErrNotFound
}
func (*extractionLifecycleStoreStub) Forget(context.Context, VersionedCommand) (Memory, error) {
	return Memory{}, ErrNotFound
}
func (*extractionLifecycleStoreStub) Erase(context.Context, VersionedCommand) error {
	return ErrNotFound
}
func (*extractionLifecycleStoreStub) Revisions(context.Context, RevisionQuery) ([]Revision, error) {
	return nil, nil
}

var _ ExtractionJobStore = (*extractionJobStoreStub)(nil)
var _ LifecycleStore = (*extractionLifecycleStoreStub)(nil)
var _ SourceReader = sourceReaderFunc(nil)
