package memorykit_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/memorystore"
	"github.com/google/uuid"
)

func TestEvaluateProjectMemoryV1ProductionRecallerAblations(t *testing.T) {
	corpus := loadProductionEvalCorpus(t)
	if corpus.Version != "project-memory-v1" {
		t.Fatalf("corpus version = %q", corpus.Version)
	}
	if len(corpus.Memories) != 6 || len(corpus.Cases) != 4 {
		t.Fatalf("corpus shape = %d memories, %d cases", len(corpus.Memories), len(corpus.Cases))
	}

	const maxItems = 2
	results, reports := runProductionEvalAblations(t, corpus, maxItems)
	for policy, cases := range results {
		for caseID, ids := range cases {
			if len(ids) > maxItems {
				t.Fatalf("%s case %q returned %d items, max %d", policy, caseID, len(ids), maxItems)
			}
		}
	}

	if !containsProductionEvalID(results["exact-only"]["keyword-command"], "m-command") {
		t.Fatalf("production exact missed keyword case: %v", results["exact-only"]["keyword-command"])
	}
	if !containsProductionEvalID(results["vector-only"]["semantic-synonym"], "m-pg-gate") ||
		!containsProductionEvalID(results["fused"]["semantic-synonym"], "m-pg-gate") {
		t.Fatalf("production vector/RRF missed semantic case: vector=%v fused=%v",
			results["vector-only"]["semantic-synonym"], results["fused"]["semantic-synonym"])
	}

	want := map[string]memorykit.EvalReport{
		"exact-only": {
			Cases: 4, ExpectedFound: 1, ExpectedTotal: 2,
			ForbiddenTotal: 4, RecallAtBudget: 0.5,
		},
		"fts-only": {
			Cases: 4, ExpectedTotal: 2, ForbiddenTotal: 4,
		},
		"vector-only": {
			Cases: 4, ExpectedFound: 2, ExpectedTotal: 2,
			ForbiddenTotal: 4, RecallAtBudget: 1,
		},
		"fused": {
			Cases: 4, ExpectedFound: 2, ExpectedTotal: 2,
			ForbiddenTotal: 4, RecallAtBudget: 1,
		},
	}
	if !reflect.DeepEqual(reports, want) {
		t.Fatalf("production ablation reports = %#v, want %#v", reports, want)
	}
	for name, report := range reports {
		if report.FalseInjectionRate != 0 || report.ForbiddenInjected != 0 {
			t.Fatalf("%s injected forbidden memory: %#v", name, report)
		}
		t.Logf("%s: %+v", name, report)
	}
}

