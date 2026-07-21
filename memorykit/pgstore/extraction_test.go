package pgstore

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/google/uuid"
)

const extractionTenantID = "tenant-extraction"

func TestExtractionMigrationHasClaimableQueueIndex(t *testing.T) {
	ddl := strings.ToLower(strings.Join(migrationV1, "\n"))
	if !strings.Contains(ddl, "idx_memory_extraction_claimable") ||
		!strings.Contains(ddl, "where status in ('pending','leased')") {
		t.Fatal("migration does not contain the claimable extraction queue index")
	}
}

func TestExtractionJobEnqueueIsIdempotentByID(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("81111111-1111-4111-8111-111111111111")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	changed := job
	changed.Source.Ref = "artifact:must-not-overwrite"
	if err := store.EnqueueExtraction(context.Background(), changed); err != nil {
		t.Fatal(err)
	}

	var count int
	var sourceRef string
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT count(*), min(source_ref) FROM memory_extraction_jobs WHERE id = $1
	`, job.ID).Scan(&count, &sourceRef); err != nil {
		t.Fatal(err)
	}
	if count != 1 || sourceRef != job.Source.Ref {
		t.Fatalf("enqueued row count/source = %d/%q", count, sourceRef)
	}
}

func TestExtractionJobRejectsOversizedDerivedActorAndLeaseOverflow(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("81222222-2222-4222-8222-222222222222")
	job.ExtractorID = strings.Repeat("e", validConfig().Limits.MaxMetadataRunes-len("extractor:")+1)
	if err := store.EnqueueExtraction(context.Background(), job); !errors.Is(err, memorykit.ErrInvalidMemory) {
		t.Fatalf("oversized derived actor enqueue = %v, want ErrInvalidMemory", err)
	}

	overflowNow := time.Unix(int64(^uint64(0)>>1), 0)
	if _, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, 3, overflowNow); !errors.Is(err, memorykit.ErrInvalidMemory) {
		t.Fatalf("overflow claim = %v, want ErrInvalidMemory", err)
	}
	if _, err := store.ClaimExtraction(context.Background(), "worker-1", time.Nanosecond, 3, time.Now()); !errors.Is(err, memorykit.ErrInvalidMemory) {
		t.Fatalf("sub-microsecond claim = %v, want ErrInvalidMemory", err)
	}
	if _, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, int(math.MaxInt32)+1, time.Now()); !errors.Is(err, memorykit.ErrInvalidMemory) {
		t.Fatalf("oversized max-attempt claim = %v, want ErrInvalidMemory", err)
	}

	validJob := extractionJobFixture("81333333-3333-4333-8333-333333333333")
	if err := store.EnqueueExtraction(context.Background(), validJob); err != nil {
		t.Fatal(err)
	}
	leased, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, 3, validJob.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteExtraction(context.Background(), validJob.ID, "worker-1", 0, validJob.CreatedAt); !errors.Is(err, memorykit.ErrInvalidMemory) {
		t.Fatalf("zero attempt complete = %v, want ErrInvalidMemory", err)
	}
	if err := store.FailExtraction(context.Background(), validJob.ID, "worker-1", leased.Attempts, "source_read_failed", int(math.MaxInt32)+1, validJob.CreatedAt); !errors.Is(err, memorykit.ErrInvalidMemory) {
		t.Fatalf("oversized max-attempt fail = %v, want ErrInvalidMemory", err)
	}
}

func TestExtractionJobConcurrentClaimHasOneLeaseOwner(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("82222222-2222-4222-8222-222222222222")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	claimed := make([]memorykit.ExtractionJob, 2)
	errs := make([]error, 2)
	var workers sync.WaitGroup
	for index := range claimed {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			claimed[index], errs[index] = store.ClaimExtraction(
				context.Background(), "worker-"+string(rune('1'+index)), time.Minute, 3, job.CreatedAt)
		}(index)
	}
	close(start)
	workers.Wait()

	succeeded, empty := 0, 0
	for index, err := range errs {
		switch {
		case err == nil:
			succeeded++
			if claimed[index].ID != job.ID || claimed[index].Status != memorykit.ExtractionLeased ||
				claimed[index].Attempts != 1 || claimed[index].LeaseOwner == "" {
				t.Fatalf("claimed job = %#v", claimed[index])
			}
		case errors.Is(err, memorykit.ErrNoExtractionJob):
			empty++
		default:
			t.Fatalf("ClaimExtraction[%d] error = %v", index, err)
		}
	}
	if succeeded != 1 || empty != 1 {
		t.Fatalf("claim success/empty = %d/%d, want 1/1", succeeded, empty)
	}
}

func TestExtractionJobActiveLeaseIsSkippedAndExpiredLeaseIsReclaimed(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("83333333-3333-4333-8333-333333333333")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, 3, job.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExtraction(context.Background(), "worker-2", time.Minute, 3, job.CreatedAt.Add(30*time.Second)); !errors.Is(err, memorykit.ErrNoExtractionJob) {
		t.Fatalf("active lease claim error = %v, want ErrNoExtractionJob", err)
	}
	reclaimed, err := store.ClaimExtraction(context.Background(), "worker-2", time.Minute, 3, first.LeaseUntil)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.LeaseOwner != "worker-2" || reclaimed.Attempts != 2 || reclaimed.ID != job.ID {
		t.Fatalf("reclaimed job = %#v", reclaimed)
	}
}

func TestExtractionJobWrongOwnerCannotCompleteOrFail(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("84444444-4444-4444-8444-444444444444")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	leased, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, 3, job.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteExtraction(context.Background(), job.ID, "worker-2", leased.Attempts, job.CreatedAt.Add(time.Second)); !errors.Is(err, memorykit.ErrConflict) {
		t.Fatalf("wrong-owner CompleteExtraction() = %v, want ErrConflict", err)
	}
	if err := store.FailExtraction(context.Background(), job.ID, "worker-2", leased.Attempts, "source_read_failed", 3, job.CreatedAt.Add(time.Second)); !errors.Is(err, memorykit.ErrConflict) {
		t.Fatalf("wrong-owner FailExtraction() = %v, want ErrConflict", err)
	}
}

func TestExtractionJobLeaseFencingRejectsExpiredAndStaleAttemptForSameWorker(t *testing.T) {
	tests := []struct {
		name string
		id   string
		run  func(*Store, memorykit.ExtractionJob, int, time.Time) error
	}{
		{name: "complete", id: "84555555-5555-4555-8555-555555555555", run: func(store *Store, job memorykit.ExtractionJob, attempt int, now time.Time) error {
			return store.CompleteExtraction(context.Background(), job.ID, "worker-1", attempt, now)
		}},
		{name: "fail", id: "84666666-6666-4666-8666-666666666666", run: func(store *Store, job memorykit.ExtractionJob, attempt int, now time.Time) error {
			return store.FailExtraction(context.Background(), job.ID, "worker-1", attempt, "source_read_failed", 3, now)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openExtractionStore(t)
			job := extractionJobFixture(test.id)
			if err := store.EnqueueExtraction(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			first, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, 3, job.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.run(store, first, first.Attempts, first.LeaseUntil); !errors.Is(err, memorykit.ErrConflict) {
				t.Fatalf("expired attempt transition = %v, want ErrConflict", err)
			}
			second, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, 3, first.LeaseUntil)
			if err != nil {
				t.Fatal(err)
			}
			transitionNow := first.LeaseUntil.Add(time.Second)
			if err := test.run(store, second, first.Attempts, transitionNow); !errors.Is(err, memorykit.ErrConflict) {
				t.Fatalf("stale attempt transition = %v, want ErrConflict", err)
			}
			if err := test.run(store, second, second.Attempts, transitionNow); err != nil {
				t.Fatalf("current attempt transition = %v", err)
			}
		})
	}
}

func TestExtractionJobFailureRetriesThenScrubsTerminalSource(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("85555555-5555-4555-8555-555555555555")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, 2, job.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FailExtraction(context.Background(), job.ID, first.LeaseOwner, first.Attempts, "candidate_extract_failed", 2, job.CreatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	status, attempts, source, failure := extractionJobState(t, store, job.ID)
	if status != memorykit.ExtractionPending || attempts != 1 || source != job.Source || failure != "candidate_extract_failed" {
		t.Fatalf("retry state = %q/%d/%#v/%q", status, attempts, source, failure)
	}

	second, err := store.ClaimExtraction(context.Background(), "worker-2", time.Minute, 2, job.CreatedAt.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FailExtraction(context.Background(), job.ID, second.LeaseOwner, second.Attempts, "candidate_write_failed", 2, job.CreatedAt.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	status, attempts, source, failure = extractionJobState(t, store, job.ID)
	if status != memorykit.ExtractionFailed || attempts != 2 || source != (memorykit.Source{}) || failure != "candidate_write_failed" {
		t.Fatalf("terminal state = %q/%d/%#v/%q", status, attempts, source, failure)
	}
}

func TestExtractionJobExpiredFinalLeaseBecomesScrubbedTombstone(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("86666666-6666-4666-8666-666666666666")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	leased, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, 1, job.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExtraction(context.Background(), "worker-2", time.Minute, 1, leased.LeaseUntil); !errors.Is(err, memorykit.ErrNoExtractionJob) {
		t.Fatalf("exhausted ClaimExtraction() = %v, want ErrNoExtractionJob", err)
	}
	status, attempts, source, failure := extractionJobState(t, store, job.ID)
	if status != memorykit.ExtractionFailed || attempts != 1 || source != (memorykit.Source{}) || failure != "lease_exhausted" {
		t.Fatalf("expired terminal state = %q/%d/%#v/%q", status, attempts, source, failure)
	}
}

func TestExtractionJobCompletionKeepsScrubbedIdempotencyTombstone(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("87777777-7777-4777-8777-777777777777")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	leased, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, 3, job.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteExtraction(context.Background(), job.ID, leased.LeaseOwner, leased.Attempts, job.CreatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	status, attempts, source, failure := extractionJobState(t, store, job.ID)
	if status != memorykit.ExtractionCompleted || attempts != 1 || source != (memorykit.Source{}) || failure != "" {
		t.Fatalf("completion tombstone = %q/%d/%#v/%q", status, attempts, source, failure)
	}
	var sourceAgent, extractor string
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT source_agent_id, extractor_id FROM memory_extraction_jobs WHERE id = $1
	`, job.ID).Scan(&sourceAgent, &extractor); err != nil {
		t.Fatal(err)
	}
	if sourceAgent != "" || extractor != job.ExtractorID {
		t.Fatalf("completion source agent/extractor = %q/%q", sourceAgent, extractor)
	}
}

