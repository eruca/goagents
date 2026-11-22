// Command icu-handoff writes an ICU shift handoff from a gocms server.
//
//	icu-handoff run   --gocms URL --ward W --out DIR [--since prev_shift] [--concurrency 2] [--reasoning-effort none] [--as-of T] [--dry-run]
//	icu-handoff check --gocms URL --patients S1ab,S2cd --out DIR [--as-of T]
//
// run lists the ward's patients and prefetches their overviews over MCP, then
// gives every patient one fresh model run with the gocms tools pinned to that
// patient, checks each point's references and numbers against what the tools
// returned, and writes handoff.md, handoff.json and stats.json. check writes
// the overview checklist for manual verification.
//
// Credentials come only from the environment: GOCMS_AGENT_TOKEN, and for run
// without --dry-run OPENAI_COMPAT_BASE_URL, OPENAI_COMPAT_API_KEY and
// OPENAI_COMPAT_MODEL. stats.json never contains patient content.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/eruca/goagents/examples/icu-handoff/handoff"
	"github.com/eruca/goagents/goagent/extensions/providers/openaiapi"
	"github.com/eruca/goagents/goagent/ports"
)

//go:embed skills/icu-handoff/SKILL.md
var embeddedSkill string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// env is the process environment; tests replace it.
var env = os.Getenv

// newLLM builds the model client; tests replace it. reasoningEffort is sent
// as reasoning_effort unless empty.
var newLLM = func(reasoningEffort string) (ports.LLMClient, error) {
	cfg := openaiapi.Config{
		BaseURL:         env("OPENAI_COMPAT_BASE_URL"),
		APIKey:          env("OPENAI_COMPAT_API_KEY"),
		Model:           env("OPENAI_COMPAT_MODEL"),
		ReasoningEffort: reasoningEffort,
	}
	if cfg.BaseURL == "" || cfg.Model == "" {
		return nil, errors.New("OPENAI_COMPAT_BASE_URL and OPENAI_COMPAT_MODEL are required (or use --dry-run)")
	}
	return openaiapi.New(cfg)
}

