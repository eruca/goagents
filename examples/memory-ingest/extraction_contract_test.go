package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/agentadapter"
	"github.com/eruca/goagents/memorykit/memorystore"
)

// Only the external LLM is replaced; ingestion, strict extraction, worker and
// memory lifecycle use their real implementations without credentials or network.
type offlineKnowledgeClient struct{ calls int }

func (c *offlineKnowledgeClient) Chat(_ context.Context, _ ports.ChatRequest) (*ports.ChatResponse, error) {
	c.calls++
	return &ports.ChatResponse{Content: `{"candidates":[{"kind":"fact","key":"source-claim","content":"The source reports a bounded claim."}]}`}, nil
}

func TestModelIngestionCarriesSourceIdentity(t *testing.T) {
	ctx := context.Background()
	reg, err := openRegistry(t.TempDir(), testScope())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.root.Close()
	input := t.TempDir()
	if err := os.WriteFile(filepath.Join(input, "source.txt"), []byte("A bounded source claim."), 0600); err != nil {
		t.Fatal(err)
	}
	jobs := &testJobs{jobs: map[string]memorykit.ExtractionJob{}}
	if _, err := reg.ingest(ctx, input, jobs, "model:v1"); err != nil {
		t.Fatal(err)
	}
	client := &offlineKnowledgeClient{}
	model, err := agentadapter.NewJSONCandidateExtractor(knowledgeClient{client}, limits, 8)
	if err != nil {
		t.Fatal(err)
	}
	memories, err := memorystore.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := memorykit.NewExtractionWorker(memorykit.ExtractionWorkerConfig{
		Jobs: jobs, Memories: memories, Reader: reg,
		Extractor:       cachedExtractor{reg, model, limits, "model:v1"},
		ValidateContent: validator{}, Limits: limits, WorkerID: "offline-test",
		LeaseDuration: time.Minute, MaxAttempts: 3, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("model ingestion must create a candidate: worked=%v err=%v", worked, err)
	}
	candidates, err := memories.List(ctx, memorykit.ListQuery{Scope: testScope(), Status: memorykit.StatusCandidate, Limit: 10})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%v err=%v", candidates, err)
	}
	for _, job := range jobs.jobs {
		if job.SourceAgentID != job.ID || candidates[0].SourceAgentID != job.ID {
			t.Fatal("source identity must identify the caller-owned ingestion job")
		}
	}
	if client.calls != 1 {
		t.Fatalf("model calls=%d", client.calls)
	}
	if worked, err := worker.RunOnce(ctx); err != nil || worked {
		t.Fatalf("completed job replay: %v %v", worked, err)
	}
}

func TestExtractorVersionMismatchFailsBeforeDelegateOrCache(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "cached"}[cached], func(t *testing.T) {
			reg, err := openRegistry(t.TempDir(), testScope())
			if err != nil {
				t.Fatal(err)
			}
			defer reg.root.Close()
			source, err := reg.register("old source", digest([]byte("document")), 0, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			req := memorykit.ExtractionRequest{Scope: testScope(), Source: source, SourceAgentID: "ingestion-job", ExtractorID: "model:old", Text: "old source", Now: time.Now()}
			if cached {
				old := cachedExtractor{reg, copyExtractor{}, limits, "model:old"}
				if _, err := old.Extract(context.Background(), req); err != nil {
					t.Fatal(err)
				}
			}
			for _, version := range []string{"model:new", "copy-smoke:old", ""} {
				delegate := &countingExtractor{}
				current := cachedExtractor{reg, delegate, limits, version}
				if _, err := current.Extract(context.Background(), req); err == nil {
					t.Errorf("extractor %q accepted a job bound to %q", version, req.ExtractorID)
				}
				if delegate.calls != 0 {
					t.Errorf("mismatched delegate called %d times", delegate.calls)
				}
			}
		})
	}
}
