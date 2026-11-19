package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOllama serves /api/embed with deterministic 8-dimensional vectors
// derived from the text's words, so related texts share dimensions.
func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "fake" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		embeddings := make([][]float32, len(request.Input))
		for i, text := range request.Input {
			vector := make([]float32, 8)
			vector[0] = 0.01
			for _, word := range strings.Fields(strings.ToLower(text)) {
				sum := sha256.Sum256([]byte(word))
				vector[int(sum[0])%8]++
			}
			embeddings[i] = vector
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	}))
}

func TestEvaluateRunsEveryPolicyAgainstPostgreSQL(t *testing.T) {
	dsn := os.Getenv("MEMORYKIT_POSTGRES_TEST_DSN")
	if dsn == "" {
		if os.Getenv("MEMORYKIT_REQUIRE_POSTGRES") == "1" {
			t.Fatal("MEMORYKIT_POSTGRES_TEST_DSN is required")
		}
		t.Skip("set MEMORYKIT_POSTGRES_TEST_DSN to run against PostgreSQL")
	}
	server := fakeOllama(t)
	defer server.Close()
	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--dsn", dsn, "--ollama", server.URL, "--model", "fake",
		"--corpus", "../../memorykit/testdata/recall_cases.json",
		"--min-similarity", "0,0.5", "--out", out,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Embedding.Dimensions != 8 || report.Embedding.Embedded == 0 || report.Corpus.Cases != 24 {
		t.Fatalf("report header = %+v %+v", report.Embedding, report.Corpus)
	}
	// 3 lexical policies plus vector-only and fused per threshold.
	if len(report.Policies) != 7 {
		t.Fatalf("policies = %d", len(report.Policies))
	}
	byName := map[string]PolicyReport{}
	for _, policy := range report.Policies {
		byName[policy.Name] = policy
		if policy.Overall.ExpectedTotal != 22 || policy.Overall.ForbiddenTotal != 30 {
			t.Fatalf("%s totals = %+v", policy.Name, policy.Overall)
		}
	}
	// The lexical policies do not depend on the embedding model and match
	// the pinned PostgreSQL lexical evaluation.
	for name, want := range map[string]int{"exact-only": 11, "fts-only": 15, "exact+fts": 19} {
		if got := byName[name].Overall.ExpectedFound; got != want {
			t.Fatalf("%s found %d, want %d", name, got, want)
		}
	}
	if len(report.Similarities) != 24 || report.Latency["query_embedding"].Count == 0 {
		t.Fatalf("diagnostics missing: %d similarities, latency %+v", len(report.Similarities), report.Latency)
	}
	// The report carries IDs only, never corpus text.
	if strings.Contains(string(raw), "PostgreSQL is the only durable source of truth") ||
		strings.Contains(string(raw), "What is the authoritative persistent store") {
		t.Fatal("report contains corpus text")
	}
	if !strings.Contains(stdout.String(), "exact+fts+vector") {
		t.Fatalf("summary = %s", stdout.String())
	}
}

func TestParseOptionsRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{
		{"--dsn", ""},
		{"--dsn", "postgres://x", "--min-similarity", "1.5"},
		{"--dsn", "postgres://x", "--max-items", "0"},
		{"--dsn", "postgres://x", "extra"},
	} {
		t.Setenv("MEMORYKIT_POSTGRES_TEST_DSN", "")
		if _, err := parseOptions(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("parseOptions(%q) accepted", args)
		}
	}
}
