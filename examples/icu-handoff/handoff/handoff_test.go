package handoff

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/eruca/goagents/goagent/ports"
)

func tokenValues(text string) []string {
	var out []string
	for _, tok := range Tokens(text) {
		out = append(out, string(tok.Kind)+":"+tok.Value)
	}
	return out
}

func TestTokens(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"血钾 3.2↓ mmol/L（06:00）", []string{"number:3.2", "time:06:00"}},
		{"MAP 62↓–82 mmHg[C9100231ab,C9100877kd]", []string{"number:62", "number:82"}},
		{"P/F 300/0.5", []string{"number:300", "number:0.5"}},
		{"RASS -2，平衡 +860 ml，平衡-134 ml", []string{"number:-2", "number:860", "number:-134"}},
		{"统计日 11-21 07:00 起", []string{"pair:11-21", "time:07:00"}},
		{"2026-11-21 23:00", []string{"date:11-21", "time:23:00"}},
		{"11月3日 5:40", []string{"date:11-03", "time:05:40"}},
		{"SpO2 91%，FiO2 45%，q8h，IL-6 120", []string{"number:91", "number:45", "number:120"}},
		{"10^9/L", []string{"number:10", "number:9"}},
		{"62-82 mmHg", []string{"number:62", "number:82"}},
		{"120-80", []string{"number:120", "number:80"}},
		{"2.8-3.2", []string{"number:2.8", "number:3.2"}},
		{"３．２ mmol/L", []string{"number:3.2"}},
		{"近24h 第3天 S4821id", []string{"number:24", "number:3"}},
	}
	for _, c := range cases {
		if got := tokenValues(c.text); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Tokens(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestEvidenceMissing(t *testing.T) {
	ev := NewEvidence()
	ev.Add("【01床｜S1ab】现在 11-22 08:00\n统计日 11-21：平衡 +860 ml；MAP 62↓–82 mmHg[C101aa]；血钾 2.8↓↓（05:40）[L201ba]；RASS -2")
	cases := []struct {
		text    string
		missing []string
	}{
		{"血钾 05:40 危急值 2.8 mmol/L", nil},
		{"MAP 62–82 mmHg，平衡 860 ml", nil},
		{"MAP 62-82 mmHg", nil}, // hyphen range passes on its two numbers
		{"11月21日平衡 +860 ml", nil},
		{"RASS -2", nil},
		{"RASS 2", []string{"2"}},            // the sign is part of the number
		{"血钾 2.80 mmol/L", []string{"2.80"}}, // compared verbatim, no reformatting
		{"血钾 05:41", []string{"05:41"}},
		{"昨日 23:00 起", []string{"23:00"}},
		{"11-20 平衡", []string{"11-20"}},
		{"血钾较前下降 0.6", []string{"0.6"}}, // the model's own arithmetic
		{"见 C101aa", nil},               // reference digits are not numbers
	}
	for _, c := range cases {
		got, _ := ev.Missing(c.text)
		var values []string
		for _, tok := range got {
			values = append(values, tok.Value)
		}
		if !reflect.DeepEqual(values, c.missing) {
			t.Errorf("Missing(%q) = %v, want %v", c.text, values, c.missing)
		}
	}
	if !ev.HasRef("C101aa") || !ev.HasRef("S1ab") || ev.HasRef("C101ab") {
		t.Fatal("refs not recorded exactly")
	}
}

func TestExtractRefs(t *testing.T) {
	got := ExtractRefs("MAP 62–82[C9100231ab,C9100877kd]；SpO2 91%；PaO2/FiO2；L8812345dk L8812345dk；xL12ab；S4821id")
	want := []string{"C9100231ab", "C9100877kd", "L8812345dk", "S4821id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractRefs = %v, want %v", got, want)
	}
}

type fakeExpander struct {
	results map[string]ExpandResult
	calls   [][]string
}

func (f *fakeExpander) Expand(_ context.Context, _ string, refs []string) (map[string]ExpandResult, error) {
	f.calls = append(f.calls, refs)
	return f.results, nil
}

func TestVerify(t *testing.T) {
	ev := NewEvidence()
	ev.Add("血钾 2.8↓↓ mmol/L（05:40）[L201ba]；MAP 62↓–82 mmHg[C101aa]；[C555zz]")
	expander := &fakeExpander{results: map[string]ExpandResult{
		"L201ba": {Ref: "L201ba"},
		"C101aa": {Ref: "C101aa"},
		"C555zz": {Ref: "C555zz", Error: &ExpandError{Code: "ref_mismatch"}},
	}}
	s := Summary{Points: []Point{
		{Text: "血钾 05:40 危急值 2.8 mmol/L", Refs: []string{"L201ba"}},
		{Text: "MAP 62–82 mmHg", Refs: []string{"C101aa", "L777xx"}}, // invented ref
		{Text: "心率 110 次/分", Refs: []string{"C101aa"}},               // invented number
		{Text: "某值", Refs: []string{"C555zz"}},                       // another patient's ref
		{Text: "某值", Refs: []string{"11床"}},                          // not a reference
	}}
	checks, err := Verify(context.Background(), s, "S1ab", ev, expander)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{nil, {IssueRefNotSeen}, {IssueNumberNotInTools}, {IssueRefRejected}, {IssueRefBadFormat}}
	for i, c := range checks {
		if !reflect.DeepEqual(c.Issues, want[i]) || c.Verified != (want[i] == nil) {
			t.Errorf("point %d: issues %v verified %v, want %v", i, c.Issues, c.Verified, want[i])
		}
	}
	if checks[3].BadRefs["C555zz"] != IssueRefRejected+":ref_mismatch" {
		t.Fatalf("bad refs = %v", checks[3].BadRefs)
	}
	// Only references seen in tool returns go to expand, once each.
	if len(expander.calls) != 1 || !reflect.DeepEqual(expander.calls[0], []string{"C101aa", "C555zz", "L201ba"}) {
		t.Fatalf("expand calls = %v", expander.calls)
	}
	if unverified(checks) != 4 {
		t.Fatalf("unverified = %d", unverified(checks))
	}
}

type echoTool struct {
	got []json.RawMessage
}

func (e *echoTool) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "labs", Permission: ports.PermissionRead, Schema: ports.ToolSchema{JSONSchema: json.RawMessage(
		`{"type":"object","required":["patient","items"],"properties":{"patient":{"type":"string"},"as_of":{"type":"string"},"with_data":{"type":"boolean"},"items":{"type":"array"}},"additionalProperties":false}`)}}
}

