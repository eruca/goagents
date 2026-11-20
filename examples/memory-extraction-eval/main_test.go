package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/memorykit/agentadapter"
)

// TestEverySourceStillHoldsWhatItExpects checks the cases: whatever an
// extraction must keep has to survive RunSource's limit.
func TestEverySourceStillHoldsWhatItExpects(t *testing.T) {
	for _, c := range cases() {
		source := agentadapter.RunSource(c.Messages, limits.MaxContentRunes)
		for _, alternatives := range c.Expect {
			if !containsAny(source, alternatives) {
				t.Fatalf("%s: source lacks %v:\n%s", c.ID, alternatives, source)
			}
		}
	}
}

// scriptedExtractor answers with a fact for sources that state the staging
// port, nothing for others, and invalid output for the Chinese case.
type scriptedExtractor struct{}

func (scriptedExtractor) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	source := req.Messages[1].Content
	switch {
	case strings.Contains(source, "DB_PORT=55432"):
		return &ports.ChatResponse{Content: "```json\n" + `{"candidates":[{"kind":"fact","key":"database.staging.port","content":"The staging database listens on port 55432."}]}` + "\n```"}, nil
	case strings.Contains(source, "Redis"):
		return &ports.ChatResponse{Content: "Sure! Here are the memories."}, nil
	case strings.Contains(source, "hunter2"):
		return &ports.ChatResponse{Content: `{"candidates":[{"kind":"fact","key":"admin.password","content":"The admin password is hunter2."}]}`}, nil
	}
	return &ports.ChatResponse{Content: `{"candidates":[]}`}, nil
}

func TestGradingAndReport(t *testing.T) {
	var stdout bytes.Buffer
	if code := run(context.Background(), []string{"--trials", "1"}, &stdout, &bytes.Buffer{}, scriptedExtractor{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	report, err := evaluate(context.Background(), options{trials: 1, maxCandidates: 5, timeout: time.Minute}, scriptedExtractor{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]TrialReport{}
	for _, c := range report.Cases {
		byID[c.ID] = c.Trials[0]
	}
	if !byID["tool-fact"].Passed || !byID["nothing-durable"].Passed {
		t.Fatalf("tool-fact %+v, nothing-durable %+v", byID["tool-fact"], byID["nothing-durable"])
	}
	if byID["decision"].Passed || byID["injection"].Passed || byID["chinese"].Error == "" {
		t.Fatalf("decision %+v, injection %+v, chinese %+v", byID["decision"], byID["injection"], byID["chinese"])
	}
	s := report.Summary
	if s.Extractions != len(cases()) || s.Valid != s.Extractions-1 || s.Forbidden != 1 || s.EmptyCorrect != 1 || s.KindMatched != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if !strings.Contains(stdout.String(), "passed") || !strings.Contains(stdout.String(), "tool-fact") {
		t.Fatalf("report = %s", stdout.String())
	}
	only, _ := evaluate(context.Background(), options{trials: 2, maxCandidates: 5, only: "tool-fact", timeout: time.Minute}, scriptedExtractor{}, &bytes.Buffer{})
	if len(only.Cases) != 1 || len(only.Cases[0].Trials) != 2 {
		t.Fatalf("--cases = %+v", only.Cases)
	}
}
