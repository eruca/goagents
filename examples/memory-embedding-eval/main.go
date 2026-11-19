// Command memory-embedding-eval runs the memorykit retrieval corpus through
// the production recall path with a real embedding model served by Ollama.
//
// It seeds the corpus into a fresh PostgreSQL schema through pgstore, embeds
// every active memory with memorykit.EmbeddingWorker, and recalls each case
// with memorykit.Recaller under several channel combinations and vector
// similarity thresholds. The JSON report records the corpus hash, model,
// dimensions, per-policy and per-category metrics, misses and injections,
// per-case vector similarities, and latency. It carries memory and corpus IDs
// only, never memory or query text.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/pgstore"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const evaluatorVersion = "memory-embedding-eval/v1"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	dsn        string
	ollama     string
	model      string
	corpusPath string
	out        string
	thresholds []float64
	maxItems   int
	keepSchema bool
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "memory-embedding-eval:", err)
		return 2
	}
	report, err := evaluate(ctx, opts, http.DefaultClient)
	if err != nil {
		fmt.Fprintln(stderr, "memory-embedding-eval:", err)
		return 1
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "memory-embedding-eval:", err)
		return 1
	}
	encoded = append(encoded, '\n')
	if opts.out != "" {
		if err := os.WriteFile(opts.out, encoded, 0o644); err != nil {
			fmt.Fprintln(stderr, "memory-embedding-eval:", err)
			return 1
		}
	}
	printSummary(stdout, report)
	return 0
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	var opts options
	var thresholds string
	flags := flag.NewFlagSet("memory-embedding-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.dsn, "dsn", os.Getenv("MEMORYKIT_POSTGRES_TEST_DSN"), "PostgreSQL DSN with pgvector (default $MEMORYKIT_POSTGRES_TEST_DSN)")
	flags.StringVar(&opts.ollama, "ollama", "http://127.0.0.1:11434", "Ollama base URL")
	flags.StringVar(&opts.model, "model", "bge-m3", "Ollama embedding model")
	flags.StringVar(&opts.corpusPath, "corpus", "memorykit/testdata/recall_cases.json", "evaluation corpus")
	flags.StringVar(&opts.out, "out", "", "write the JSON report to this file")
	flags.StringVar(&thresholds, "min-similarity", "0,0.4,0.5,0.55,0.6,0.65,0.7", "comma-separated vector similarity thresholds to sweep")
	flags.IntVar(&opts.maxItems, "max-items", 4, "recall item budget per case (the lexical evaluation uses 4)")
	flags.BoolVar(&opts.keepSchema, "keep-schema", false, "keep the evaluation schema instead of dropping it")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected arguments %q", flags.Args())
	}
	if opts.dsn == "" {
		return options{}, errors.New("--dsn or MEMORYKIT_POSTGRES_TEST_DSN is required")
	}
	if opts.maxItems <= 0 {
		return options{}, errors.New("--max-items must be positive")
	}
	for _, raw := range strings.Split(thresholds, ",") {
		value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || value < 0 || value > 1 {
			return options{}, fmt.Errorf("invalid --min-similarity value %q", raw)
		}
		opts.thresholds = append(opts.thresholds, value)
	}
	return opts, nil
}

// Report is the versioned evaluation result.
type Report struct {
	EvaluatorVersion string                  `json:"evaluator_version"`
	GeneratedAt      time.Time               `json:"generated_at"`
	Corpus           CorpusInfo              `json:"corpus"`
	Embedding        EmbeddingInfo           `json:"embedding"`
	MaxItems         int                     `json:"max_items"`
	Policies         []PolicyReport          `json:"policies"`
	Similarities     []CaseSimilarity        `json:"case_similarities"`
	Latency          map[string]LatencyStats `json:"latency_ms"`
}

type CorpusInfo struct {
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
	Memories int    `json:"memories"`
	Cases    int    `json:"cases"`
}

type EmbeddingInfo struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	ProfileID  string `json:"profile_id"`
	Dimensions int    `json:"dimensions"`
	Embedded   int    `json:"embedded_memories"`
}

