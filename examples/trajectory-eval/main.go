// Command trajectory-eval grades the stored trajectories of host-api
// workflows. It lists workflows through GET /workflows, reads each one's
// content-free trace through GET /workflows/{id}/trace, grades them with
// evalkit's trajectory graders, and prints a JSON report.
//
// Exit codes: 0 when every trajectory passes, 1 when any fails, 2 on a usage,
// suite, or host error.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eruca/goagents/evalkit"
)

const maxResponseBytes = 8 << 20

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, http.DefaultClient, os.Stdout, os.Stderr))
}

type options struct {
	host      string
	token     string
	project   string
	status    string
	limit     int
	workflows []string
	suitePath string
}

func run(ctx context.Context, args []string, getenv func(string) string, client *http.Client, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, getenv, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "trajectory-eval:", err)
		return 2
	}
	suite, err := loadSuite(opts.suitePath)
	if err != nil {
		fmt.Fprintln(stderr, "trajectory-eval:", err)
		return 2
	}
	host := hostClient{base: strings.TrimRight(opts.host, "/"), token: opts.token, client: client}
	ids := opts.workflows
	if len(ids) == 0 {
		ids, err = host.listWorkflows(ctx, opts.project, opts.status, opts.limit)
		if err != nil {
			fmt.Fprintln(stderr, "trajectory-eval:", err)
			return 2
		}
	}
	if len(ids) == 0 {
		fmt.Fprintln(stderr, "trajectory-eval: no workflows matched")
		return 2
	}
	recorded := evalkit.RecordedHarness{}
	for _, id := range ids {
		result, err := host.trace(ctx, id)
		if err != nil {
			fmt.Fprintln(stderr, "trajectory-eval:", err)
			return 2
		}
		recorded[id] = result
		suite.Tasks = append(suite.Tasks, evalkit.Task{ID: id})
	}
	result, err := evalkit.Runner{Harness: recorded}.Run(ctx, suite)
	if err != nil {
		fmt.Fprintln(stderr, "trajectory-eval:", err)
		return 2
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(toReport(result)); err != nil {
		fmt.Fprintln(stderr, "trajectory-eval:", err)
		return 2
	}
	if result.Summary.FailedTrials > 0 {
		return 1
	}
	return 0
}

func parseOptions(args []string, getenv func(string) string, stderr io.Writer) (options, error) {
	var opts options
	var workflows string
	flags := flag.NewFlagSet("trajectory-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.host, "host", "", "host-api base URL, for example http://127.0.0.1:8080")
	flags.StringVar(&opts.project, "project", "", "project_id to list workflows from (required when the host enables memory)")
	flags.StringVar(&opts.status, "status", "", "only grade workflows in this status")
	flags.IntVar(&opts.limit, "limit", 50, "maximum number of workflows to grade (host caps it at 100)")
	flags.StringVar(&workflows, "workflows", "", "comma-separated workflow IDs to grade instead of listing")
	flags.StringVar(&opts.suitePath, "suite", "", "path to the suite JSON file")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected arguments %q", flags.Args())
	}
	if opts.host == "" || opts.suitePath == "" {
		return options{}, errors.New("--host and --suite are required")
	}
	if parsed, err := url.Parse(opts.host); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return options{}, fmt.Errorf("--host must be an http or https URL")
	}
	if opts.limit <= 0 {
		return options{}, errors.New("--limit must be positive")
	}
	for _, id := range strings.Split(workflows, ",") {
		if id = strings.TrimSpace(id); id != "" {
			opts.workflows = append(opts.workflows, id)
		}
	}
	// The bearer token comes from the environment so it never appears in
	// process listings or shell history.
	opts.token = strings.TrimSpace(getenv("TRAJECTORY_EVAL_TOKEN"))
	return opts, nil
}

// suiteFile is the JSON suite format. Only evalkit's trajectory graders are
// available; unknown fields and grader types are rejected.
type suiteFile struct {
	Name    string       `json:"name"`
	Graders []graderSpec `json:"graders"`
}

