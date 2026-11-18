package memorykit_test

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/memorystore"
	"github.com/google/uuid"
)

// TestEvaluateRetrievalV2LexicalAblations runs the bilingual v2 corpus
// through the production Recaller over memorystore with the vector channel
// disabled. Scope and lifecycle leaks must stay at zero; positive recall and
// the one known in-scope distractor are pinned to what the current tokenizer
// actually achieves, so regressions and improvements both surface here.
func TestEvaluateRetrievalV2LexicalAblations(t *testing.T) {
	corpus := loadLexicalEvalCorpus(t)
	if err := memorykit.ValidateEvalCorpus(corpus); err != nil {
		t.Fatalf("ValidateEvalCorpus(): %v", err)
	}

	const maxItems = 4
	results, reports := runLexicalEvalAblations(t, corpus, maxItems)
	for policy, cases := range results {
		for caseID, ids := range cases {
			if len(ids) > maxItems {
				t.Fatalf("%s case %q returned %d items, max %d", policy, caseID, len(ids), maxItems)
			}
		}
	}
	for _, name := range sortedLexicalPolicyNames(reports) {
		t.Logf("%s lexical ablation: %+v", name, reports[name])
		logLexicalCategoryReports(t, name, corpus.Cases, results[name])
	}
	// A forbidden memory outside the request's tenant/project, or not
	// active and valid at the case's time, must never be returned by any
	// channel: that would be a scope or lifecycle leak.
	memories := make(map[string]memorykit.EvalMemory, len(corpus.Memories))
	for _, evalMemory := range corpus.Memories {
		memories[evalMemory.ID] = evalMemory
	}
	for _, name := range sortedLexicalPolicyNames(reports) {
		for _, evalCase := range corpus.Cases {
			for _, id := range results[name][evalCase.ID] {
				if !lexicalEvalEligible(memories[id], evalCase) {
					t.Fatalf("%s case %q returned ineligible memory %q", name, evalCase.ID, id)
				}
			}
		}
	}

	// Full-text recall matches any shared term (see README "Recall paths"),
	// so an in-scope distractor that shares words with the query can be
	// returned. The only such injection today is the abstain case: "UI ...
	// theme" shares "ui" and "theme" with ui.theme_draft. It is pinned here
	// so any new injection fails, and fixing it fails too until this list is
	// updated.
	knownInjections := map[string][]string{
		"exact-only": nil,
		"fts-only":   {"q-abstain-theme/m-noise-theme"},
		"exact+fts":  {"q-abstain-theme/m-noise-theme"},
	}
	for _, name := range sortedLexicalPolicyNames(reports) {
		got := lexicalForbiddenInjections(corpus.Cases, results[name])
		if !reflect.DeepEqual(got, knownInjections[name]) {
			t.Fatalf("%s forbidden injections = %v, want %v", name, got, knownInjections[name])
		}
	}

	if reports["fts-only"].ExpectedFound == 0 {
		t.Fatalf("fts-only made no positive contribution: %+v", reports["fts-only"])
	}
	if reports["exact+fts"].ExpectedFound <= reports["exact-only"].ExpectedFound {
		t.Fatalf("exact+fts did not improve exact-only: exact=%+v exact+fts=%+v",
			reports["exact-only"], reports["exact+fts"])
	}
	wantFound := map[string]int{"exact-only": 11, "fts-only": 16, "exact+fts": 19}
	for _, name := range sortedLexicalPolicyNames(reports) {
		got := reports[name]
		if got.ExpectedFound != wantFound[name] || got.ExpectedTotal != 22 || got.ForbiddenTotal != 30 {
			t.Fatalf("%s report = %+v, want ExpectedFound=%d ExpectedTotal=22 ForbiddenTotal=30",
				name, got, wantFound[name])
		}
	}
}

func lexicalEvalEligible(memory memorykit.EvalMemory, evalCase memorykit.EvalCase) bool {
	return memory.TenantID == evalCase.TenantID && memory.ProjectID == evalCase.ProjectID &&
		memorykit.Status(memory.Status) == memorykit.StatusActive && !evalCase.Now.Before(memory.ValidFrom) &&
		(memory.ValidUntil.IsZero() || evalCase.Now.Before(memory.ValidUntil))
}

func lexicalForbiddenInjections(cases []memorykit.EvalCase, results map[string][]string) []string {
	var injections []string
	for _, evalCase := range cases {
		for _, forbidden := range evalCase.ForbiddenIDs {
			for _, id := range results[evalCase.ID] {
				if id == forbidden {
					injections = append(injections, evalCase.ID+"/"+id)
					break
				}
			}
		}
	}
	return injections
}

func loadLexicalEvalCorpus(t *testing.T) memorykit.EvalCorpus {
	t.Helper()
	data, err := os.ReadFile("testdata/recall_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus memorykit.EvalCorpus
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&corpus); err != nil {
		t.Fatal(err)
	}
	return corpus
}