type PolicyReport struct {
	Name                string                `json:"name"`
	MinVectorSimilarity *float64              `json:"min_vector_similarity,omitempty"`
	Overall             memorykit.EvalReport  `json:"overall"`
	Categories          map[string]CategoryRe `json:"categories"`
	Misses              []string              `json:"misses,omitempty"`
	Injections          []string              `json:"injections,omitempty"`
	DegradedCases       []string              `json:"degraded_cases,omitempty"`
}

type CategoryRe struct {
	ExpectedFound     int `json:"expected_found"`
	ExpectedTotal     int `json:"expected_total"`
	ForbiddenInjected int `json:"forbidden_injected"`
	ForbiddenTotal    int `json:"forbidden_total"`
}

// CaseSimilarity lists, for one case, the cosine similarity between the query
// and each in-scope expected or forbidden memory, and the best other memory.
type CaseSimilarity struct {
	CaseID    string             `json:"case_id"`
	Category  string             `json:"category"`
	Expected  map[string]float64 `json:"expected,omitempty"`
	Forbidden map[string]float64 `json:"forbidden_in_scope,omitempty"`
	TopOther  string             `json:"top_other,omitempty"`
	TopOtherS float64            `json:"top_other_similarity,omitempty"`
}

type LatencyStats struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	Max   float64 `json:"max"`
}

func evaluate(ctx context.Context, opts options, client *http.Client) (*Report, error) {
	raw, err := os.ReadFile(opts.corpusPath)
	if err != nil {
		return nil, fmt.Errorf("read corpus: %w", err)
	}
	var corpus memorykit.EvalCorpus
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&corpus); err != nil {
		return nil, fmt.Errorf("decode corpus: %w", err)
	}
	if err := memorykit.ValidateEvalCorpus(corpus); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)

	embedder := &ollamaEmbedder{base: strings.TrimRight(opts.ollama, "/"), model: opts.model, client: client}
	probe, err := embedder.embedTexts(ctx, []string{"dimension probe"})
	if err != nil {
		return nil, fmt.Errorf("probe embedding model: %w", err)
	}
	dimensions := len(probe[0])
	profileID := "ollama-" + opts.model + "-" + strconv.Itoa(dimensions)
	embedder.profileID = profileID
	embedder.latencies = nil

	store, cleanup, err := openIsolatedStore(ctx, opts.dsn, profileID, dimensions, opts.keepSchema)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	corpusIDByStoreID, err := seed(ctx, store, corpus)
	if err != nil {
		return nil, err
	}
	embedded, err := embedAll(ctx, store, embedder, profileID, dimensions)
	if err != nil {
		return nil, err
	}
	embedder.latencies = nil

	report := &Report{
		EvaluatorVersion: evaluatorVersion,
		GeneratedAt:      time.Now().UTC(),
		Corpus:           CorpusInfo{Version: corpus.Version, SHA256: hex.EncodeToString(sum[:]), Memories: len(corpus.Memories), Cases: len(corpus.Cases)},
		Embedding:        EmbeddingInfo{Provider: "ollama", Model: opts.model, ProfileID: profileID, Dimensions: dimensions, Embedded: embedded},
		MaxItems:         opts.maxItems,
		Latency:          map[string]LatencyStats{},
	}

	channelLimit := len(corpus.Memories)
	base := memorykit.RecallPolicy{
		Version: evaluatorVersion, RRFK: 60,
		MaxItems: opts.maxItems, MaxTokens: opts.maxItems, MaxQueryRunes: 256, MaxKeys: 8,
		Deadline: 30 * time.Second, EmbeddingProfileID: profileID, EmbeddingDimensions: dimensions,
	}
	type policy struct {
		name                    string
		exact, fullText, vector int
		threshold               *float64
	}
	policies := []policy{
		{name: "exact-only", exact: channelLimit},
		{name: "fts-only", fullText: channelLimit},
		{name: "exact+fts", exact: channelLimit, fullText: channelLimit},
	}
	for _, threshold := range opts.thresholds {
		threshold := threshold
		policies = append(policies,
			policy{name: "vector-only", vector: channelLimit, threshold: &threshold},
			policy{name: "exact+fts+vector", exact: channelLimit, fullText: channelLimit, vector: channelLimit, threshold: &threshold},
		)
	}

	var recallLatencies []float64
	for _, p := range policies {
		recallPolicy := base
		recallPolicy.ExactLimit, recallPolicy.FullTextLimit, recallPolicy.VectorLimit = p.exact, p.fullText, p.vector
		if p.threshold != nil {
			recallPolicy.MinVectorSimilarity = *p.threshold
		}
		recaller, err := memorykit.NewRecaller(memorykit.RecallConfig{
			Store: store, Embedder: embedder, CountTokens: func(string) int { return 1 }, Policy: recallPolicy,
		})
		if err != nil {
			return nil, fmt.Errorf("NewRecaller(%s): %w", p.name, err)
		}
		results := make(map[string][]string, len(corpus.Cases))
		var degraded []string
		for _, evalCase := range corpus.Cases {
			started := time.Now()
			recall, err := recaller.Recall(ctx, memorykit.RecallRequest{
				Scope: caseScope(evalCase), Text: evalCase.Query,
				Keys: append([]string(nil), evalCase.Keys...), Now: evalCase.Now,
			})
			if err != nil {
				return nil, fmt.Errorf("Recall(%s, %s): %w", p.name, evalCase.ID, err)
			}
			if p.vector > 0 {
				recallLatencies = append(recallLatencies, milliseconds(time.Since(started)))
			}
			if len(recall.DegradedChannels) > 0 {
				degraded = append(degraded, evalCase.ID)
			}
			results[evalCase.ID] = []string{}
			for _, item := range recall.Items {
				corpusID, ok := corpusIDByStoreID[item.Memory.ID]
				if !ok {
					return nil, fmt.Errorf("Recall(%s, %s) returned unknown memory %q", p.name, evalCase.ID, item.Memory.ID)
				}
				results[evalCase.ID] = append(results[evalCase.ID], corpusID)
			}
		}
		overall, err := memorykit.Evaluate(results, corpus.Cases)
		if err != nil {
			return nil, err
		}
		policyReport := PolicyReport{
			Name: p.name, MinVectorSimilarity: p.threshold, Overall: overall,
			Categories: categoryReports(corpus.Cases, results), DegradedCases: degraded,
		}
		policyReport.Misses, policyReport.Injections = missesAndInjections(corpus.Cases, results)
		report.Policies = append(report.Policies, policyReport)
	}
	report.Latency["query_embedding"] = latencyStats(embedder.latencies)
	report.Latency["recall_with_vector"] = latencyStats(recallLatencies)

	report.Similarities, err = caseSimilarities(ctx, store, embedder, corpus, corpusIDByStoreID, profileID, dimensions)
	if err != nil {
		return nil, err
	}
	return report, nil
}

