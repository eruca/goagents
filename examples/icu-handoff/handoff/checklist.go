package handoff

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ChecklistConfig drives the overview checklist (gocms design §10.4 item 3):
// every fact of each patient's overview as one row for two people to check
// against the UI.
type ChecklistConfig struct {
	Gocms    *Gocms
	Patients []string // S references
	AsOf     string
	Budget   int
	// RunID prefixes X-Agent-Run-Id.
	RunID string
}

// ChecklistRow is one fact to check.
type ChecklistRow struct {
	Patient  string `json:"patient"`
	Bed      string `json:"bed,omitempty"`
	Block    string `json:"block"`
	Fragment string `json:"fragment"`
	Value    string `json:"value"`
	Unit     string `json:"unit,omitempty"`
	At       string `json:"at,omitempty"`
	Flag     string `json:"flag,omitempty"`
	Ref      string `json:"ref,omitempty"`
	URL      string `json:"url,omitempty"`
	// Basis is the replayable tool call behind an aggregate (no ref).
	Basis string `json:"basis,omitempty"`
	// RefError is gocms' expand error code when the ref does not expand.
	RefError string `json:"ref_error,omitempty"`
}

// ChecklistStats is the content-free summary of a checklist run.
type ChecklistStats struct {
	Version    string         `json:"version"`
	Mode       string         `json:"mode"`
	Patients   int            `json:"patients"`
	Failed     int            `json:"failed"`
	Errors     map[string]int `json:"errors"`
	Rows       int            `json:"rows"`
	WithRef    int            `json:"with_ref"`
	Aggregates int            `json:"aggregates"`
	RefErrors  map[string]int `json:"ref_errors"`
	ByBlock    map[string]int `json:"by_block"`
	TotalMS    int64          `json:"total_ms"`
}