type graderSpec struct {
	Type          string   `json:"type"`
	Tools         []string `json:"tools,omitempty"`
	Statuses      []string `json:"statuses,omitempty"`
	StepTypes     []string `json:"step_types,omitempty"`
	MaxToolCalls  int      `json:"max_tool_calls,omitempty"`
	MaxModelCalls int      `json:"max_model_calls,omitempty"`
	MaxTokens     int      `json:"max_tokens,omitempty"`
	MaxDuration   string   `json:"max_duration,omitempty"`
}

func loadSuite(path string) (evalkit.Suite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return evalkit.Suite{}, fmt.Errorf("read suite: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file suiteFile
	if err := decoder.Decode(&file); err != nil {
		return evalkit.Suite{}, fmt.Errorf("decode suite: %w", err)
	}
	if decoder.More() {
		return evalkit.Suite{}, errors.New("decode suite: trailing data")
	}
	if strings.TrimSpace(file.Name) == "" {
		return evalkit.Suite{}, errors.New("suite name is required")
	}
	if len(file.Graders) == 0 {
		return evalkit.Suite{}, errors.New("suite needs at least one grader")
	}
	suite := evalkit.Suite{Name: file.Name}
	for i, spec := range file.Graders {
		grader, err := spec.grader()
		if err != nil {
			return evalkit.Suite{}, fmt.Errorf("grader %d: %w", i, err)
		}
		suite.Graders = append(suite.Graders, grader)
	}
	return suite, nil
}

func (s graderSpec) grader() (evalkit.Grader, error) {
	requireList := func(name string, values []string) error {
		if len(values) == 0 {
			return fmt.Errorf("%s needs %s", s.Type, name)
		}
		return nil
	}
	switch s.Type {
	case "require_tools":
		return evalkit.RequireTools(s.Tools...), requireList("tools", s.Tools)
	case "forbid_tools":
		return evalkit.ForbidTools(s.Tools...), requireList("tools", s.Tools)
	case "tool_order":
		return evalkit.ToolOrder(s.Tools...), requireList("tools", s.Tools)
	case "outcome_status":
		return evalkit.OutcomeStatus(s.Statuses...), requireList("statuses", s.Statuses)
	case "no_failed_steps":
		return evalkit.NoFailedSteps(s.StepTypes...), nil
	case "budget":
		budget := evalkit.Budget{MaxToolCalls: s.MaxToolCalls, MaxModelCalls: s.MaxModelCalls, MaxTokens: s.MaxTokens}
		if s.MaxDuration != "" {
			duration, err := time.ParseDuration(s.MaxDuration)
			if err != nil || duration <= 0 {
				return nil, fmt.Errorf("budget max_duration must be a positive duration such as \"2m\"")
			}
			budget.MaxDuration = duration
		}
		if budget == (evalkit.Budget{}) {
			return nil, errors.New("budget needs at least one limit")
		}
		if budget.MaxToolCalls < 0 || budget.MaxModelCalls < 0 || budget.MaxTokens < 0 {
			return nil, errors.New("budget limits must not be negative")
		}
		return budget.Grader(), nil
	default:
		return nil, fmt.Errorf("unknown grader type %q", s.Type)
	}
}

type hostClient struct {
	base   string
	token  string
	client *http.Client
}

func (h hostClient) get(ctx context.Context, path string, query url.Values, out any) error {
	target := h.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("GET %s: response exceeds %d bytes", path, maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		// host-api errors are {"error": "<code>", "message": ...}; only the
		// stable code is reported.
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &problem)
		return fmt.Errorf("GET %s: status %d %s", path, resp.StatusCode, problem.Error)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: decode: %w", path, err)
	}
	return nil
}

func (h hostClient) listWorkflows(ctx context.Context, project, status string, limit int) ([]string, error) {
	query := url.Values{"order": {"desc"}, "limit": {strconv.Itoa(limit)}}
	if project != "" {
		query.Set("project_id", project)
	}
	if status != "" {
		query.Set("status", status)
	}
	var list struct {
		Workflows []struct {
			ID string `json:"id"`
		} `json:"workflows"`
	}
	if err := h.get(ctx, "/workflows", query, &list); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list.Workflows))
	for _, workflow := range list.Workflows {
		ids = append(ids, workflow.ID)
	}
	return ids, nil
}

