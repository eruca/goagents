// memory-ingest is a local, caller-owned private-project CLI, not a network service.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/agentadapter"
	"github.com/eruca/goagents/memorykit/pgstore"
)

var limits = memorykit.Limits{Version: "file-ingest-v1", MaxKeyRunes: 160, MaxContentRunes: 2400, MaxMetadataRunes: 256, MaxSourcesPerMemory: 8, MaxListItems: 100}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "memory-ingest failed:", err)
		os.Exit(1)
	}
}
func run() error {
	command := flag.String("command", "", "ingest, list, activate, recall, or benchmark")
	registryDir := flag.String("registry", "", "private source registry directory")
	input := flag.String("input", "", "directory of complete UTF-8 .txt files")
	id := flag.String("id", "", "reviewed candidate UUID")
	version := flag.Int64("version", 0, "reviewed candidate version")
	approve := flag.Bool("approve", false, "explicitly approve this candidate only")
	reason := flag.String("reason", "", "review reason")
	query := flag.String("query", "", "recall query")
	mode := flag.String("extractor", "", "copy-smoke or model")
	extractorVersion := flag.String("extractor-version", "", "immutable extractor/model/prompt version")
	cases := flag.String("cases", "", "benchmark case JSON path")
	vector := flag.Bool("vector", false, "enable real embeddings")
	dimensions := flag.Int("dimensions", 0, "embedding profile dimensions")
	profile := flag.String("profile", "", "explicit embedding profile ID")
	flag.Parse()
	if *command != "ingest" && *command != "list" && *command != "activate" && *command != "recall" && *command != "benchmark" {
		return errors.New("choose a supported command")
	}
	// These are trusted local operator configuration, never document/model fields.
	scope := memorykit.Scope{TenantID: os.Getenv("MEMORY_TENANT_ID"), SubjectType: memorykit.SubjectProject, SubjectID: os.Getenv("MEMORY_PROJECT_ID")}
	actor := os.Getenv("MEMORY_ACTOR")
	if scope.Validate() != nil || strings.TrimSpace(actor) == "" || *registryDir == "" || *profile == "" || *dimensions <= 0 {
		return errors.New("explicit tenant, project, actor, registry and embedding contract required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	reg, err := openRegistry(*registryDir, scope)
	if err != nil {
		return err
	}
	defer reg.root.Close()
	lock, err := reg.root.OpenFile("operator.lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("registry locked; stop the other process before recovery")
	}
	lock.Close()
	defer reg.root.Remove("operator.lock")
	dsn := os.Getenv("MEMORYKIT_POSTGRES_TEST_DSN")
	// Global workers require an exclusive database: do not use a shared Host DB.
	if os.Getenv("MEMORY_INGEST_EXCLUSIVE_DB") != "1" {
		return errors.New("exclusive private-project database acknowledgement required")
	}
	store, err := pgstore.Open(ctx, dsn, pgstore.Config{Limits: limits, EmbeddingProfileID: *profile, EmbeddingDimensions: *dimensions})
	if err != nil {
		return errors.New("PostgreSQL initialization failed")
	}
	defer store.Close()
	if err = checkExclusiveScope(ctx, dsn, scope); err != nil {
		return err
	}
	switch *command {
	case "ingest":
		if *input == "" || *extractorVersion == "" {
			return errors.New("input and immutable extractor version required")
		}
		var extractor memorykit.CandidateExtractor
		switch *mode {
		case "copy-smoke":
			extractor = copyExtractor{}
		case "model":
			client, err := openaiapi.New(openaiapi.Config{BaseURL: os.Getenv("OPENAI_COMPAT_BASE_URL"), APIKey: os.Getenv("OPENAI_COMPAT_API_KEY"), Model: os.Getenv("OPENAI_COMPAT_MODEL"), Limits: openaiapi.Limits{MaxOutputTokens: 4096, MaxRequestBytes: 64 << 10, MaxResponseBytes: 64 << 10}})
			if err != nil {
				return errors.New("model configuration invalid")
			}
			extractor, err = agentadapter.NewJSONCandidateExtractor(knowledgeClient{client}, limits, 8)
			if err != nil {
				return errors.New("extractor configuration invalid")
			}
		default:
			return errors.New("explicit extractor mode required")
		}
		extractorID := *mode + ":" + *extractorVersion
		if len(extractorID) > 200 {
			return errors.New("extractor version too long")
		}
		count, err := reg.ingest(ctx, *input, store, extractorID)
		if err != nil {
			return errors.New("text import failed")
		}
		worker, err := memorykit.NewExtractionWorker(memorykit.ExtractionWorkerConfig{Jobs: store, Memories: store, Reader: reg, Extractor: cachedExtractor{reg, extractor, limits, extractorID}, ValidateContent: validator{}, Limits: limits, WorkerID: "file-ingest", LeaseDuration: 90 * time.Second, MaxAttempts: 3, Now: time.Now})
		if err != nil {
			return errors.New("worker configuration failed")
		}
		for {
			worked, err := worker.RunOnce(ctx)
			if err != nil {
				return errors.New("extraction failed; no automatic activation")
			}
			if !worked {
				break
			}
		}
		fmt.Printf("submitted chunks: %d (reimports reuse job identities); candidates require review\n", count)
	case "list":
		memories, err := store.List(ctx, memorykit.ListQuery{Scope: scope, Status: memorykit.StatusCandidate, Limit: 100})
		if err != nil {
			return errors.New("candidate listing failed")
		}
		// Explicit review output may contain content; it is not a diagnostic log.
		for _, m := range memories {
			sources, err := store.Sources(ctx, scope, m.ID)
			if err != nil {
				return errors.New("source listing failed")
			}
			if err = emit(map[string]any{"memory": m, "sources": sources}); err != nil {
				return err
			}
		}
	case "activate":
		if !*approve || *reason == "" || *version <= 0 {
			return errors.New("approval, reviewed version and reason required")
		}
		m, err := store.Get(ctx, scope, *id)
		if err != nil {
			return errors.New("candidate unavailable")
		}
		if m.Status != memorykit.StatusCandidate || m.Version != *version {
			return errors.New("candidate state/version changed; review again")
		}
		if err = (validator{}).ValidateMemoryContent(ctx, m.Content); err != nil {
			return err
		}
		sources, err := store.Sources(ctx, scope, m.ID)
		if err != nil || len(sources) == 0 {
			return errors.New("candidate evidence unavailable")
		}
		for _, source := range sources {
			if _, err = reg.ReadSource(ctx, source); err != nil {
				return errors.New("candidate evidence invalid")
			}
		}
		m, err = store.Activate(ctx, memorykit.VersionedCommand{Scope: scope, ID: m.ID, ExpectedVersion: *version, Actor: actor, Reason: *reason, Now: time.Now().UTC()})
		if err != nil {
			return errors.New("activation failed")
		}
		return emit(map[string]any{"id": m.ID, "version": m.Version, "status": m.Status})
	case "recall", "benchmark":
		var embedder memorykit.Embedder
		if *vector {
			embedder, err = newHTTPEmbedder(*profile, *dimensions)
			if err != nil {
				return err
			}
			worker, err := memorykit.NewEmbeddingWorker(memorykit.EmbeddingWorkerConfig{Store: store, Embedder: embedder, ProfileID: *profile, Dimensions: *dimensions, BatchSize: 20})
			if err != nil {
				return errors.New("embedding configuration failed")
			}
			for {
				n, err := worker.RunOnce(ctx)
				if err != nil {
					return errors.New("embedding rebuild failed")
				}
				if n == 0 {
					break
				}
			}
		}
		vectorLimit := 0
		if *vector {
			vectorLimit = 20
		}
		recaller, err := memorykit.NewRecaller(memorykit.RecallConfig{Store: store, Embedder: embedder, CountTokens: func(s string) int { return utf8.RuneCountInString(s) }, Policy: memorykit.RecallPolicy{Version: "file-recall-v1", ExactLimit: 10, FullTextLimit: 20, VectorLimit: vectorLimit, RRFK: 60, MinVectorSimilarity: 0, MaxItems: 5, MaxTokens: 12000, MaxQueryRunes: 1000, MaxKeys: 10, Deadline: 15 * time.Second, EmbeddingProfileID: *profile, EmbeddingDimensions: *dimensions}})
		if err != nil {
			return errors.New("recall configuration failed")
		}
		if *command == "benchmark" {
			return benchmark(ctx, recaller, scope, *cases)
		}
		result, err := recaller.Recall(ctx, memorykit.RecallRequest{Scope: scope, Text: *query, Now: time.Now().UTC()})
		if err != nil {
			return errors.New("recall failed")
		}
		return emit(result)
	}
	return nil
}
func checkExclusiveScope(ctx context.Context, dsn string, scope memorykit.Scope) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errors.New("database scope check failed")
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM memories WHERE tenant_id <> $1 OR subject_type <> 'project' OR subject_id <> $2) + (SELECT count(*) FROM memory_extraction_jobs WHERE tenant_id <> $1 OR subject_type <> 'project' OR subject_id <> $2)`, scope.TenantID, scope.SubjectID).Scan(&n)
	if err != nil || n != 0 {
		return errors.New("database is not exclusive to configured project")
	}
	return nil
}
func emit(v any) error {
	if json.NewEncoder(os.Stdout).Encode(v) != nil {
		return errors.New("output failed")
	}
	return nil
}