func TestExtractionJobRejectsRawFailureText(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("88888888-8888-4888-8888-888888888888")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	leased, err := store.ClaimExtraction(context.Background(), "worker-1", time.Minute, 3, job.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FailExtraction(context.Background(), job.ID, "worker-1", leased.Attempts, "provider said secret text", 3, job.CreatedAt.Add(time.Second)); !errors.Is(err, memorykit.ErrInvalidMemory) {
		t.Fatalf("raw FailExtraction() = %v, want ErrInvalidMemory", err)
	}
	status, _, _, failure := extractionJobState(t, store, job.ID)
	if status != memorykit.ExtractionLeased || failure != "" {
		t.Fatalf("raw failure mutated state = %q/%q", status, failure)
	}
}

func TestExtractionJobOperationsPreserveCancellation(t *testing.T) {
	store := openExtractionStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job := extractionJobFixture("89999999-9999-4999-8999-999999999999")
	checks := []struct {
		name string
		run  func() error
	}{
		{name: "enqueue", run: func() error { return store.EnqueueExtraction(ctx, job) }},
		{name: "claim", run: func() error {
			_, err := store.ClaimExtraction(ctx, "worker-1", time.Minute, 3, job.CreatedAt)
			return err
		}},
		{name: "complete", run: func() error { return store.CompleteExtraction(ctx, job.ID, "worker-1", 1, job.CreatedAt) }},
		{name: "fail", run: func() error {
			return store.FailExtraction(ctx, job.ID, "worker-1", 1, "source_read_failed", 3, job.CreatedAt)
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("operation error = %v, want context.Canceled", err)
			}
		})
	}
}

