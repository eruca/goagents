package handoff

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Output file names under --out.
const (
	MarkdownFile = "handoff.md"
	JSONFile     = "handoff.json"
	StatsFile    = "stats.json"
)

// Write writes handoff.md, handoff.json (patient content, mode 0600) and
// stats.json (content-free) into dir.
func Write(dir string, r *Result) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, MarkdownFile), []byte(Markdown(r)), 0o600); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, JSONFile), r, 0o600); err != nil {
		return err
	}
	return WriteStats(dir, r.Stats)
}

// WriteStats writes stats.json into dir.
func WriteStats(dir string, s any) error {
	return writeJSON(filepath.Join(dir, StatsFile), s, 0o644)
}

func writeJSON(path string, v any, mode os.FileMode) error {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), mode)
}

// Markdown lays the handoff out for people: ward header, movements, then one
// section per patient by bed.
func Markdown(r *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s 交班摘要\n\n", r.Ward)
	b.WriteString("> 由模型生成，以原始记录为准。标\"未核实\"的条目没有通过自动核对（引用不是本次工具返回的或展开不了，或其中的数字在工具返回里找不到），请对照原始记录。\n")
	if r.Mode == "dry_run" {
		b.WriteString(">\n> 试运行：只预取了概览，没有调用模型；下面每位病人是将发给模型的概览原文。\n")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "- 现在：%s\n", clock(r.Now))
	if r.Since != "" {
		fmt.Fprintf(&b, "- 本班起点：%s\n", clock(r.Since))
	}
	switch {
	case r.Sync.LastAt == nil:
		b.WriteString("- 数据同步：⚠ 无同步记录，数据可能不是最新\n")
	case r.Sync.Stale:
		fmt.Fprintf(&b, "- 数据同步：%s ⚠ 数据可能不是最新\n", clock(*r.Sync.LastAt))
	default:
		fmt.Fprintf(&b, "- 数据同步：%s\n", clock(*r.Sync.LastAt))
	}
	fmt.Fprintf(&b, "- 在科：%d 人", r.Census)
	if len(r.Patients) < r.Census {
		fmt.Fprintf(&b, "（本单只含前 %d 位）", len(r.Patients))
	}
	b.WriteString("\n")
	admitted, transfers, deaths, others := movements(r)
	fmt.Fprintf(&b, "- 本班新入 %d：%s\n", len(admitted), listOrNone(admitted))
	fmt.Fprintf(&b, "- 本班转出 %d：%s\n", len(transfers), listOrNone(transfers))
	fmt.Fprintf(&b, "- 本班死亡 %d：%s\n", len(deaths), listOrNone(deaths))
	if len(others) > 0 {
		fmt.Fprintf(&b, "- 其他出科 %d：%s\n", len(others), listOrNone(others))
	}

	for _, p := range r.Patients {
		b.WriteString("\n")
		writePatient(&b, p)
	}
	return b.String()
}

func writePatient(b *strings.Builder, p *PatientResult) {
	head := []string{p.Bed + "床"}
	for _, part := range []string{p.Sex, p.Age} {
		if part != "" {
			head = append(head, part)
		}
	}
	if p.Day > 0 {
		head = append(head, fmt.Sprintf("第%d天", p.Day))
	}
	title := strings.Join(head, " ")
	if p.Dx != "" {
		title += "｜" + p.Dx
	}
	if len(p.Support) > 0 {
		title += "｜" + strings.Join(p.Support, " ")
	}
	fmt.Fprintf(b, "## %s｜%s\n\n", title, p.Stay)

	switch p.Status {
	case StatusOverviewFailed:
		fmt.Fprintf(b, "**取不到概览**（%s），请直接查看记录。\n", p.ErrorCode)
		return
	case StatusDryRun:
		writeOverview(b, p.Overview)
		return
	case StatusFailed:
		fmt.Fprintf(b, "**生成失败**（%s），下面是概览原文：\n\n", p.ErrorCode)
		writeOverview(b, p.Overview)
		return
	case StatusOK:
	default:
		b.WriteString("**未处理**（运行被中断）。\n")
		return
	}
	s := p.Summary
	b.WriteString("**要点**\n\n")
	for i, point := range s.Points {
		mark := ""
		if i < len(p.Checks) && !p.Checks[i].Verified {
			mark = " 〔未核实：" + describeCheck(p.Checks[i]) + "〕"
		}
		fmt.Fprintf(b, "%d. %s%s\n", i+1, point.Text, mark)
	}
	if len(s.Watch) > 0 {
		b.WriteString("\n**下一班要盯**（模型归纳，不是记录里的数据）\n\n")
		for _, w := range s.Watch {
			fmt.Fprintf(b, "- %s\n", w)
		}
	}
	if len(s.Uncertain) > 0 {
		b.WriteString("\n**拿不准的**（工具里没有依据）\n\n")
		for _, u := range s.Uncertain {
			fmt.Fprintf(b, "- %s\n", u)
		}
	}
}

func writeOverview(b *strings.Builder, text string) {
	b.WriteString("```text\n")
	b.WriteString(strings.TrimRight(text, "\n"))
	b.WriteString("\n```\n")
}

func describeCheck(c PointCheck) string {
	var parts []string
	if len(c.MissingNumbers) > 0 {
		parts = append(parts, "数字 "+strings.Join(c.MissingNumbers, "、")+" 在工具返回里找不到")
	}
	if len(c.BadRefs) > 0 {
		parts = append(parts, fmt.Sprintf("%d 个引用核对不过", len(c.BadRefs)))
	}
	if len(parts) == 0 {
		return strings.Join(c.Issues, "、")
	}
	return strings.Join(parts, "；")
}

// movements splits the changes into this shift's admissions, transfers,
// deaths and other discharges. With a known shift start, earlier movements
// are left out.
func movements(r *Result) (admitted, transfers, deaths, others []string) {
	since, hasSince := parseTime(r.Since)
	inShift := func(at string) bool {
		if !hasSince {
			return true
		}
		t, ok := parseTime(at)
		return !ok || !t.Before(since)
	}
	for _, m := range r.Changes.Admitted {
		if inShift(m.At) {
			admitted = append(admitted, fmt.Sprintf("%s床（%s）", m.Bed, clock(m.At)))
		}
	}
	for _, m := range r.Changes.Discharged {
		if !inShift(m.At) {
			continue
		}
		item := fmt.Sprintf("%s床（%s，%s）", m.Bed, clock(m.At), m.Stay)
		switch m.Type {
		case "transfer":
			transfers = append(transfers, item)
		case "death":
			deaths = append(deaths, item)
		default:
			others = append(others, item)
		}
	}
	return admitted, transfers, deaths, others
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "无"
	}
	return strings.Join(items, "、")
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// clock writes an RFC 3339 time as "MM-DD HH:mm" in its own offset, which
// is gocms' local time.
func clock(s string) string {
	t, ok := parseTime(s)
	if !ok {
		return s
	}
	return t.Format("01-02 15:04")
}
