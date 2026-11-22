package handoff

import (
	"sort"
	"strings"
)

// Stats is stats.json: counts, durations, verification rates and error
// codes only. It must never hold patient text, beds or references, because
// it is the one output read during development against real data. Every
// field is a number, a fixed code or a configuration value the caller chose.
type Stats struct {
	Version   string `json:"version"`
	Mode      string `json:"mode"`
	Census    int    `json:"census"`
	Patients  int    `json:"patients"`
	SyncStale bool   `json:"sync_stale"`

	Config StatsConfig `json:"config"`

	ToolsOffered int        `json:"tools_offered"`
	Input        InputStats `json:"input"`
	Runs         RunStats   `json:"runs"`
	LLM          LLMStats   `json:"llm"`
	Tools        ToolStats  `json:"tools"`
	Check        CheckStats `json:"check"`
	// Errors counts patients by error code (fixed codes such as
	// "output_invalid", "timeout", "tool_not_found").
	Errors    map[string]int `json:"errors"`
	Durations DurationStats  `json:"durations"`
}

// StatsConfig echoes the knobs that shape speed and cost.
type StatsConfig struct {
	Concurrency         int `json:"concurrency"`
	OverviewConcurrency int `json:"overview_concurrency"`
	OverviewBudget      int `json:"overview_budget"`
	MaxToolCalls        int `json:"max_tool_calls"`
	MaxOutputTokens     int `json:"max_output_tokens"`
	ChangesHours        int `json:"changes_hours"`
}

// InputStats sizes what the model reads, in characters.
type InputStats struct {
	SystemChars   int      `json:"system_chars"`
	ToolSpecChars int      `json:"tool_spec_chars"`
	Overview      Summary5 `json:"overview_chars"`
	UserMessage   Summary5 `json:"user_message_chars"`
	// EvidenceChars is all tool text per patient, including tools the model
	// called.
	Evidence Summary5 `json:"evidence_chars"`
}

// Summary5 is a min / median / p95 / max / total over patients.
type Summary5 struct {
	N      int `json:"n"`
	Min    int `json:"min"`
	Median int `json:"median"`
	P95    int `json:"p95"`
	Max    int `json:"max"`
	Total  int `json:"total"`
}

// RunStats counts patient outcomes.
type RunStats struct {
	OK              int `json:"ok"`
	Failed          int `json:"failed"`
	OverviewFailed  int `json:"overview_failed"`
	DryRun          int `json:"dry_run"`
	SchemaRetries   int `json:"schema_retries"`
	Rewrites        int `json:"rewrites"`
	PatientMismatch int `json:"patient_mismatch"`
	// LLMCallsPerPatient summarises model calls per generated patient.
	LLMCallsPerPatient Summary5 `json:"llm_calls_per_patient"`
}

// LLMStats totals model use.
type LLMStats struct {
	Calls           int   `json:"calls"`
	Failures        int   `json:"failures"`
	InputTokens     int   `json:"input_tokens"`
	OutputTokens    int   `json:"output_tokens"`
	ReasoningTokens int   `json:"reasoning_tokens"`
	TotalMS         int64 `json:"total_ms"`
}

// ToolStats counts the model's gocms tool calls.
type ToolStats struct {
	Calls     int            `json:"calls"`
	ByTool    map[string]int `json:"by_tool"`
	LimitHits int            `json:"limit_hits"`
	Errors    int            `json:"errors"`
}

// CheckStats summarises verification over all points.
type CheckStats struct {
	Points     int            `json:"points"`
	Verified   int            `json:"verified"`
	Unverified int            `json:"unverified"`
	PassRate   float64        `json:"pass_rate"`
	Refs       int            `json:"refs"`
	Numbers    int            `json:"numbers"`
	ByIssue    map[string]int `json:"by_issue"`
	// RefErrors counts rejected references by gocms error code.
	RefErrors map[string]int `json:"ref_errors"`
	// PatientsWithUnverified counts patients with at least one unverified point.
	PatientsWithUnverified int `json:"patients_with_unverified"`
	Watch                  int `json:"watch"`
	Uncertain              int `json:"uncertain"`
}

// DurationStats are wall-clock milliseconds.
type DurationStats struct {
	TotalMS              int64    `json:"total_ms"`
	PatientsMS           int64    `json:"patients_ms"`
	PrefetchMS           int64    `json:"prefetch_ms"`
	GeneratePerPatientMS Summary5 `json:"generate_per_patient_ms"`
}