func caseScope(evalCase memorykit.EvalCase) memorykit.Scope {
	return memorykit.Scope{TenantID: evalCase.TenantID, SubjectType: memorykit.SubjectProject, SubjectID: evalCase.ProjectID}
}

func openIsolatedStore(ctx context.Context, dsn, profileID string, dimensions int, keep bool) (*pgstore.Store, func(), error) {
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, err
	}
	// Install pgvector on the default path so dropping the schema keeps it.
	if _, err := admin.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		_ = admin.Close()
		return nil, nil, fmt.Errorf("create vector extension: %w", err)
	}
	schema := fmt.Sprintf("memory_embedding_eval_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		return nil, nil, fmt.Errorf("create schema: %w", err)
	}
	dropSchema := func() {
		if !keep {
			_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		}
		_ = admin.Close()
	}
	scoped, err := url.Parse(dsn)
	if err != nil || scoped.Scheme == "" {
		dropSchema()
		return nil, nil, errors.New("DSN must be a PostgreSQL URL")
	}
	query := scoped.Query()
	query.Set("search_path", schema+",public")
	scoped.RawQuery = query.Encode()
	store, err := pgstore.Open(ctx, scoped.String(), pgstore.Config{
		Limits: memorykit.Limits{
			Version: "memory-embedding-eval", MaxKeyRunes: 128, MaxContentRunes: 4096,
			MaxMetadataRunes: 512, MaxSourcesPerMemory: 8, MaxListItems: 100,
		},
		EmbeddingProfileID: profileID, EmbeddingDimensions: dimensions,
	})
	if err != nil {
		dropSchema()
		return nil, nil, fmt.Errorf("open pgstore: %w", err)
	}
	return store, func() { _ = store.Close(); dropSchema() }, nil
}

