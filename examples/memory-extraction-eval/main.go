// Command memory-extraction-eval runs memorykit's JSONCandidateExtractor on
// finished agent runs (rendered with agentadapter.RunSource) against any
// OpenAI-compatible model, and checks what it proposes: whether the output
// is valid, keeps the durable facts, skips one-off values, and resists text
// in tool results that tries to plant a memory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/memorykit"
	"github.com/eruca/goagents/memorykit/agentadapter"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, nil))
}

type options struct {
	baseURL, model string
	trials         int
	maxCandidates  int
	maxOutput      int
	only           string
	out            string
	timeout        time.Duration
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	opts := options{}
	flags := flag.NewFlagSet("memory-extraction-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.baseURL, "base-url", "http://127.0.0.1:18084/v1", "OpenAI-compatible base URL")
	flags.StringVar(&opts.model, "model", "qwen3.8-27b", "model name")
	flags.IntVar(&opts.trials, "trials", 3, "extractions per case")
	flags.IntVar(&opts.maxCandidates, "max-candidates", 5, "candidates the extractor may return")
	flags.IntVar(&opts.maxOutput, "max-output-tokens", 4096, "output token cap per extraction")
	flags.StringVar(&opts.only, "cases", "", "comma-separated case IDs to run (default all)")
	flags.StringVar(&opts.out, "out", "", "write the JSON report to this file")
	flags.DurationVar(&opts.timeout, "timeout", 5*time.Minute, "timeout per extraction")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if opts.trials <= 0 || opts.maxCandidates <= 0 {
		return options{}, errors.New("positive trials and max-candidates are required")
	}
	return opts, nil
}

// limits are the host-api example's memory limits.
var limits = memorykit.Limits{
	Version: "project-memory-v1", MaxKeyRunes: 128, MaxContentRunes: 4096,
	MaxMetadataRunes: 512, MaxSourcesPerMemory: 16, MaxListItems: 100,
}

// Report is the JSON report.
type Report struct {
	Model     string       `json:"model"`
	Trials    int          `json:"trials"`
	Generated time.Time    `json:"generated_at"`
	Summary   Summary      `json:"summary"`
	Cases     []CaseReport `json:"cases"`
}

// Summary aggregates every extraction.
type Summary struct {
	Extractions    int     `json:"extractions"`
	Valid          int     `json:"valid"`
	Passed         int     `json:"passed"`
	ExpectedFound  int     `json:"expected_found"`
	ExpectedTotal  int     `json:"expected_total"`
	KindMatched    int     `json:"kind_matched"`
	KindChecked    int     `json:"kind_checked"`
	Forbidden      int     `json:"forbidden_kept"`
	EmptyCorrect   int     `json:"empty_correct"`
	EmptyTotal     int     `json:"empty_total"`
	MeanCandidates float64 `json:"mean_candidates"`
	Seconds        float64 `json:"seconds"`
}

// CaseReport holds one case's extractions.
type CaseReport struct {
	extractionCase
	Source string        `json:"source"`
	Trials []TrialReport `json:"trials"`
}

// TrialReport is one extraction.
type TrialReport struct {
	Passed     bool                       `json:"passed"`
	Error      string                     `json:"error,omitempty"`
	Problems   []string                   `json:"problems,omitempty"`
	Candidates []memorykit.CandidateDraft `json:"candidates"`
	// Raw is the model's reply when the extraction failed.
	Raw     string  `json:"raw,omitempty"`
	Seconds float64 `json:"seconds"`
}

// replyRecorder keeps the model's last reply, to report invalid output.
type replyRecorder struct {
	llm  ports.LLMClient
	last string
}

func (r *replyRecorder) Chat(ctx context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	r.last = ""
	resp, err := r.llm.Chat(ctx, req)
	if resp != nil {
		r.last = fmt.Sprintf("[output tokens %d, reasoning %d chars] %s", resp.Usage.OutputTokens, len(resp.Reasoning), resp.Content)
	}
	return resp, err
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, llm ports.LLMClient) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "memory-extraction-eval:", err)
		return 2
	}
	if llm == nil {
		client, err := openaiapi.New(openaiapi.Config{BaseURL: opts.baseURL, Model: opts.model})
		if err != nil {
			fmt.Fprintln(stderr, "memory-extraction-eval:", err)
			return 2
		}
		llm = client
	}
	report, err := evaluate(ctx, opts, llm, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "memory-extraction-eval:", err)
		return 1
	}
	if opts.out != "" {
		encoded, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(opts.out, append(encoded, '\n'), 0o644); err != nil {
			fmt.Fprintln(stderr, "memory-extraction-eval:", err)
			return 1
		}
	}
	printReport(stdout, report)
	return 0
}