// traceResponse mirrors host-api's GET /workflows/{id}/trace response.
type traceResponse struct {
	Outcome struct {
		Status    string         `json:"status"`
		OutputRef string         `json:"output_ref"`
		ErrorCode string         `json:"error_code"`
		Metadata  map[string]any `json:"metadata"`
	} `json:"outcome"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		LLMCalls     int `json:"llm_calls"`
		ToolCalls    int `json:"tool_calls"`
	} `json:"usage"`
	Labels map[string]string `json:"labels"`
	Steps  []struct {
		Type      string         `json:"type"`
		Name      string         `json:"name"`
		Status    string         `json:"status"`
		StartedAt *time.Time     `json:"started_at"`
		EndedAt   *time.Time     `json:"ended_at"`
		Metadata  map[string]any `json:"metadata"`
	} `json:"steps"`
}

func (h hostClient) trace(ctx context.Context, workflowID string) (*evalkit.RunResult, error) {
	var response traceResponse
	if err := h.get(ctx, "/workflows/"+url.PathEscape(workflowID)+"/trace", nil, &response); err != nil {
		return nil, err
	}
	result := &evalkit.RunResult{
		Outcome: evalkit.Outcome{
			Status:    response.Outcome.Status,
			OutputRef: response.Outcome.OutputRef,
			ErrorCode: response.Outcome.ErrorCode,
			Metadata:  response.Outcome.Metadata,
		},
		Trace: evalkit.Trace{
			RunID:  workflowID,
			Labels: response.Labels,
			Usage: evalkit.Usage{
				InputTokens:  response.Usage.InputTokens,
				OutputTokens: response.Usage.OutputTokens,
				LLMCalls:     response.Usage.LLMCalls,
				ToolCalls:    response.Usage.ToolCalls,
			},
		},
	}
	for _, step := range response.Steps {
		traceStep := evalkit.TraceStep{Type: step.Type, Name: step.Name, Status: step.Status, Metadata: step.Metadata}
		if step.StartedAt != nil {
			traceStep.StartedAt = *step.StartedAt
		}
		if step.EndedAt != nil {
			traceStep.EndedAt = *step.EndedAt
		}
		result.Trace.Steps = append(result.Trace.Steps, traceStep)
	}
	return result, nil
}

type report struct {
	Suite        string        `json:"suite"`
	TotalTrials  int           `json:"total"`
	PassedTrials int           `json:"passed"`
	FailedTrials int           `json:"failed"`
	Workflows    []trialReport `json:"workflows"`
}

type trialReport struct {
	WorkflowID string        `json:"workflow_id"`
	Passed     bool          `json:"passed"`
	Error      string        `json:"error,omitempty"`
	Grades     []gradeReport `json:"grades,omitempty"`
}

type gradeReport struct {
	Name     string   `json:"name"`
	Passed   bool     `json:"passed"`
	Score    float64  `json:"score"`
	Message  string   `json:"message,omitempty"`
	Failures []string `json:"failures,omitempty"`
}

func toReport(result *evalkit.SuiteResult) report {
	out := report{
		Suite:        result.Name,
		TotalTrials:  result.Summary.TotalTrials,
		PassedTrials: result.Summary.PassedTrials,
		FailedTrials: result.Summary.FailedTrials,
		Workflows:    make([]trialReport, 0, len(result.Trials)),
	}
	for _, trial := range result.Trials {
		item := trialReport{WorkflowID: trial.Trial.TaskID, Passed: trial.Passed, Error: trial.Trial.Error}
		for _, grade := range trial.Grades {
			entry := gradeReport{Name: grade.Name, Passed: grade.Passed, Score: grade.Score, Message: grade.Message}
			for _, assertion := range grade.Assertions {
				if !assertion.Passed {
					entry.Failures = append(entry.Failures, strings.TrimSpace(assertion.Name+": "+assertion.Message))
				}
			}
			item.Grades = append(item.Grades, entry)
		}
		out.Workflows = append(out.Workflows, item)
	}
	return out
}