func TestExtractionWorkerWithPostgreSQLCreatesOnlyCandidate(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("8aaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	candidateID := uuid.NewSHA1(uuid.MustParse(job.ID), []byte("candidate:0")).String()
	worker, err := memorykit.NewExtractionWorker(memorykit.ExtractionWorkerConfig{
		Jobs: store, Memories: store,
		Reader: extractionSourceReaderFunc(func(context.Context, memorykit.Source) (string, error) {
			return "terminal output", nil
		}),
		Extractor: extractionCandidateExtractorFunc(func(context.Context, memorykit.ExtractionRequest) ([]memorykit.CandidateDraft, error) {
			return []memorykit.CandidateDraft{{
				Kind: memorykit.KindLesson, Key: "worker.lesson", Content: "Review model-derived output", Confidence: 1,
			}}, nil
		}),
		ValidateContent: extractionContentValidatorFunc(func(context.Context, string) error { return nil }),
		Limits:          validConfig().Limits, WorkerID: "worker-1", LeaseDuration: time.Minute, MaxAttempts: 3,
		Now: func() time.Time { return job.CreatedAt },
	})
	if err != nil {
		t.Fatal(err)
	}
	worked, err := worker.RunOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("RunOnce() = %v, %v", worked, err)
	}
	created, err := store.Get(context.Background(), job.Scope, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != memorykit.StatusCandidate {
		t.Fatalf("worker memory status = %q, want candidate", created.Status)
	}
}

func TestExtractionWorkerExpiredLeaseCannotUseOldTimeToFailPostgreSQL(t *testing.T) {
	store := openExtractionStore(t)
	job := extractionJobFixture("8bbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	clockCalls := 0
	worker, err := memorykit.NewExtractionWorker(memorykit.ExtractionWorkerConfig{
		Jobs: store, Memories: store,
		Reader: extractionSourceReaderFunc(func(context.Context, memorykit.Source) (string, error) {
			return "terminal output", nil
		}),
		Extractor: extractionCandidateExtractorFunc(func(context.Context, memorykit.ExtractionRequest) ([]memorykit.CandidateDraft, error) {
			return nil, nil
		}),
		ValidateContent: extractionContentValidatorFunc(func(context.Context, string) error { return nil }),
		Limits:          validConfig().Limits, WorkerID: "worker-1", LeaseDuration: time.Microsecond, MaxAttempts: 3,
		Now: func() time.Time {
			clockCalls++
			if clockCalls == 1 {
				return job.CreatedAt
			}
			return job.CreatedAt.Add(time.Microsecond)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	worked, err := worker.RunOnce(context.Background())
	if !worked || !errors.Is(err, memorykit.ErrConflict) {
		t.Fatalf("RunOnce() = %v, %v, want true ErrConflict", worked, err)
	}
	status, attempts, source, failure := extractionJobState(t, store, job.ID)
	if status != memorykit.ExtractionLeased || attempts != 1 || source != job.Source || failure != "" {
		t.Fatalf("expired worker state = %q/%d/%#v/%q, want unchanged leased attempt", status, attempts, source, failure)
	}
}

func TestExtractionWorkerRetryReusesCandidateWithoutDuplicateRevision(t *testing.T) {
	store := openExtractionStore(t)
	jobs := &failFirstCompletionStore{Store: store}
	job := extractionJobFixture("8ddddddd-dddd-4ddd-8ddd-dddddddddddd")
	if err := store.EnqueueExtraction(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	candidateID := uuid.NewSHA1(uuid.MustParse(job.ID), []byte("candidate:0")).String()
	newWorker := func(now time.Time) *memorykit.ExtractionWorker {
		t.Helper()
		worker, err := memorykit.NewExtractionWorker(memorykit.ExtractionWorkerConfig{
			Jobs: jobs, Memories: store,
			Reader: extractionSourceReaderFunc(func(context.Context, memorykit.Source) (string, error) {
				return "terminal output", nil
			}),
			Extractor: extractionCandidateExtractorFunc(func(context.Context, memorykit.ExtractionRequest) ([]memorykit.CandidateDraft, error) {
				return []memorykit.CandidateDraft{{
					Kind: memorykit.KindLesson, Key: "worker.retry", Content: "Use stable candidate identity", Confidence: .9,
				}}, nil
			}),
			ValidateContent: extractionContentValidatorFunc(func(context.Context, string) error { return nil }),
			Limits:          validConfig().Limits, WorkerID: "worker-1", LeaseDuration: time.Minute, MaxAttempts: 3,
			Now: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		return worker
	}
	worker := newWorker(job.CreatedAt)
	worked, firstErr := worker.RunOnce(context.Background())
	if !worked || !errors.Is(firstErr, errInjectedCompletion) {
		t.Fatalf("first RunOnce() = %v, %v, want injected completion error", worked, firstErr)
	}
	worker = newWorker(job.CreatedAt.Add(time.Second))
	worked, err := worker.RunOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("second RunOnce() = %v, %v", worked, err)
	}

	var memoryCount int
	if err := store.db.QueryRowContext(context.Background(),
		"SELECT count(*) FROM memories WHERE id = $1", candidateID).Scan(&memoryCount); err != nil {
		t.Fatal(err)
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{
		Scope: job.Scope, MemoryID: candidateID, Limit: 10,
	})
	if err != nil || memoryCount != 1 || len(revisions) != 1 {
		t.Fatalf("retry memory/revisions = %d/%d, %v", memoryCount, len(revisions), err)
	}
	status, attempts, source, _ := extractionJobState(t, store, job.ID)
	if status != memorykit.ExtractionCompleted || attempts != 2 || source != (memorykit.Source{}) {
		t.Fatalf("retry job state = %q/%d/%#v", status, attempts, source)
	}
}

func TestProjectTrustedPostgreSQLReplayHasOneRevision(t *testing.T) {
	store := openExtractionStore(t)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	projection := memorykit.TrustedProjection{EventID: "approved-event-1", Create: memorykit.CreateRequest{
		ID: "8ccccccc-cccc-4ccc-8ccc-cccccccccccc", Scope: extractionScope(),
		Kind: memorykit.KindConstraint, Key: "trusted.constraint", Status: memorykit.StatusActive,
		Content: "Use the approved deployment gate", ValidFrom: now, Importance: 90, Confidence: 1,
		Actor: "trusted-projector", Reason: "approved event", Now: now,
	}}
	validator := extractionContentValidatorFunc(func(context.Context, string) error { return nil })
	first, err := memorykit.ProjectTrusted(context.Background(), store, validator, validConfig().Limits, projection)
	if err != nil {
		t.Fatal(err)
	}
	projection.Create.Now = projection.Create.Now.Add(time.Hour)
	second, err := memorykit.ProjectTrusted(context.Background(), store, validator, validConfig().Limits, projection)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("trusted replay differs: %#v / %#v", first, second)
	}
	revisions, err := store.Revisions(context.Background(), memorykit.RevisionQuery{
		Scope: projection.Create.Scope, MemoryID: projection.Create.ID, Limit: 10,
	})
	if err != nil || len(revisions) != 1 {
		t.Fatalf("trusted replay revisions = %#v, %v", revisions, err)
	}
}

func TestProjectTrustedRejectsReplayAfterForget(t *testing.T) {
	store := openExtractionStore(t)
	now := time.Date(2026, 7, 21, 10, 30, 0, 0, time.UTC)
	projection := memorykit.TrustedProjection{EventID: "approved-event-forget", Create: memorykit.CreateRequest{
		ID: "8fffffff-ffff-4fff-8fff-ffffffffffff", Scope: extractionScope(),
		Kind: memorykit.KindConstraint, Key: "trusted.forgotten", Status: memorykit.StatusActive,
		Content: "Do not revive forgotten trusted memory", ValidFrom: now, Importance: 90, Confidence: 1,
		Actor: "trusted-projector", Reason: "approved event", Now: now,
	}}
	validator := extractionContentValidatorFunc(func(context.Context, string) error { return nil })
	created, err := memorykit.ProjectTrusted(context.Background(), store, validator, validConfig().Limits, projection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Forget(context.Background(), memorykit.VersionedCommand{
		Scope: created.Scope, ID: created.ID, ExpectedVersion: created.Version,
		Actor: "reviewer", Reason: "forgotten", Now: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := memorykit.ProjectTrusted(context.Background(), store, validator, validConfig().Limits, projection); !errors.Is(err, memorykit.ErrInvalidRecallResult) {
		t.Fatalf("forgotten ProjectTrusted() = %v, want ErrInvalidRecallResult", err)
	}
}

func extractionJobFixture(id string) memorykit.ExtractionJob {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	return memorykit.ExtractionJob{
		ID: id, Scope: extractionScope(),
		Source:        memorykit.Source{Kind: "artifact", Ref: "artifact:" + id, EvidenceHash: "sha256:" + id},
		SourceAgentID: "agent-1", ExtractorID: "extractor-v1", Status: memorykit.ExtractionPending,
		CreatedAt: now, UpdatedAt: now,
	}
}

func extractionScope() memorykit.Scope {
	return memorykit.Scope{TenantID: extractionTenantID, SubjectType: memorykit.SubjectProject, SubjectID: "project-1"}
}

func openExtractionStore(t *testing.T) *Store {
	t.Helper()
	store := openTestStore(t)
	cleanupExtractionTenant(t, store)
	t.Cleanup(func() { cleanupExtractionTenant(t, store) })
	return store
}

func cleanupExtractionTenant(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(),
		"DELETE FROM memory_extraction_jobs WHERE tenant_id = $1", extractionTenantID); err != nil {
		t.Fatalf("cleanup extraction jobs: %v", err)
	}
	if _, err := store.db.ExecContext(context.Background(),
		"DELETE FROM memories WHERE tenant_id = $1", extractionTenantID); err != nil {
		t.Fatalf("cleanup extraction memories: %v", err)
	}
}

func extractionJobState(t *testing.T, store *Store, id string) (memorykit.ExtractionJobStatus, int, memorykit.Source, string) {
	t.Helper()
	var status memorykit.ExtractionJobStatus
	var attempts int
	var source memorykit.Source
	var failure string
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT status, attempts, source_kind, source_ref, evidence_hash, failure_code
		FROM memory_extraction_jobs WHERE id = $1
	`, id).Scan(&status, &attempts, &source.Kind, &source.Ref, &source.EvidenceHash, &failure); err != nil {
		t.Fatal(err)
	}
	return status, attempts, source, failure
}

type extractionSourceReaderFunc func(context.Context, memorykit.Source) (string, error)

func (f extractionSourceReaderFunc) ReadSource(ctx context.Context, source memorykit.Source) (string, error) {
	return f(ctx, source)
}

type extractionCandidateExtractorFunc func(context.Context, memorykit.ExtractionRequest) ([]memorykit.CandidateDraft, error)

func (f extractionCandidateExtractorFunc) Extract(ctx context.Context, request memorykit.ExtractionRequest) ([]memorykit.CandidateDraft, error) {
	return f(ctx, request)
}

type extractionContentValidatorFunc func(context.Context, string) error

func (f extractionContentValidatorFunc) ValidateMemoryContent(ctx context.Context, content string) error {
	return f(ctx, content)
}

var errInjectedCompletion = errors.New("injected completion failure")

type failFirstCompletionStore struct {
	*Store
	completeCalls int
}

func (s *failFirstCompletionStore) CompleteExtraction(ctx context.Context, id, workerID string, expectedAttempt int, now time.Time) error {
	s.completeCalls++
	if s.completeCalls == 1 {
		return errInjectedCompletion
	}
	return s.Store.CompleteExtraction(ctx, id, workerID, expectedAttempt, now)
}