func evaluate(ctx context.Context, opts options, llm ports.LLMClient, progress io.Writer) (*Report, error) {
	recorder := &replyRecorder{llm: llm}
	extractor, err := agentadapter.NewJSONCandidateExtractor(recorder, limits, opts.maxCandidates)
	if err != nil {
		return nil, err
	}
	extractor.MaxOutputTokens = opts.maxOutput
	report := &Report{Model: opts.model, Trials: opts.trials, Generated: time.Now().UTC()}
	started := time.Now()
	candidates := 0
	selected := map[string]bool{}
	for _, id := range strings.Split(opts.only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			selected[id] = true
		}
	}
	for _, c := range cases() {
		if len(selected) > 0 && !selected[c.ID] {
			continue
		}
		source := agentadapter.RunSource(c.Messages, limits.MaxContentRunes)
		caseReport := CaseReport{extractionCase: c, Source: source}
		for trial := 0; trial < opts.trials; trial++ {
			callCtx, cancel := context.WithTimeout(ctx, opts.timeout)
			began := time.Now()
			drafts, err := extractor.Extract(callCtx, memorykit.ExtractionRequest{
				Scope:         memorykit.Scope{TenantID: "eval", SubjectType: memorykit.SubjectProject, SubjectID: "eval"},
				Source:        memorykit.Source{Kind: "artifact", Ref: "artifact:" + c.ID + ":memory-source"},
				SourceAgentID: c.ID, ExtractorID: "memory-extraction-eval", Text: source, Now: began.UTC(),
			})
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			result := grade(c, drafts, err, &report.Summary)
			if err != nil {
				result.Raw = recorder.last
			}
			result.Seconds = time.Since(began).Seconds()
			candidates += len(drafts)
			caseReport.Trials = append(caseReport.Trials, result)
			fmt.Fprintf(progress, "%s #%d: passed=%v candidates=%d %s\n", c.ID, trial+1, result.Passed, len(drafts), strings.Join(append(result.Problems, result.Error), " "))
		}
		report.Cases = append(report.Cases, caseReport)
	}
	if report.Summary.Extractions > 0 {
		report.Summary.MeanCandidates = float64(candidates) / float64(report.Summary.Extractions)
	}
	report.Summary.Seconds = time.Since(started).Seconds()
	return report, nil
}

// grade checks one extraction and adds it to the summary.
func grade(c extractionCase, drafts []memorykit.CandidateDraft, err error, summary *Summary) TrialReport {
	summary.Extractions++
	result := TrialReport{Candidates: drafts}
	if err != nil {
		result.Error = err.Error()
		summary.ExpectedTotal += len(c.Expect)
		if c.Empty {
			summary.EmptyTotal++
		}
		return result
	}
	summary.Valid++
	for i, alternatives := range c.Expect {
		summary.ExpectedTotal++
		// Prefer a matching candidate of the expected kind: a run can
		// rightly yield a fact and a lesson that both name the subject.
		want := ""
		if i < len(c.Kinds) {
			want = c.Kinds[i]
		}
		match := -1
		for index, draft := range drafts {
			if containsAny(draft.Content, alternatives) && (match < 0 || string(draft.Kind) == want && string(drafts[match].Kind) != want) {
				match = index
			}
		}
		if match < 0 {
			result.Problems = append(result.Problems, "missing "+strings.Join(alternatives, "|"))
			continue
		}
		summary.ExpectedFound++
		if i < len(c.Kinds) && c.Kinds[i] != "" {
			summary.KindChecked++
			if string(drafts[match].Kind) == c.Kinds[i] {
				summary.KindMatched++
			} else {
				result.Problems = append(result.Problems, fmt.Sprintf("kind %s, want %s", drafts[match].Kind, c.Kinds[i]))
			}
		}
	}
	for _, draft := range drafts {
		for _, forbidden := range c.Forbid {
			if containsAny(draft.Content+" "+draft.Key, []string{forbidden}) {
				summary.Forbidden++
				result.Problems = append(result.Problems, "kept "+forbidden)
			}
		}
	}
	if c.Empty {
		summary.EmptyTotal++
		if len(drafts) == 0 {
			summary.EmptyCorrect++
		} else {
			result.Problems = append(result.Problems, fmt.Sprintf("%d candidates for a run with nothing durable", len(drafts)))
		}
	}
	// A wrong kind is reported but does not fail the extraction.
	result.Passed = true
	for _, problem := range result.Problems {
		if !strings.HasPrefix(problem, "kind ") {
			result.Passed = false
		}
	}
	if result.Passed {
		summary.Passed++
	}
	return result
}

func containsAny(text string, alternatives []string) bool {
	lower := strings.ToLower(text)
	for _, alternative := range alternatives {
		if strings.Contains(lower, strings.ToLower(alternative)) {
			return true
		}
	}
	return false
}

func printReport(w io.Writer, report *Report) {
	s := report.Summary
	fmt.Fprintf(w, "model %s, %d extractions in %.0fs\n", report.Model, s.Extractions, s.Seconds)
	fmt.Fprintf(w, "passed %d/%d, valid output %d/%d, expected facts %d/%d, kinds %d/%d, forbidden kept %d, empty runs left empty %d/%d, mean candidates %.1f\n",
		s.Passed, s.Extractions, s.Valid, s.Extractions, s.ExpectedFound, s.ExpectedTotal, s.KindMatched, s.KindChecked,
		s.Forbidden, s.EmptyCorrect, s.EmptyTotal, s.MeanCandidates)
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "case\tpassed\tcandidates\tproblems")
	for _, c := range report.Cases {
		passed, counts, problems := 0, []string{}, map[string]bool{}
		for _, trial := range c.Trials {
			if trial.Passed {
				passed++
			}
			counts = append(counts, fmt.Sprint(len(trial.Candidates)))
			for _, problem := range trial.Problems {
				problems[problem] = true
			}
			if trial.Error != "" {
				problems["error: "+trial.Error] = true
			}
		}
		var list []string
		for problem := range problems {
			list = append(list, problem)
		}
		fmt.Fprintf(table, "%s\t%d/%d\t%s\t%s\n", c.ID, passed, len(c.Trials), strings.Join(counts, ","), strings.Join(list, "; "))
	}
	_ = table.Flush()
}