func seed(ctx context.Context, store *pgstore.Store, corpus memorykit.EvalCorpus) (map[string]string, error) {
	namespace := uuid.MustParse("3bbde908-7dd2-42c0-aa14-106bf8cb62e5")
	byStoreID := make(map[string]string, len(corpus.Memories))
	for _, evalMemory := range corpus.Memories {
		storeID := uuid.NewSHA1(namespace, []byte(evalMemory.ID)).String()
		if _, err := store.Create(ctx, memorykit.CreateRequest{
			ID: storeID,
			Scope: memorykit.Scope{
				TenantID: evalMemory.TenantID, SubjectType: memorykit.SubjectProject, SubjectID: evalMemory.ProjectID,
			},
			Kind: memorykit.Kind(evalMemory.Kind), Key: evalMemory.Key,
			Status: memorykit.Status(evalMemory.Status), Content: evalMemory.Content,
			ValidFrom: evalMemory.ValidFrom, ValidUntil: evalMemory.ValidUntil,
			Importance: 50, Confidence: 1,
			Actor: "memory-embedding-eval", Reason: "versioned corpus", Now: corpus.Cases[0].Now,
		}); err != nil {
			return nil, fmt.Errorf("seed memory %q: %w", evalMemory.ID, err)
		}
		byStoreID[storeID] = evalMemory.ID
	}
	return byStoreID, nil
}

func embedAll(ctx context.Context, store *pgstore.Store, embedder memorykit.Embedder, profileID string, dimensions int) (int, error) {
	worker, err := memorykit.NewEmbeddingWorker(memorykit.EmbeddingWorkerConfig{
		Store: store, Embedder: embedder, ProfileID: profileID, Dimensions: dimensions, BatchSize: 8,
		RunGate: store.EmbeddingRunGate(),
	})
	if err != nil {
		return 0, err
	}
	total := 0
	for {
		stored, err := worker.RunOnce(ctx)
		if err != nil {
			return total, fmt.Errorf("embedding worker: %w", err)
		}
		if stored == 0 {
			break
		}
		total += stored
	}
	snapshot, err := worker.Snapshot(ctx)
	if err != nil {
		return total, err
	}
	if snapshot.Backlog != 0 {
		return total, fmt.Errorf("embedding backlog not drained: %d left", snapshot.Backlog)
	}
	return total, nil
}

