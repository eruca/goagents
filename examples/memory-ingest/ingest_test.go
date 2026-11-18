package main

import (
	"context"
	"errors"
	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/memorystore"
	"github.com/eruca/goagents/memorykit/pgstore"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testScope() memorykit.Scope {
	return memorykit.Scope{TenantID: "ingest-test", SubjectType: memorykit.SubjectProject, SubjectID: "private-knowledge"}
}
func TestFileLifecycle(t *testing.T) {
	store, err := memorystore.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	exerciseLifecycle(t, store, &testJobs{jobs: map[string]memorykit.ExtractionJob{}})
}
func TestPostgresFileLifecycle(t *testing.T) {
	dsn := os.Getenv("MEMORYKIT_POSTGRES_TEST_DSN")
	if dsn == "" {
		if os.Getenv("MEMORYKIT_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL DSN required")
		}
		t.Skip("PostgreSQL DSN not configured")
	}
	store, err := pgstore.Open(context.Background(), dsn, pgstore.Config{Limits: limits, EmbeddingProfileID: "fixture-3d", EmbeddingDimensions: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := testScope()
	scope.SubjectID = t.Name() + time.Now().Format("150405.000000000")
	exerciseLifecycleScope(t, store, store, scope)
}
func exerciseLifecycle(t *testing.T, store memorykit.Store, jobs memorykit.ExtractionJobStore) {
	exerciseLifecycleScope(t, store, jobs, testScope())
}
func exerciseLifecycleScope(t *testing.T, store memorykit.Store, jobs memorykit.ExtractionJobStore, scope memorykit.Scope) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	text := "RCA evidence: citrate monitoring is a source claim, not a personal decision."
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	reg, err := openRegistry(t.TempDir(), scope)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.root.Close()
	worker, err := memorykit.NewExtractionWorker(memorykit.ExtractionWorkerConfig{Jobs: jobs, Memories: store, Reader: reg, Extractor: cachedExtractor{reg, copyExtractor{}, limits, "copy-smoke:v1"}, ValidateContent: validator{}, Limits: limits, WorkerID: "test-worker", LeaseDuration: time.Minute, MaxAttempts: 3, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	ingest := func() {
		t.Helper()
		if _, err := reg.ingest(ctx, dir, jobs, "copy-smoke:v1"); err != nil {
			t.Fatal(err)
		}
		for {
			worked, err := worker.RunOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !worked {
				break
			}
		}
	}
	ingest()
	ingest()
	candidates, err := store.List(ctx, memorykit.ListQuery{Scope: scope, Status: memorykit.StatusCandidate, Limit: 100})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("reimport candidates=%d error=%v", len(candidates), err)
	}
	m := candidates[0]
	query := memorykit.CandidateQuery{Scope: scope, Keys: []string{m.Key}, Text: "citrate", Now: time.Now(), ExactLimit: 5, FullTextLimit: 5}
	result, err := store.SearchCandidates(ctx, query)
	if err != nil || len(result.Exact)+len(result.FullText) != 0 {
		t.Fatal("candidate leaked into recall", err)
	}
	active, err := store.Activate(ctx, memorykit.VersionedCommand{Scope: scope, ID: m.ID, ExpectedVersion: m.Version, Actor: "reviewer", Reason: "checked source", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Activate(ctx, memorykit.VersionedCommand{Scope: scope, ID: m.ID, ExpectedVersion: m.Version, Actor: "reviewer", Now: time.Now()}); !errors.Is(err, memorykit.ErrConflict) {
		t.Fatal("stale activation accepted", err)
	}
	embeddingWorker, err := memorykit.NewEmbeddingWorker(memorykit.EmbeddingWorkerConfig{Store: store, Embedder: fixtureEmbedder{}, ProfileID: "fixture-3d", Dimensions: 3, BatchSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = embeddingWorker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	recaller, err := memorykit.NewRecaller(memorykit.RecallConfig{Store: store, Embedder: fixtureEmbedder{}, CountTokens: func(s string) int { return len([]rune(s)) }, Policy: memorykit.RecallPolicy{Version: "test", ExactLimit: 5, FullTextLimit: 5, VectorLimit: 5, RRFK: 60, MaxItems: 5, MaxTokens: 4096, MaxQueryRunes: 1000, MaxKeys: 5, Deadline: time.Second, EmbeddingProfileID: "fixture-3d", EmbeddingDimensions: 3}})
	if err != nil {
		t.Fatal(err)
	}
	recalled, err := recaller.Recall(ctx, memorykit.RecallRequest{Scope: scope, Text: "citrate", Keys: []string{m.Key}, Now: time.Now()})
	if err != nil || len(recalled.Items) != 1 || len(recalled.Items[0].Sources) != 1 {
		t.Fatal("active recall/source failed", err)
	}
	other := scope
	other.SubjectID += "other"
	foreign, err := recaller.Recall(ctx, memorykit.RecallRequest{Scope: other, Text: "citrate", Keys: []string{m.Key}, Now: time.Now()})
	if err != nil || len(foreign.Items) != 0 {
		t.Fatal("cross-project leak", err)
	}
	if _, err = store.Forget(ctx, memorykit.VersionedCommand{Scope: scope, ID: m.ID, ExpectedVersion: active.Version, Actor: "reviewer", Reason: "withdrawn", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	recalled, err = recaller.Recall(ctx, memorykit.RecallRequest{Scope: scope, Text: "citrate", Keys: []string{m.Key}, Now: time.Now()})
	if err != nil || len(recalled.Items) != 0 {
		t.Fatal("forgotten memory leaked", err)
	}
	ingest()
	candidates, err = store.List(ctx, memorykit.ListQuery{Scope: scope, Status: memorykit.StatusCandidate, Limit: 100})
	if err != nil || len(candidates) != 0 {
		t.Fatal("reimport resurrected reviewed memory", err)
	}
}

type fixtureEmbedder struct{}

func (fixtureEmbedder) Embed(_ context.Context, r memorykit.EmbedRequest) ([][]float32, error) {
	v := make([][]float32, len(r.Texts))
	for i := range v {
		v[i] = []float32{1, 0, 0}
	}
	return v, nil
}
func TestRegistryRejectsTamperingAndScope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	reg, err := openRegistry(dir, testScope())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.root.Close()
	source, err := reg.register("evidence", strings.Repeat("a", 64), 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wrong := source
	wrong.EvidenceHash = "wrong"
	if _, err = reg.ReadSource(ctx, wrong); err == nil {
		t.Fatal("hash accepted")
	}
	wrong = source
	wrong.Ref = "../../secret"
	if _, err = reg.ReadSource(ctx, wrong); err == nil {
		t.Fatal("path accepted")
	}
	other := testScope()
	other.SubjectID = "other"
	foreign, err := openRegistry(dir, other)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.root.Close()
	if _, err = foreign.ReadSource(ctx, source); err == nil {
		t.Fatal("foreign source accepted")
	}
	name, _ := reg.filename(source.Ref)
	if err = os.WriteFile(filepath.Join(dir, name), []byte(`{"Text":"tampered"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = reg.ReadSource(ctx, source); err == nil {
		t.Fatal("tampered snapshot accepted")
	}
	if _, err = reg.register("evidence", strings.Repeat("a", 64), 0, time.Now()); err == nil {
		t.Fatal("corrupt snapshot overwritten")
	}
}
func TestInputRejectsSymlinkAndChunksUTF8(t *testing.T) {
	input := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("secret"), 0600)
	if err := os.Symlink(secret, filepath.Join(input, "link.txt")); err != nil {
		t.Fatal(err)
	}
	reg, _ := openRegistry(t.TempDir(), testScope())
	defer reg.root.Close()
	if _, err := reg.ingest(context.Background(), input, &testJobs{jobs: map[string]memorykit.ExtractionJob{}}, "v1"); err == nil {
		t.Fatal("input symlink accepted")
	}
	chunks := splitText(strings.Repeat("中文", 2000))
	for _, c := range chunks {
		if len([]rune(c)) > chunkRunes {
			t.Fatal("chunk too long")
		}
	}
}

// Minimal serial job fixture; PostgreSQL test exercises the real leased store.
type testJobs struct {
	jobs map[string]memorykit.ExtractionJob
}

func (j *testJobs) EnqueueExtraction(_ context.Context, job memorykit.ExtractionJob) error {
	if _, ok := j.jobs[job.ID]; !ok {
		j.jobs[job.ID] = job
	}
	return nil
}
func (j *testJobs) ClaimExtraction(_ context.Context, owner string, lease time.Duration, max int, now time.Time) (memorykit.ExtractionJob, error) {
	for id, job := range j.jobs {
		if job.Status == memorykit.ExtractionPending {
			job.Status = memorykit.ExtractionLeased
			job.Attempts++
			job.LeaseOwner = owner
			job.LeaseUntil = now.Add(lease)
			job.UpdatedAt = now
			j.jobs[id] = job
			return job, nil
		}
	}
	return memorykit.ExtractionJob{}, memorykit.ErrNoExtractionJob
}
func (j *testJobs) CompleteExtraction(_ context.Context, id, owner string, attempt int, now time.Time) error {
	job := j.jobs[id]
	job.Status = memorykit.ExtractionCompleted
	j.jobs[id] = job
	return nil
}
func (j *testJobs) FailExtraction(_ context.Context, id, owner string, attempt int, code string, max int, now time.Time) error {
	job := j.jobs[id]
	job.Status = memorykit.ExtractionFailed
	j.jobs[id] = job
	return nil
}

type countingExtractor struct{ calls int }

func (e *countingExtractor) Extract(_ context.Context, r memorykit.ExtractionRequest) ([]memorykit.CandidateDraft, error) {
	e.calls++
	return []memorykit.CandidateDraft{{Kind: memorykit.KindFact, Key: "model-key", Content: r.Text, ValidFrom: r.Now}}, nil
}
func TestCachedExtractionFreezesDrafts(t *testing.T) {
	reg, err := openRegistry(t.TempDir(), testScope())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.root.Close()
	source, err := reg.register("source", strings.Repeat("b", 64), 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	delegate := &countingExtractor{}
	extractor := cachedExtractor{reg, delegate, limits, "model:v1"}
	req := memorykit.ExtractionRequest{Scope: testScope(), Source: source, ExtractorID: "model:v1", Text: "source", Now: time.Now()}
	first, err := extractor.Extract(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Now = time.Now().Add(time.Hour)
	second, err := extractor.Extract(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if delegate.calls != 1 || first[0].Key != second[0].Key || !first[0].ValidFrom.Equal(second[0].ValidFrom) {
		t.Fatal("retry changed candidate identity/content timestamp")
	}
	req.Scope.SubjectID = "foreign"
	if _, err = extractor.Extract(context.Background(), req); err == nil {
		t.Fatal("foreign extraction accepted")
	}
}
