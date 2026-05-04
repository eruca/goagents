package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestAuditLogWritesEventAndTerminalJSONL(t *testing.T) {
	var out bytes.Buffer

	if err := runAuditDemo(&out); err != nil {
		t.Fatalf("runAuditDemo returned error: %v", err)
	}

	records := decodeRecords(t, out.String())
	if len(records) == 0 {
		t.Fatal("no audit records written")
	}
	assertRecord(t, records, "run_event", "approval.requested")
	assertRecord(t, records, "run_event", "tool.completed")
	assertRecord(t, records, "run_event", "finalized")

	terminal := findTerminal(t, records)
	if terminal["status"] != "succeeded" {
		t.Fatalf("terminal status = %v", terminal["status"])
	}
	if terminal["content_preview"] != "Final answer: draft updated after approval." {
		t.Fatalf("content preview = %v", terminal["content_preview"])
	}
	summary, ok := terminal["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary = %#v", terminal["summary"])
	}
	if summary["llm_calls"] != float64(2) || summary["tool_calls"] != float64(1) {
		t.Fatalf("summary = %#v", summary)
	}
	if !strings.Contains(out.String(), `"used_tools":["update_draft"]`) {
		t.Fatalf("used tools missing from output:\n%s", out.String())
	}
	assertNotContains(t, out.String(), `{"title":"Approved draft"}`)
	assertNotContains(t, out.String(), "Update the draft title.")
	assertNotContains(t, out.String(), `draft updated title=\"Approved draft\"`)
}

func TestAuditLogAssignsEventSequences(t *testing.T) {
	var out bytes.Buffer

	if err := runAuditDemo(&out); err != nil {
		t.Fatalf("runAuditDemo returned error: %v", err)
	}

	records := decodeRecords(t, out.String())
	want := float64(1)
	for _, record := range records {
		if record["record"] != "run_event" {
			continue
		}
		if record["sequence"] != want {
			t.Fatalf("sequence = %v, want %v in record %#v", record["sequence"], want, record)
		}
		want++
	}
	if want == 1 {
		t.Fatal("no run_event records found")
	}
}

func decodeRecords(t *testing.T, body string) []map[string]any {
	t.Helper()
	var records []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan records: %v", err)
	}
	return records
}

func assertRecord(t *testing.T, records []map[string]any, recordType string, eventType string) {
	t.Helper()
	for _, record := range records {
		if record["record"] == recordType && record["event_type"] == eventType {
			return
		}
	}
	t.Fatalf("missing record=%s event_type=%s in %#v", recordType, eventType, records)
}

func findTerminal(t *testing.T, records []map[string]any) map[string]any {
	t.Helper()
	for _, record := range records {
		if record["record"] == "run_terminal" {
			return record
		}
	}
	t.Fatalf("missing terminal record in %#v", records)
	return nil
}

func assertNotContains(t *testing.T, s string, want string) {
	t.Helper()
	if strings.Contains(s, want) {
		t.Fatalf("found %q in:\n%s", want, s)
	}
}