func runLexicalEvalAblations(
	t *testing.T,
	corpus memorykit.EvalCorpus,
	maxItems int,
) (map[string]map[string][]string, map[string]memorykit.EvalReport) {
	t.Helper()
	const channelLimit = 36
	limits := memorykit.Limits{
		Version: "project-memory-v1", MaxKeyRunes: 128, MaxContentRunes: 4096,
		MaxMetadataRunes: 512, MaxSourcesPerMemory: 8, MaxListItems: 100,
	}
	store, err := memorystore.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	corpusIDByStoreID := make(map[string]string, len(corpus.Memories))
	for _, evalMemory := range corpus.Memories {
		storeID := stableLexicalEvalUUID(evalMemory.ID)
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
			Actor: "eval-harness", Reason: "versioned lexical corpus", Now: corpus.Cases[0].Now,
		}); err != nil {
			t.Fatalf("seed memory %q: %v", evalMemory.ID, err)
		}
		corpusIDByStoreID[storeID] = evalMemory.ID
	}

	base := memorykit.RecallPolicy{
		Version: memorykit.EvalCorpusVersionV2, RRFK: 60,
		MaxItems: maxItems, MaxTokens: maxItems, MaxQueryRunes: 256, MaxKeys: 8,
		Deadline: time.Second, EmbeddingProfileID: "vector-disabled", EmbeddingDimensions: 1,
	}
	policies := map[string]memorykit.RecallPolicy{
		"exact-only": withLexicalEvalChannels(base, channelLimit, 0),
		"fts-only":   withLexicalEvalChannels(base, 0, channelLimit),
		"exact+fts":  withLexicalEvalChannels(base, channelLimit, channelLimit),
	}
	results := make(map[string]map[string][]string, len(policies))
	reports := make(map[string]memorykit.EvalReport, len(policies))
	for name, policy := range policies {
		recaller, err := memorykit.NewRecaller(memorykit.RecallConfig{
			Store: store, CountTokens: func(string) int { return 1 }, Policy: policy,
		})
		if err != nil {
			t.Fatalf("NewRecaller(%s): %v", name, err)
		}
		policyResults := make(map[string][]string, len(corpus.Cases))
		for _, evalCase := range corpus.Cases {
			recall, err := recaller.Recall(t.Context(), memorykit.RecallRequest{
				Scope: memorykit.Scope{
					TenantID:    evalCase.TenantID,
					SubjectType: memorykit.SubjectProject,
					SubjectID:   evalCase.ProjectID,
				},
				Text: evalCase.Query,
				Keys: append([]string(nil), evalCase.Keys...),
				Now:  evalCase.Now,
			})
			if err != nil {
				t.Fatalf("Recall(%s, %s): %v", name, evalCase.ID, err)
			}
			for _, item := range recall.Items {
				corpusID, ok := corpusIDByStoreID[item.Memory.ID]
				if !ok {
					t.Fatalf("Recall(%s, %s) returned unknown store ID %q", name, evalCase.ID, item.Memory.ID)
				}
				policyResults[evalCase.ID] = append(policyResults[evalCase.ID], corpusID)
			}
		}
		report, err := memorykit.Evaluate(policyResults, corpus.Cases)
		if err != nil {
			t.Fatalf("Evaluate(%s): %v", name, err)
		}
		results[name] = policyResults
		reports[name] = report
	}
	return results, reports
}

func withLexicalEvalChannels(base memorykit.RecallPolicy, exactLimit, fullTextLimit int) memorykit.RecallPolicy {
	base.ExactLimit = exactLimit
	base.FullTextLimit = fullTextLimit
	base.VectorLimit = 0
	return base
}

func logLexicalCategoryReports(
	t *testing.T,
	policy string,
	cases []memorykit.EvalCase,
	results map[string][]string,
) {
	t.Helper()
	categoryCases := make(map[string][]memorykit.EvalCase)
	for _, evalCase := range cases {
		categoryCases[evalCase.Category] = append(categoryCases[evalCase.Category], evalCase)
	}
	categories := make([]string, 0, len(categoryCases))
	for category := range categoryCases {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	for _, category := range categories {
		report, err := memorykit.Evaluate(results, categoryCases[category])
		if err != nil {
			t.Fatalf("Evaluate(%s, %s): %v", policy, category, err)
		}
		t.Logf("%s/%s: found %d/%d, forbidden %d/%d", policy, category,
			report.ExpectedFound, report.ExpectedTotal, report.ForbiddenInjected, report.ForbiddenTotal)
	}
	for _, evalCase := range cases {
		t.Logf("%s/%s %q -> %v (want %v)", policy, evalCase.ID, evalCase.Query,
			results[evalCase.ID], evalCase.ExpectedIDs)
	}
}

func sortedLexicalPolicyNames(reports map[string]memorykit.EvalReport) []string {
	names := make([]string, 0, len(reports))
	for name := range reports {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func stableLexicalEvalUUID(corpusID string) string {
	namespace := uuid.MustParse("0473803e-dbb4-4cc4-9626-69003b7a3eab")
	return uuid.NewSHA1(namespace, []byte(corpusID)).String()
}
