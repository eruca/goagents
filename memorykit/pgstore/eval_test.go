package pgstore

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/google/uuid"
)

// TestCJKLexicalEvalCorpus runs the bilingual v2 corpus against real
// PostgreSQL with the vector channel disabled. It mirrors the memorystore
// evaluation in memorykit/eval_lexical_test.go: pgstore follows the same
// full-text rule, so the pinned numbers are the same.
func TestCJKLexicalEvalCorpus(t *testing.T) {
	corpus := loadPostgresEvalCorpus(t)
	store := openIsolatedSchemaStore(t)
	corpusIDByStoreID := seedPostgresEvalCorpus(t, store, corpus)
	memories := make(map[string]memorykit.EvalMemory, len(corpus.Memories))
	for _, evalMemory := range corpus.Memories {
		memories[evalMemory.ID] = evalMemory
	}

	const (
		channelLimit = 36
		maxItems     = 4
	)
	base := memorykit.RecallPolicy{
		Version: memorykit.EvalCorpusVersionV2, RRFK: 60,
		MaxItems: maxItems, MaxTokens: maxItems, MaxQueryRunes: 256, MaxKeys: 8,
		Deadline: time.Second, EmbeddingProfileID: "vector-disabled", EmbeddingDimensions: 1,
	}
	policies := map[string]memorykit.RecallPolicy{
		"exact-only": postgresEvalPolicy(base, channelLimit, 0),
		"fts-only":   postgresEvalPolicy(base, 0, channelLimit),
		"exact+fts":  postgresEvalPolicy(base, channelLimit, channelLimit),
	}
	// See eval_lexical_test.go: full-text matches any shared term, so the
	// abstain case returns the in-scope ui.theme_draft distractor.
	knownInjections := map[string][]string{
		"exact-only": nil,
		"fts-only":   {"q-abstain-theme/m-noise-theme"},
		"exact+fts":  {"q-abstain-theme/m-noise-theme"},
	}
	wantFound := map[string]int{"exact-only": 11, "fts-only": 16, "exact+fts": 19}
	names := make([]string, 0, len(policies))
	for name := range policies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		recaller, err := memorykit.NewRecaller(memorykit.RecallConfig{
			Store: store, CountTokens: func(string) int { return 1 }, Policy: policies[name],
		})
		if err != nil {
			t.Fatalf("NewRecaller(%s): %v", name, err)
		}
		results := make(map[string][]string, len(corpus.Cases))
		var injections []string
		for _, evalCase := range corpus.Cases {
			recall, err := recaller.Recall(t.Context(), memorykit.RecallRequest{
				Scope: memorykit.Scope{
					TenantID:    evalCase.TenantID,
					SubjectType: memorykit.SubjectProject,
					SubjectID:   evalCase.ProjectID,
				},
				Text: evalCase.Query, Keys: append([]string(nil), evalCase.Keys...), Now: evalCase.Now,
			})
			if err != nil {
				t.Fatalf("Recall(%s, %s): %v", name, evalCase.ID, err)
			}
			for _, item := range recall.Items {
				corpusID, exists := corpusIDByStoreID[item.Memory.ID]
				if !exists {
					t.Fatalf("Recall(%s, %s) returned unknown memory %q", name, evalCase.ID, item.Memory.ID)
				}
				if !postgresEvalEligible(memories[corpusID], evalCase) {
					t.Fatalf("%s case %q returned ineligible memory %q", name, evalCase.ID, corpusID)
				}
				results[evalCase.ID] = append(results[evalCase.ID], corpusID)
				for _, forbidden := range evalCase.ForbiddenIDs {
					if forbidden == corpusID {
						injections = append(injections, evalCase.ID+"/"+corpusID)
					}
				}
			}
		}
		report, err := memorykit.Evaluate(results, corpus.Cases)
		if err != nil {
			t.Fatalf("Evaluate(%s): %v", name, err)
		}
		t.Logf("%s PostgreSQL lexical evaluation: %+v", name, report)
		if !reflect.DeepEqual(injections, knownInjections[name]) {
			t.Fatalf("%s forbidden injections = %v, want %v", name, injections, knownInjections[name])
		}
		if report.ExpectedFound != wantFound[name] || report.ExpectedTotal != 22 || report.ForbiddenTotal != 30 {
			t.Fatalf("%s report = %+v, want ExpectedFound=%d ExpectedTotal=22 ForbiddenTotal=30",
				name, report, wantFound[name])
		}
	}
}

func postgresEvalEligible(memory memorykit.EvalMemory, evalCase memorykit.EvalCase) bool {
	return memory.TenantID == evalCase.TenantID && memory.ProjectID == evalCase.ProjectID &&
		memorykit.Status(memory.Status) == memorykit.StatusActive && !evalCase.Now.Before(memory.ValidFrom) &&
		(memory.ValidUntil.IsZero() || evalCase.Now.Before(memory.ValidUntil))
}

func loadPostgresEvalCorpus(t *testing.T) memorykit.EvalCorpus {
	t.Helper()
	data, err := os.ReadFile("../testdata/recall_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus memorykit.EvalCorpus
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&corpus); err != nil {
		t.Fatal(err)
	}
	if err := memorykit.ValidateEvalCorpus(corpus); err != nil {
		t.Fatalf("ValidateEvalCorpus(): %v", err)
	}
	return corpus
}

func seedPostgresEvalCorpus(
	t *testing.T,
	store *Store,
	corpus memorykit.EvalCorpus,
) map[string]string {
	t.Helper()
	result := make(map[string]string, len(corpus.Memories))
	for _, evalMemory := range corpus.Memories {
		storeID := postgresEvalUUID(evalMemory.ID)
		if _, err := store.Create(t.Context(), memorykit.CreateRequest{
			ID: storeID,
			Scope: memorykit.Scope{
				TenantID:    evalMemory.TenantID,
				SubjectType: memorykit.SubjectProject,
				SubjectID:   evalMemory.ProjectID,
			},
			Kind: memorykit.Kind(evalMemory.Kind), Key: evalMemory.Key,
			Status: memorykit.Status(evalMemory.Status), Content: evalMemory.Content,
			ValidFrom: evalMemory.ValidFrom, ValidUntil: evalMemory.ValidUntil,
			Importance: 50, Confidence: 1,
			Actor: "postgres-eval", Reason: "versioned lexical corpus", Now: corpus.Cases[0].Now,
		}); err != nil {
			t.Fatalf("seed memory %q: %v", evalMemory.ID, err)
		}
		result[storeID] = evalMemory.ID
	}
	return result
}

func postgresEvalPolicy(base memorykit.RecallPolicy, exactLimit, fullTextLimit int) memorykit.RecallPolicy {
	base.ExactLimit = exactLimit
	base.FullTextLimit = fullTextLimit
	base.VectorLimit = 0
	return base
}

func postgresEvalUUID(corpusID string) string {
	namespace := uuid.MustParse("3bbde908-7dd2-42c0-aa14-106bf8cb62e5")
	return uuid.NewSHA1(namespace, []byte(corpusID)).String()
}
