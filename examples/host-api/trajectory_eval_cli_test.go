package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestTrajectoryEvalCLIGradesHostWorkflows builds examples/trajectory-eval
// and runs it against a live host-api handler, so the CLI's view of
// GET /workflows and GET /workflows/{id}/trace stays in step with the host.
func TestTrajectoryEvalCLIGradesHostWorkflows(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the trajectory-eval binary")
	}
	server, err := NewServer(Config{RuntimeHome: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		closeStoreIfPossible(t, server.workflows)
		closeStoreIfPossible(t, server.runs)
	})
	for _, id := range []string{"wf-cli-1", "wf-cli-2"} {
		doJSON[workflowResponse](t, server.Handler(), http.MethodPost, "/workflows", map[string]string{"id": id, "input": "review " + id})
	}
	host := httptest.NewServer(server.Handler())
	defer host.Close()

	binary := filepath.Join(t.TempDir(), "trajectory-eval")
	build := exec.Command("go", "build", "-o", binary, "../trajectory-eval")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build trajectory-eval: %v\n%s", err, output)
	}
	writeSuite := func(suite string) string {
		path := filepath.Join(t.TempDir(), "suite.json")
		if err := os.WriteFile(path, []byte(suite), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	runCLI := func(suite string) (int, map[string]any) {
		command := exec.Command(binary, "--host", host.URL, "--suite", writeSuite(suite), "--status", "waiting_approval")
		output, err := command.Output()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("run trajectory-eval: %v", err)
		}
		var report map[string]any
		if err := json.Unmarshal(output, &report); err != nil {
			t.Fatalf("decode report: %v\n%s", err, output)
		}
		return code, report
	}

	passing := `{"name":"host-pass","graders":[
		{"type":"outcome_status","statuses":["waiting_approval"]},
		{"type":"no_failed_steps"},
		{"type":"budget","max_model_calls":1,"max_tool_calls":0}
	]}`
	if code, report := runCLI(passing); code != 0 || report["passed"] != float64(2) {
		t.Fatalf("passing suite = %d %v", code, report)
	}
	failing := `{"name":"host-fail","graders":[{"type":"outcome_status","statuses":["succeeded"]}]}`
	if code, report := runCLI(failing); code != 1 || report["failed"] != float64(2) {
		t.Fatalf("failing suite = %d %v", code, report)
	}
}