const usage = `usage:
  icu-handoff run   --gocms URL --ward WARD --out DIR [flags]
  icu-handoff check --gocms URL --patients S1ab,S2cd --out DIR [flags]
run "icu-handoff run -h" or "icu-handoff check -h" for flags`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = runHandoff(ctx, args[1:], stdout, stderr)
	case "check":
		err = runCheck(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
	var usageErr usageError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &usageErr):
		fmt.Fprintln(stderr, "icu-handoff:", err)
		return 2
	default:
		fmt.Fprintln(stderr, "icu-handoff:", err)
		return 1
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func runHandoff(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("icu-handoff run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	gocms := flags.String("gocms", "", "gocms base URL, e.g. http://127.0.0.1:9002 (required)")
	ward := flags.String("ward", "", "ward code or name (required)")
	out := flags.String("out", "", "output directory (required)")
	since := flags.String("since", "prev_shift", `shift start for the overview "since" line: a time, "shift" or "prev_shift"`)
	asOf := flags.String("as-of", "", "pin gocms' \"now\" in the past (RFC 3339 or YYYY-MM-DD HH:mm)")
	concurrency := flags.Int("concurrency", 2, "patients generated at the same time (model slots)")
	overviewConcurrency := flags.Int("overview-concurrency", 4, "overview prefetches at the same time")
	changesHours := flags.Int("changes-hours", 24, "window for admissions and discharges (0-72)")
	budget := flags.Int("budget", 1000, "overview character budget")
	maxToolCalls := flags.Int("max-tool-calls", 3, "tool calls per patient run")
	maxOutputTokens := flags.Int("max-output-tokens", 4096, "model output token limit per call, reasoning included")
	patientTimeout := flags.Duration("patient-timeout", 5*time.Minute, "time limit per patient")
	limit := flags.Int("limit", 0, "only the first N patients by bed (0 = all)")
	reasoningEffort := flags.String("reasoning-effort", "none", `model reasoning_effort: none, minimal, low, medium or high ("" = not sent)`)
	toolSpec := flags.String("tool-spec", handoff.ToolSpecCompact, "tool descriptions the model sees: compact or full")
	skillPath := flags.String("skill", "", "SKILL.md to use instead of the embedded one")
	dryRun := flags.Bool("dry-run", false, "prefetch only; report what would be sent to the model without calling it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return usageError{fmt.Sprintf("unexpected arguments %q", flags.Args())}
	}
	if *gocms == "" || *ward == "" || *out == "" {
		return usageError{"--gocms, --ward and --out are required"}
	}
	switch *reasoningEffort {
	case "", "none", "minimal", "low", "medium", "high":
	default:
		return usageError{fmt.Sprintf("--reasoning-effort %q: want none, minimal, low, medium or high", *reasoningEffort)}
	}
	if *toolSpec != handoff.ToolSpecCompact && *toolSpec != handoff.ToolSpecFull {
		return usageError{fmt.Sprintf("--tool-spec %q: want compact or full", *toolSpec)}
	}
	token := env("GOCMS_AGENT_TOKEN")
	if token == "" {
		return usageError{"GOCMS_AGENT_TOKEN is required"}
	}
	skill, err := loadSkill(*skillPath)
	if err != nil {
		return err
	}
	var llm ports.LLMClient
	if !*dryRun {
		if llm, err = newLLM(*reasoningEffort); err != nil {
			return err
		}
	}

	client := handoff.NewGocms(*gocms, token, 30*time.Second)
	handoffID := "handoff-" + time.Now().Format("20060102T150405")
	mcp, err := client.ConnectMCP(handoff.WithRunID(ctx, handoffID+"-connect"))
	if err != nil {
		return fmt.Errorf("connect gocms mcp: %w", err)
	}
	defer mcp.Close()

	result, err := handoff.Run(ctx, handoff.Config{
		MCP:                 mcp,
		Expander:            client,
		LLM:                 llm,
		Skill:               skill,
		Ward:                *ward,
		Since:               *since,
		AsOf:                *asOf,
		ChangesHours:        *changesHours,
		OverviewBudget:      *budget,
		OverviewConcurrency: *overviewConcurrency,
		Concurrency:         *concurrency,
		MaxToolCalls:        *maxToolCalls,
		MaxOutputTokens:     *maxOutputTokens,
		PatientTimeout:      *patientTimeout,
		Limit:               *limit,
		ReasoningEffort:     *reasoningEffort,
		ToolSpec:            *toolSpec,
		HandoffID:           handoffID,
		DryRun:              *dryRun,
		// Log lines carry positions and error codes only.
		Logf: func(format string, args ...any) {
			fmt.Fprintf(stderr, "icu-handoff: "+format+"\n", args...)
		},
	})
	if err != nil {
		return err
	}
	if err := handoff.Write(*out, result); err != nil {
		return err
	}
	s := result.Stats
	fmt.Fprintf(stdout, "%s: %d patients (ok %d, failed %d, overview failed %d, dry run %d); points %d, unverified %d; %d ms\n",
		s.Mode, s.Patients, s.Runs.OK, s.Runs.Failed, s.Runs.OverviewFailed, s.Runs.DryRun,
		s.Check.Points, s.Check.Unverified, s.Durations.TotalMS)
	if len(s.Errors) > 0 {
		fmt.Fprintf(stderr, "icu-handoff: errors by code: %s\n", formatCounts(s.Errors))
	}
	fmt.Fprintf(stdout, "wrote %s/{%s,%s,%s}\n", strings.TrimRight(*out, "/"), handoff.MarkdownFile, handoff.JSONFile, handoff.StatsFile)
	return nil
}

func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("icu-handoff check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	gocms := flags.String("gocms", "", "gocms base URL (required)")
	patients := flags.String("patients", "", "comma-separated S references (required)")
	asOf := flags.String("as-of", "", "pin gocms' \"now\"; use a fixed time on a database with sync stopped")
	out := flags.String("out", "", "output directory (required)")
	budget := flags.Int("budget", 1000, "overview character budget")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return usageError{fmt.Sprintf("unexpected arguments %q", flags.Args())}
	}
	var stays []string
	for _, s := range strings.Split(*patients, ",") {
		if s = strings.TrimSpace(s); s != "" {
			stays = append(stays, s)
		}
	}
	if *gocms == "" || *out == "" || len(stays) == 0 {
		return usageError{"--gocms, --patients and --out are required"}
	}
	for _, s := range stays {
		if !handoff.IsRef(s) || s[0] != 'S' {
			return usageError{"--patients takes S references (for example S4821id)"}
		}
	}
	token := env("GOCMS_AGENT_TOKEN")
	if token == "" {
		return usageError{"GOCMS_AGENT_TOKEN is required"}
	}
	client := handoff.NewGocms(*gocms, token, 30*time.Second)
	rows, stats, err := handoff.Checklist(ctx, handoff.ChecklistConfig{
		Gocms: client, Patients: stays, AsOf: *asOf, Budget: *budget,
	})
	if err != nil {
		return err
	}
	if err := handoff.WriteChecklist(*out, rows, stats); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "check: %d patients (failed %d), %d rows (%d with ref, %d aggregates); %d ms\n",
		stats.Patients, stats.Failed, stats.Rows, stats.WithRef, stats.Aggregates, stats.TotalMS)
	fmt.Fprintf(stdout, "wrote %s/{checklist.md,checklist.csv,%s}\n", strings.TrimRight(*out, "/"), handoff.StatsFile)
	if stats.Failed == stats.Patients {
		return errors.New("no patient could be fetched")
	}
	return nil
}

// formatCounts renders code counts as "a=1 b=2" in code order.
func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	return strings.Join(parts, " ")
}

// loadSkill returns the SKILL.md body without its YAML front matter.
func loadSkill(path string) (string, error) {
	text := embeddedSkill
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		text = string(raw)
	}
	return stripFrontMatter(text), nil
}

func stripFrontMatter(text string) string {
	if !strings.HasPrefix(text, "---\n") {
		return strings.TrimSpace(text)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return strings.TrimSpace(text)
	}
	rest = rest[end+len("\n---"):]
	return strings.TrimSpace(strings.TrimPrefix(rest, "\n"))
}