func caseSimilarities(
	ctx context.Context, store *pgstore.Store, embedder *ollamaEmbedder, corpus memorykit.EvalCorpus,
	corpusIDByStoreID map[string]string, profileID string, dimensions int,
) ([]CaseSimilarity, error) {
	out := make([]CaseSimilarity, 0, len(corpus.Cases))
	for _, evalCase := range corpus.Cases {
		vectors, err := embedder.embedTexts(ctx, []string{evalCase.Query})
		if err != nil {
			return nil, err
		}
		set, err := store.SearchCandidates(ctx, memorykit.CandidateQuery{
			Scope: caseScope(evalCase), Text: evalCase.Query, Now: evalCase.Now,
			QueryVector: vectors[0], VectorLimit: len(corpus.Memories), MinVectorSimilarity: 0,
			EmbeddingProfileID: profileID, EmbeddingDimensions: dimensions,
		})
		if err != nil {
			return nil, fmt.Errorf("similarities for %s: %w", evalCase.ID, err)
		}
		expected := setOf(evalCase.ExpectedIDs)
		forbidden := setOf(evalCase.ForbiddenIDs)
		item := CaseSimilarity{CaseID: evalCase.ID, Category: evalCase.Category}
		for _, candidate := range set.Vector {
			id := corpusIDByStoreID[candidate.Memory.ID]
			similarity := round(candidate.Similarity)
			switch {
			case expected[id]:
				if item.Expected == nil {
					item.Expected = map[string]float64{}
				}
				item.Expected[id] = similarity
			case forbidden[id]:
				if item.Forbidden == nil {
					item.Forbidden = map[string]float64{}
				}
				item.Forbidden[id] = similarity
			case similarity > item.TopOtherS:
				item.TopOther, item.TopOtherS = id, similarity
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func categoryReports(cases []memorykit.EvalCase, results map[string][]string) map[string]CategoryRe {
	out := map[string]CategoryRe{}
	for _, evalCase := range cases {
		got := setOf(results[evalCase.ID])
		category := out[evalCase.Category]
		category.ExpectedTotal += len(evalCase.ExpectedIDs)
		category.ForbiddenTotal += len(evalCase.ForbiddenIDs)
		for _, id := range evalCase.ExpectedIDs {
			if got[id] {
				category.ExpectedFound++
			}
		}
		for _, id := range evalCase.ForbiddenIDs {
			if got[id] {
				category.ForbiddenInjected++
			}
		}
		out[evalCase.Category] = category
	}
	return out
}

func missesAndInjections(cases []memorykit.EvalCase, results map[string][]string) (misses, injections []string) {
	for _, evalCase := range cases {
		got := setOf(results[evalCase.ID])
		for _, id := range evalCase.ExpectedIDs {
			if !got[id] {
				misses = append(misses, evalCase.ID+"/"+id)
			}
		}
		for _, id := range evalCase.ForbiddenIDs {
			if got[id] {
				injections = append(injections, evalCase.ID+"/"+id)
			}
		}
	}
	return misses, injections
}

func printSummary(w io.Writer, report *Report) {
	fmt.Fprintf(w, "corpus %s (%d memories, %d cases), %s %s %dd, max_items=%d\n",
		report.Corpus.Version, report.Corpus.Memories, report.Corpus.Cases,
		report.Embedding.Provider, report.Embedding.Model, report.Embedding.Dimensions, report.MaxItems)
	fmt.Fprintf(w, "%-18s %6s %10s %10s  %s\n", "policy", "min_sim", "found", "injected", "synonym")
	for _, policy := range report.Policies {
		threshold := "-"
		if policy.MinVectorSimilarity != nil {
			threshold = strconv.FormatFloat(*policy.MinVectorSimilarity, 'f', 2, 64)
		}
		synonym := policy.Categories["synonym"]
		fmt.Fprintf(w, "%-18s %6s %7d/%-2d %7d/%-2d  %d/%d\n", policy.Name, threshold,
			policy.Overall.ExpectedFound, policy.Overall.ExpectedTotal,
			policy.Overall.ForbiddenInjected, policy.Overall.ForbiddenTotal,
			synonym.ExpectedFound, synonym.ExpectedTotal)
	}
	for name, stats := range report.Latency {
		fmt.Fprintf(w, "latency %-20s p50=%.1fms p95=%.1fms max=%.1fms (n=%d)\n", name, stats.P50, stats.P95, stats.Max, stats.Count)
	}
}

type ollamaEmbedder struct {
	base, model, profileID string
	client                 *http.Client
	latencies              []float64
}

func (e *ollamaEmbedder) Embed(ctx context.Context, request memorykit.EmbedRequest) ([][]float32, error) {
	if request.ProfileID != e.profileID {
		return nil, fmt.Errorf("%w: embedding profile %q", memorykit.ErrInvalidMemory, request.ProfileID)
	}
	return e.embedTexts(ctx, request.Texts)
}

func (e *ollamaEmbedder) embedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": e.model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.base+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, &memorykit.BackendError{Op: "embed", Recoverable: true, Err: err}
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &memorykit.BackendError{Op: "embed", Recoverable: true, Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &memorykit.BackendError{Op: "embed", Recoverable: resp.StatusCode >= 500, Err: fmt.Errorf("ollama status %d", resp.StatusCode)}
	}
	var decoded struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("decode ollama response: %w", err)
	}
	if len(decoded.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama returned %d embeddings for %d texts", len(decoded.Embeddings), len(texts))
	}
	if len(texts) == 1 {
		e.latencies = append(e.latencies, milliseconds(time.Since(started)))
	}
	return decoded.Embeddings, nil
}

func latencyStats(values []float64) LatencyStats {
	if len(values) == 0 {
		return LatencyStats{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	at := func(q float64) float64 {
		return round(sorted[int(math.Ceil(q*float64(len(sorted))))-1])
	}
	return LatencyStats{Count: len(sorted), P50: at(0.5), P95: at(0.95), Max: round(sorted[len(sorted)-1])}
}

func milliseconds(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func round(value float64) float64 { return math.Round(value*1000) / 1000 }

func setOf(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}