func loadProductionEvalCorpus(t *testing.T) memorykit.EvalCorpus {
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

func runProductionEvalAblations(
	t *testing.T,
	corpus memorykit.EvalCorpus,
	maxItems int,
) (map[string]map[string][]string, map[string]memorykit.EvalReport) {
	t.Helper()
	const (
		targetProject = "project-1"
		profileID     = "project-memory-v1-3d"
		channelLimit  = 6
	)
	now := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
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
		storeID := stableProductionEvalUUID(evalMemory.ID)
		scope := memorykit.Scope{
			TenantID: "eval-tenant", SubjectType: memorykit.SubjectProject, SubjectID: evalMemory.ProjectID,
		}
		created, err := store.Create(t.Context(), memorykit.CreateRequest{
			ID: storeID, Scope: scope, Kind: memorykit.Kind(evalMemory.Kind),
			Key: evalMemory.Key, Status: memorykit.Status(evalMemory.Status), Content: evalMemory.Content,
			ValidFrom: now.Add(-time.Hour), Importance: 50, Confidence: 1,
			Actor: "eval-harness", Reason: "fixed corpus", Now: now,
		})
		if err != nil {
			t.Fatalf("seed memory %q: %v", evalMemory.ID, err)
		}
		corpusIDByStoreID[storeID] = evalMemory.ID
		if created.Status != memorykit.StatusActive {
			continue
		}
		if err := store.PutEmbedding(t.Context(), memorykit.PutEmbeddingRequest{
			MemoryID: storeID, Scope: scope, ProfileID: profileID,
			ContentHash: productionEvalContentHash(evalMemory.Content),
			Dimensions:  3, Vector: append([]float32(nil), evalMemory.Vector...), EmbeddedAt: now,
		}); err != nil {
			t.Fatalf("seed embedding %q: %v", evalMemory.ID, err)
		}
	}

	base := memorykit.RecallPolicy{
		Version: "project-memory-v1", RRFK: 60, MinVectorSimilarity: 0.9,
		MaxItems: maxItems, MaxTokens: maxItems, MaxQueryRunes: 256, MaxKeys: 8,
		Deadline: time.Second, EmbeddingProfileID: profileID, EmbeddingDimensions: 3,
	}
	policies := map[string]memorykit.RecallPolicy{
		"exact-only":  withProductionEvalChannels(base, channelLimit, 0, 0),
		"fts-only":    withProductionEvalChannels(base, 0, channelLimit, 0),
		"vector-only": withProductionEvalChannels(base, 0, 0, channelLimit),
		"fused":       withProductionEvalChannels(base, channelLimit, channelLimit, channelLimit),
	}
	scope := memorykit.Scope{
		TenantID: "eval-tenant", SubjectType: memorykit.SubjectProject, SubjectID: targetProject,
	}
	results := make(map[string]map[string][]string, len(policies))
	reports := make(map[string]memorykit.EvalReport, len(policies))
	for name, policy := range policies {
		recaller, err := memorykit.NewRecaller(memorykit.RecallConfig{
			Store: store, Embedder: productionEvalEmbedder{profileID: profileID},
			CountTokens: func(string) int { return 1 }, Policy: policy,
		})
		if err != nil {
			t.Fatalf("NewRecaller(%s): %v", name, err)
		}
		policyResults := make(map[string][]string, len(corpus.Cases))
		for _, evalCase := range corpus.Cases {
			recall, err := recaller.Recall(t.Context(), memorykit.RecallRequest{
				Scope: scope, Text: evalCase.Query,
				Keys: productionEvalKeys(evalCase.Query, corpus.Memories), Now: now,
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

func withProductionEvalChannels(
	base memorykit.RecallPolicy,
	exactLimit, fullTextLimit, vectorLimit int,
) memorykit.RecallPolicy {
	base.ExactLimit = exactLimit
	base.FullTextLimit = fullTextLimit
	base.VectorLimit = vectorLimit
	return base
}

type productionEvalEmbedder struct{ profileID string }

func (e productionEvalEmbedder) Embed(ctx context.Context, request memorykit.EmbedRequest) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.ProfileID != e.profileID {
		return nil, memorykit.ErrInvalidMemory
	}
	vectors := make([][]float32, len(request.Texts))
	for index, text := range request.Texts {
		vectors[index] = productionEvalQueryVector(text)
	}
	return vectors, nil
}

func productionEvalKeys(query string, memories []memorykit.EvalMemory) []string {
	keys := make(map[string]struct{})
	for _, evalMemory := range memories {
		if productionEvalTokenOverlap(query, evalMemory.Key) >= 2 {
			keys[evalMemory.Key] = struct{}{}
		}
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func productionEvalTokenOverlap(left, right string) int {
	leftTokens := make(map[string]struct{})
	for _, token := range productionEvalTokens(left) {
		leftTokens[productionEvalNormalizeToken(token)] = struct{}{}
	}
	overlap := 0
	for _, token := range productionEvalTokens(right) {
		if _, ok := leftTokens[productionEvalNormalizeToken(token)]; ok {
			overlap++
		}
	}
	return overlap
}

func productionEvalQueryVector(query string) []float32 {
	var vector [3]float32
	for _, token := range productionEvalTokens(query) {
		switch productionEvalNormalizeToken(token) {
		case "command", "test", "deployment", "old":
			vector[0]++
		case "verify", "storage", "vector", "unreviewed", "lesson":
			vector[1]++
		case "documentation", "graphic":
			vector[2]++
		}
	}
	return vector[:]
}

func productionEvalTokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func productionEvalNormalizeToken(token string) string {
	return strings.TrimSuffix(token, "s")
}

func stableProductionEvalUUID(corpusID string) string {
	namespace := uuid.MustParse("0473803e-dbb4-4cc4-9626-69003b7a3eab")
	return uuid.NewSHA1(namespace, []byte(corpusID)).String()
}

func productionEvalContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func containsProductionEvalID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