// Checklist fetches each patient's overview data over HTTP, lists every fact
// and resolves its UI address through expand.
func Checklist(ctx context.Context, cfg ChecklistConfig) ([]ChecklistRow, ChecklistStats, error) {
	start := time.Now()
	if cfg.Budget <= 0 {
		cfg.Budget = 1000
	}
	if cfg.RunID == "" {
		cfg.RunID = "checklist-" + time.Now().Format("20060102T150405")
	}
	stats := ChecklistStats{
		Version: Version, Mode: "check",
		Errors: map[string]int{}, RefErrors: map[string]int{}, ByBlock: map[string]int{},
	}
	var rows []ChecklistRow
	for _, stay := range cfg.Patients {
		stats.Patients++
		pctx := WithRunID(ctx, cfg.RunID+"-"+sanitizeID(stay))
		query := url.Values{"cite": {"true"}, "budget": {strconv.Itoa(cfg.Budget)}}
		if cfg.AsOf != "" {
			query.Set("as_of", cfg.AsOf)
		}
		text, data, err := cfg.Gocms.Overview(pctx, stay, query)
		if err != nil {
			stats.Failed++
			stats.Errors[errorCode(err)]++
			continue
		}
		var envelope struct {
			Result struct {
				Blocks map[string]json.RawMessage `json:"blocks"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			stats.Failed++
			stats.Errors["bad_data"]++
			continue
		}
		bed := ""
		var head struct {
			Bed string `json:"bed"`
		}
		if raw, ok := envelope.Result.Blocks["head"]; ok && json.Unmarshal(raw, &head) == nil {
			bed = head.Bed
		}
		var patientRows []ChecklistRow
		for _, block := range blockOrder(envelope.Result.Blocks) {
			if block == "head" {
				continue
			}
			var node any
			if err := json.Unmarshal(envelope.Result.Blocks[block], &node); err != nil {
				continue
			}
			walkFacts(node, func(row ChecklistRow) {
				row.Patient, row.Bed, row.Block = stay, bed, block
				if row.Ref != "" {
					row.Fragment = fragmentFor(text, row.Ref)
				}
				patientRows = append(patientRows, row)
			})
		}
		var refs []string
		seen := map[string]bool{}
		for _, row := range patientRows {
			if row.Ref != "" && !seen[row.Ref] {
				seen[row.Ref] = true
				refs = append(refs, row.Ref)
			}
		}
		if len(refs) > 0 {
			expanded, err := cfg.Gocms.Expand(pctx, stay, refs)
			if err != nil {
				stats.Errors["expand_"+errorCode(err)]++
			}
			for i := range patientRows {
				if res, ok := expanded[patientRows[i].Ref]; ok {
					if res.Error != nil {
						patientRows[i].RefError = res.Error.Code
						stats.RefErrors[sanitizeID(res.Error.Code)]++
					} else if res.URL != "" {
						patientRows[i].URL = cfg.Gocms.BaseURL + res.URL
					}
				}
			}
		}
		for _, row := range patientRows {
			stats.Rows++
			stats.ByBlock[sanitizeID(row.Block)]++
			if row.Ref != "" {
				stats.WithRef++
			}
			if row.Basis != "" {
				stats.Aggregates++
			}
		}
		rows = append(rows, patientRows...)
	}
	stats.TotalMS = time.Since(start).Milliseconds()
	return rows, stats, nil
}

// blockOrder lists the overview blocks in display order, unknown ones last.
func blockOrder(blocks map[string]json.RawMessage) []string {
	order := []string{"head", "dx", "support", "drugs", "vitals", "labs", "io", "scores", "todo", "shift"}
	rank := map[string]int{}
	for i, name := range order {
		rank[name] = i
	}
	var names []string
	for name := range blocks {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		ri, iok := rank[names[i]]
		rj, jok := rank[names[j]]
		switch {
		case iok && jok:
			return ri < rj
		case iok != jok:
			return iok
		}
		return names[i] < names[j]
	})
	return names
}

// walkFacts emits a row for every object carrying a reference ("ref", or
// "order" for drugs) and for every aggregate carrying a replay "basis".
func walkFacts(node any, emit func(ChecklistRow)) {
	switch v := node.(type) {
	case []any:
		for _, item := range v {
			walkFacts(item, emit)
		}
	case map[string]any:
		ref := str(v["ref"])
		if !IsRef(ref) {
			ref = str(v["order"])
		}
		if IsRef(ref) {
			emit(ChecklistRow{
				Ref:   ref,
				Value: factValue(v),
				Unit:  str(v["unit"]),
				At:    clock(firstOf(v, "at", "start")),
				Flag:  str(v["flag"]),
			})
		} else if basis, ok := v["basis"].(map[string]any); ok {
			emit(ChecklistRow{
				Value: aggregateValue(v),
				At:    str(v["date"]),
				Basis: basisText(basis),
			})
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key == "basis" {
				continue
			}
			walkFacts(v[key], emit)
		}
	}
}

func factValue(v map[string]any) string {
	if name, dose := str(v["name"]), str(v["dose"]); name != "" && dose != "" {
		parts := []string{name, dose, str(v["freq"]), str(v["route"])}
		return strings.Join(nonEmpty(parts), " ")
	}
	name := str(v["name"])
	for _, key := range []string{"text", "total", "v", "title"} {
		if value := str(v[key]); value != "" {
			if name != "" && key != "text" {
				return name + " " + value
			}
			if name != "" && key == "text" && !strings.Contains(value, name) {
				return name + " " + value
			}
			return value
		}
	}
	return name
}

func aggregateValue(v map[string]any) string {
	var parts []string
	for _, key := range []string{"in", "out", "urine", "net"} {
		if value := str(v[key]); value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	return strings.Join(parts, " ")
}

func basisText(basis map[string]any) string {
	args, _ := json.Marshal(basis["args"])
	return fmt.Sprintf("%s(%s)", str(basis["tool"]), args)
}

func firstOf(v map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := str(v[key]); value != "" {
			return value
		}
	}
	return ""
}

func str(value any) string {
	switch x := value.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	}
	return ""
}

func nonEmpty(parts []string) []string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// fragmentFor returns the overview text that a reference annotates: from the
// last separator before its bracket (outside parentheses) up to the bracket.
func fragmentFor(text, ref string) string {
	for _, line := range strings.Split(text, "\n") {
		idx := strings.Index(line, ref)
		if idx < 0 {
			continue
		}
		open := strings.LastIndex(line[:idx], "[")
		if open < 0 {
			return ""
		}
		before := []rune(line[:open])
		cut, depth := 0, 0
	scan:
		for i := len(before) - 1; i >= 0; i-- {
			switch before[i] {
			case '）', ')':
				depth++
			case '（', '(':
				if depth > 0 {
					depth--
				}
			case '；', '：', '】', '|', '｜':
				if depth == 0 {
					cut = i + 1
					break scan
				}
			}
		}
		return strings.TrimSpace(string(before[cut:]))
	}
	return ""
}

// WriteChecklist writes checklist.md and checklist.csv (patient content,
// mode 0600) and stats.json into dir.
func WriteChecklist(dir string, rows []ChecklistRow, stats ChecklistStats) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var md strings.Builder
	md.WriteString("# 概览核对单\n\n")
	md.WriteString("逐行点开地址，在界面上核对值、单位、时间、标记方向四样；合计类对照护理页、医嘱页上的合计（\"重放\"列是可以重放的工具调用）。\n\n")
	md.WriteString("| 病人 | 床 | 块 | 文字片段 | 值 | 单位 | 时间 | 标记 | 引用 | 地址 / 重放 | 核对 |\n")
	md.WriteString("|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, row := range rows {
		link := row.URL
		if row.Basis != "" {
			link = row.Basis
		}
		if row.RefError != "" {
			link = "错误：" + row.RefError
		}
		fmt.Fprintf(&md, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | |\n",
			cell(row.Patient), cell(row.Bed), cell(row.Block), cell(row.Fragment), cell(row.Value),
			cell(row.Unit), cell(row.At), cell(row.Flag), cell(row.Ref), cell(link))
	}
	if err := os.WriteFile(filepath.Join(dir, "checklist.md"), []byte(md.String()), 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "checklist.csv"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"patient", "bed", "block", "fragment", "value", "unit", "at", "flag", "ref", "url", "basis", "ref_error"})
	for _, row := range rows {
		_ = w.Write([]string{row.Patient, row.Bed, row.Block, row.Fragment, row.Value, row.Unit, row.At, row.Flag, row.Ref, row.URL, row.Basis, row.RefError})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return WriteStats(dir, stats)
}

func cell(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}
