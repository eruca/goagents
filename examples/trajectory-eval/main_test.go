package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodTrace = `{
  "workflow_id": "wf-good",
  "outcome": {"status": "succeeded", "output_ref": "artifact:wf-good:out"},
  "usage": {"input_tokens": 10, "output_tokens": 5, "llm_calls": 1, "tool_calls": 2},
  "steps": [
    {"type": "workflow_step", "name": "review", "status": "succeeded", "started_at": "2026-11-19T08:00:00Z", "ended_at": "2026-11-19T08:00:30Z"},
    {"type": "model_call", "name": "fast", "status": "succeeded", "started_at": "2026-11-19T08:00:01Z"},
    {"type": "tool_call", "name": "search", "status": "succeeded", "started_at": "2026-11-19T08:00:02Z"},
    {"type": "tool_call", "name": "write", "status": "succeeded", "started_at": "2026-11-19T08:00:03Z"}
  ]
}`

const badTrace = `{
  "workflow_id": "wf-bad",
  "outcome": {"status": "succeeded"},
  "usage": {"llm_calls": 3, "tool_calls": 1},
  "steps": [{"type": "tool_call", "name": "delete", "status": "succeeded"}]
}`

type fakeHost struct {
	traces   map[string]string
	requests []string
	auth     []string
}

func (h *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.requests = append(h.requests, r.URL.RequestURI())
	h.auth = append(h.auth, r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/workflows" {
		ids := []string{}
		for id := range h.traces {
			ids = append(ids, `{"id":"`+id+`"}`)
		}
		_, _ = w.Write([]byte(`{"workflows":[` + strings.Join(ids, ",") + `]}`))
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/workflows/"), "/trace")
	trace, ok := h.traces[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"workflow_not_found","message":"not found"}`))
		return
	}
	_, _ = w.Write([]byte(trace))
}

func writeSuite(t *testing.T, suite string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "suite.json")
	if err := os.WriteFile(path, []byte(suite), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const suiteJSON = `{
  "name": "prod-regression",
  "graders": [
    {"type": "outcome_status", "statuses": ["succeeded"]},
    {"type": "forbid_tools", "tools": ["delete"]},
    {"type": "tool_order", "tools": ["search", "write"]},
    {"type": "no_failed_steps"},
    {"type": "budget", "max_model_calls": 2, "max_duration": "1m"}
  ]
}`

func runCLI(t *testing.T, server *httptest.Server, env map[string]string, args ...string) (int, report, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, func(key string) string { return env[key] }, server.Client(), &stdout, &stderr)
	var out report
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("decode report: %v\n%s", err, stdout.String())
		}
	}
	return code, out, stderr.String()
}

func TestGradesSelectedWorkflows(t *testing.T) {
	host := &fakeHost{traces: map[string]string{"wf-good": goodTrace, "wf-bad": badTrace}}
	server := httptest.NewServer(host)
	defer server.Close()
	suite := writeSuite(t, suiteJSON)

	code, out, stderr := runCLI(t, server, map[string]string{"TRAJECTORY_EVAL_TOKEN": "secret"}, "--host", server.URL, "--suite", suite, "--workflows", "wf-good")
	if code != 0 || out.PassedTrials != 1 || out.Workflows[0].WorkflowID != "wf-good" {
		t.Fatalf("good run = %d %+v %s", code, out, stderr)
	}
	if host.auth[0] != "Bearer secret" {
		t.Fatalf("authorization = %q", host.auth[0])
	}

	code, out, _ = runCLI(t, server, nil, "--host", server.URL, "--suite", suite, "--workflows", "wf-good, wf-bad")
	if code != 1 || out.FailedTrials != 1 {
		t.Fatalf("mixed run = %d %+v", code, out)
	}
	bad := out.Workflows[1]
	failures := []string{}
	for _, grade := range bad.Grades {
		failures = append(failures, grade.Failures...)
	}
	joined := strings.Join(failures, "\n")
	for _, want := range []string{`tool "delete" was called 1 time(s)`, "model calls: used 3, limit 2", "next expected \"search\""} {
		if !strings.Contains(joined, want) {
			t.Fatalf("failures missing %q:\n%s", want, joined)
		}
	}
}

func TestListsWorkflowsWithFilters(t *testing.T) {
	host := &fakeHost{traces: map[string]string{"wf-good": goodTrace}}
	server := httptest.NewServer(host)
	defer server.Close()
	code, out, stderr := runCLI(t, server, nil, "--host", server.URL, "--suite", writeSuite(t, suiteJSON), "--project", "p-1", "--status", "succeeded", "--limit", "7")
	if code != 0 || out.TotalTrials != 1 {
		t.Fatalf("listed run = %d %+v %s", code, out, stderr)
	}
	if got := host.requests[0]; got != "/workflows?limit=7&order=desc&project_id=p-1&status=succeeded" {
		t.Fatalf("list request = %s", got)
	}
}

func TestErrorsExitWithTwo(t *testing.T) {
	host := &fakeHost{traces: map[string]string{}}
	server := httptest.NewServer(host)
	defer server.Close()
	suite := writeSuite(t, suiteJSON)
	cases := map[string][]string{
		"missing host":     {"--suite", suite},
		"bad host":         {"--host", "ftp://x", "--suite", suite},
		"no workflows":     {"--host", server.URL, "--suite", suite},
		"missing workflow": {"--host", server.URL, "--suite", suite, "--workflows", "wf-missing"},
		"bad limit":        {"--host", server.URL, "--suite", suite, "--limit", "0"},
		"extra args":       {"--host", server.URL, "--suite", suite, "extra"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if code, _, stderr := runCLI(t, server, nil, args...); code != 2 || stderr == "" {
				t.Fatalf("code = %d, stderr = %q", code, stderr)
			}
		})
	}
	if _, _, stderr := runCLI(t, server, nil, "--host", server.URL, "--suite", suite, "--workflows", "wf-missing"); !strings.Contains(stderr, "status 404 workflow_not_found") {
		t.Fatalf("missing workflow stderr = %q", stderr)
	}
}

func TestSuiteValidation(t *testing.T) {
	for name, suite := range map[string]string{
		"unknown field":     `{"name":"s","graders":[{"type":"budget","max_tokens":1,"oops":1}]}`,
		"unknown type":      `{"name":"s","graders":[{"type":"llm_judge"}]}`,
		"no graders":        `{"name":"s","graders":[]}`,
		"no name":           `{"graders":[{"type":"no_failed_steps"}]}`,
		"tools missing":     `{"name":"s","graders":[{"type":"require_tools"}]}`,
		"empty budget":      `{"name":"s","graders":[{"type":"budget"}]}`,
		"negative budget":   `{"name":"s","graders":[{"type":"budget","max_tokens":-1}]}`,
		"bad duration":      `{"name":"s","graders":[{"type":"budget","max_duration":"soon"}]}`,
		"trailing document": `{"name":"s","graders":[{"type":"no_failed_steps"}]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadSuite(writeSuite(t, suite)); err == nil {
				t.Fatal("suite accepted")
			}
		})
	}
}
