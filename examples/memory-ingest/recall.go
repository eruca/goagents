package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/eruca/goagents/memorykit"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type httpEmbedder struct {
	endpoint, key, model, profile string
	dimensions                    int
	client                        *http.Client
}

func newHTTPEmbedder(profile string, dimensions int) (*httpEmbedder, error) {
	endpoint := os.Getenv("MEMORY_EMBEDDING_URL")
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))) {
		return nil, errors.New("embedding URL must be HTTPS or loopback HTTP")
	}
	model := os.Getenv("MEMORY_EMBEDDING_MODEL")
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("embedding model required")
	}
	return &httpEmbedder{endpoint: endpoint, key: os.Getenv("MEMORY_EMBEDDING_API_KEY"), model: model, profile: profile, dimensions: dimensions, client: &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (e *httpEmbedder) Embed(ctx context.Context, req memorykit.EmbedRequest) ([][]float32, error) {
	if req.ProfileID != e.profile {
		return nil, memorykit.ErrInvalidMemory
	}
	b, err := json.Marshal(map[string]any{"model": e.model, "input": req.Texts})
	if err != nil {
		return nil, memorykit.ErrInvalidMemory
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, memorykit.ErrInvalidMemory
	}
	request.Header.Set("Content-Type", "application/json")
	if e.key != "" {
		request.Header.Set("Authorization", "Bearer "+e.key)
	}
	response, err := e.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &memorykit.BackendError{Op: "embedding", Recoverable: true, Err: errors.New("embedding transport unavailable")}
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		if response.StatusCode == 429 || response.StatusCode >= 500 {
			return nil, &memorykit.BackendError{Op: "embedding", Recoverable: true, Err: errors.New("embedding unavailable")}
		}
		return nil, memorykit.ErrInvalidMemory
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(raw) > 8<<20 {
		return nil, memorykit.ErrInvalidMemory
	}
	var output struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &output) != nil || len(output.Data) != len(req.Texts) {
		return nil, memorykit.ErrInvalidMemory
	}
	vectors := make([][]float32, len(req.Texts))
	for _, item := range output.Data {
		if item.Index < 0 || item.Index >= len(vectors) || vectors[item.Index] != nil || len(item.Embedding) != e.dimensions {
			return nil, memorykit.ErrInvalidMemory
		}
		vectors[item.Index] = item.Embedding
	}
	return vectors, nil
}

type benchmarkCase struct {
	memorykit.EvalCase
	Keys []string `json:"keys,omitempty"`
}

func benchmark(ctx context.Context, recaller *memorykit.Recaller, scope memorykit.Scope, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("benchmark cases unavailable")
	}
	defer f.Close()
	var cases []benchmarkCase
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cases) != nil || len(cases) == 0 || len(cases) > 200 {
		return errors.New("invalid benchmark cases")
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid trailing benchmark data")
	}
	results := map[string][]string{}
	var evalCases []memorykit.EvalCase
	var elapsed []time.Duration
	reciprocal := 0.0
	failures := 0
	noAnswer := 0
	noAnswerReturned := 0
	degraded := 0
	seen := map[string]bool{}
	for _, c := range cases {
		if c.ID == "" || seen[c.ID] {
			return errors.New("case IDs must be nonempty and unique")
		}
		seen[c.ID] = true
		evalCases = append(evalCases, c.EvalCase)
		start := time.Now()
		result, err := recaller.Recall(ctx, memorykit.RecallRequest{Scope: scope, Text: c.Query, Keys: c.Keys, Now: time.Now().UTC()})
		elapsed = append(elapsed, time.Since(start))
		if err != nil {
			failures++
			if ctx.Err() != nil {
				return errors.New("benchmark deadline exceeded")
			}
			continue
		}
		if len(result.DegradedChannels) > 0 {
			degraded++
		}
		if len(c.ExpectedIDs) == 0 {
			noAnswer++
			if len(result.Items) > 0 {
				noAnswerReturned++
			}
		}
		rankFound := false
		for rank, item := range result.Items {
			results[c.ID] = append(results[c.ID], item.Memory.ID)
			if !rankFound {
				for _, expected := range c.ExpectedIDs {
					if expected == item.Memory.ID {
						reciprocal += 1 / float64(rank+1)
						rankFound = true
						break
					}
				}
			}
		}
	}
	report, err := memorykit.Evaluate(results, evalCases)
	if err != nil {
		return errors.New("invalid evaluation labels")
	}
	sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
	answerable := 0
	for _, c := range cases {
		if len(c.ExpectedIDs) > 0 {
			answerable++
		}
	}
	mrr := 0.0
	if answerable > 0 {
		mrr = reciprocal / float64(answerable)
	}
	return emit(map[string]any{"evaluation": report, "mrr_at_5": mrr, "p50_ms": float64(elapsed[(len(elapsed)-1)/2]) / float64(time.Millisecond), "p95_ms": float64(elapsed[(len(elapsed)*95+99)/100-1]) / float64(time.Millisecond), "recall_errors": failures, "degraded_cases": degraded, "no_answer_cases": noAnswer, "no_answer_returned": noAnswerReturned, "policy": "file-recall-v1"})
}