func newStats(cfg Config) *Stats {
	mode := "run"
	if cfg.DryRun {
		mode = "dry_run"
	}
	return &Stats{
		Version: Version,
		Mode:    mode,
		Config: StatsConfig{
			Concurrency:         cfg.Concurrency,
			OverviewConcurrency: cfg.OverviewConcurrency,
			OverviewBudget:      cfg.OverviewBudget,
			MaxToolCalls:        cfg.MaxToolCalls,
			MaxOutputTokens:     cfg.MaxOutputTokens,
			ChangesHours:        cfg.ChangesHours,
		},
		Errors: map[string]int{},
	}
}

func (s *Stats) collect(r *Result, counters *toolCounters, systemChars, specChars int) {
	s.Census = r.Census
	s.Patients = len(r.Patients)
	s.SyncStale = r.Sync.Stale
	s.Input.SystemChars = systemChars
	s.Input.ToolSpecChars = specChars

	counters.mu.Lock()
	s.Tools.ByTool = map[string]int{}
	for name, n := range counters.byTool {
		s.Tools.ByTool[name] = n
		s.Tools.Calls += n
	}
	s.Tools.LimitHits = counters.limitHits
	s.Tools.Errors = counters.errors
	counters.mu.Unlock()

	s.Check.ByIssue = map[string]int{}
	s.Check.RefErrors = map[string]int{}
	var overview, user, evidence, generate, llmCalls []int
	for _, p := range r.Patients {
		switch p.Status {
		case StatusOK:
			s.Runs.OK++
		case StatusFailed:
			s.Runs.Failed++
		case StatusOverviewFailed:
			s.Runs.OverviewFailed++
		case StatusDryRun:
			s.Runs.DryRun++
		}
		if p.ErrorCode != "" {
			s.Errors[p.ErrorCode]++
		}
		s.Runs.SchemaRetries += p.SchemaRetries
		if p.Rewritten {
			s.Runs.Rewrites++
		}
		if p.PatientMismatch {
			s.Runs.PatientMismatch++
		}
		if p.Status == StatusOverviewFailed {
			continue
		}
		overview = append(overview, runeLen(p.overview))
		user = append(user, p.inputChars)
		if p.evidence != nil {
			evidence = append(evidence, p.evidence.Chars())
		}
		if p.Status == StatusOK || p.Status == StatusFailed {
			generate = append(generate, int(p.generateMS))
			llmCalls = append(llmCalls, p.llmCalls)
		}
		if p.Summary != nil {
			s.Check.Watch += len(p.Summary.Watch)
			s.Check.Uncertain += len(p.Summary.Uncertain)
		}
		bad := 0
		for _, c := range p.Checks {
			s.Check.Points++
			s.Check.Refs += c.Refs
			s.Check.Numbers += c.Numbers
			if c.Verified {
				s.Check.Verified++
			} else {
				s.Check.Unverified++
				bad++
			}
			for _, issue := range c.Issues {
				s.Check.ByIssue[issue]++
			}
			for _, issue := range c.BadRefs {
				if code, ok := strings.CutPrefix(issue, IssueRefRejected+":"); ok {
					s.Check.RefErrors[sanitizeID(code)]++
				}
			}
		}
		if bad > 0 {
			s.Check.PatientsWithUnverified++
		}
	}
	if s.Check.Points > 0 {
		s.Check.PassRate = float64(s.Check.Verified) / float64(s.Check.Points)
	}
	s.Input.Overview = summarize(overview)
	s.Input.UserMessage = summarize(user)
	s.Input.Evidence = summarize(evidence)
	s.Durations.GeneratePerPatientMS = summarize(generate)
	s.Runs.LLMCallsPerPatient = summarize(llmCalls)
}

func summarize(values []int) Summary5 {
	if len(values) == 0 {
		return Summary5{}
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	total := 0
	for _, v := range sorted {
		total += v
	}
	return Summary5{
		N:      len(sorted),
		Min:    sorted[0],
		Median: sorted[(len(sorted)-1)/2],
		P95:    sorted[(len(sorted)*95+99)/100-1],
		Max:    sorted[len(sorted)-1],
		Total:  total,
	}
}