func (e *echoTool) Execute(_ context.Context, input json.RawMessage, _ ports.ToolEnv) (*ports.ToolResult, error) {
	e.got = append(e.got, input)
	return &ports.ToolResult{ForLLM: "血钾 3.2↓ mmol/L[L202bb]"}, nil
}

func TestPinnedTool(t *testing.T) {
	inner := &echoTool{}
	a := &attempt{key: "k", stay: "S1ab", asOf: "2026-11-22 08:00", runID: "h-01", maxCalls: 2, evidence: NewEvidence(), counters: newToolCounters(), cache: newToolCache()}
	tool, err := newPinnedTool(inner, a)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Spec().Schema.JSONSchema, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	for _, key := range pinnedArgs {
		if _, ok := props[key]; ok {
			t.Fatalf("schema still has %s: %s", key, tool.Spec().Schema.JSONSchema)
		}
	}
	if !reflect.DeepEqual(schema["required"], []any{"items"}) {
		t.Fatalf("required = %v", schema["required"])
	}
	if tool.Spec().Permission != ports.PermissionRead {
		t.Fatal("permission lost")
	}
	// The model sends another patient and with_data; the host overrides.
	if err := tool.Spec().Schema.ValidateInput(json.RawMessage(`{"patient":"S2cd","items":["K"]}`)); err != nil {
		t.Fatalf("a stray patient argument must not be rejected locally: %v", err)
	}
	for i := 0; i < 3; i++ {
		res, err := tool.Execute(context.Background(), json.RawMessage(`{"patient":"S2cd","with_data":true,"items":["K"]}`), ports.ToolEnv{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && res.ForLLM != "血钾 3.2↓ mmol/L[L202bb]" {
			t.Fatalf("repeated call = %q, want the cached result", res.ForLLM)
		}
		if i == 2 && res.ForLLM != ToolLimitMessage {
			t.Fatalf("third call = %q, want the limit message", res.ForLLM)
		}
	}
	// The repeat was answered from the cache but still counted.
	if len(inner.got) != 1 || a.counters.duplicates != 1 {
		t.Fatalf("inner calls = %d, duplicates = %d; want 1, 1", len(inner.got), a.counters.duplicates)
	}
	var args map[string]any
	_ = json.Unmarshal(inner.got[0], &args)
	want := map[string]any{"patient": "S1ab", "as_of": "2026-11-22 08:00", "items": []any{"K"}}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("forwarded %v, want %v", args, want)
	}
	if !a.evidence.HasRef("L202bb") {
		t.Fatal("tool result not recorded as evidence")
	}
	if a.counters.limitHits != 1 || a.counters.byTool["labs"] != 1 {
		t.Fatalf("counters = %+v", a.counters)
	}
}

func TestExtractJSON(t *testing.T) {
	for _, in := range []string{
		`{"a":1}`,
		"```json\n{\"a\":1}\n```",
		"好的：\n{\"a\":1}\n以上。",
	} {
		got, err := extractJSON(in)
		if err != nil || got != `{"a":1}` {
			t.Errorf("extractJSON(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := extractJSON("没有"); err == nil {
		t.Fatal("want an error without JSON")
	}
}

func TestNaturalBedOrder(t *testing.T) {
	beds := []string{"10", "2", "加1", "01", "3A", "3"}
	sorted := append([]string(nil), beds...)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if naturalLess(sorted[j], sorted[i]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	if got := strings.Join(sorted, ","); got != "01,2,3,3A,10,加1" {
		t.Fatalf("order = %s", got)
	}
}

func TestRunIDIsCodeLike(t *testing.T) {
	id := runIDFor("handoff-20261122T0800", &PatientResult{Bed: "加1床", Stay: "S1ab"})
	if id != "handoff-20261122T0800-1" {
		t.Fatalf("run id = %q", id)
	}
	if id := runIDFor("h", &PatientResult{Bed: "加床", Stay: "S1ab"}); id != "h-S1ab" {
		t.Fatalf("run id = %q", id)
	}
}

func TestSummarize(t *testing.T) {
	s := summarize([]int{5, 1, 3, 2, 4})
	if s != (Summary5{N: 5, Min: 1, Median: 3, P95: 5, Max: 5, Total: 15}) {
		t.Fatalf("summarize = %+v", s)
	}
}
