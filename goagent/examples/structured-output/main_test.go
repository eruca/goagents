package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

func TestStructuredOutputDemoShowsSuccessAndFailure(t *testing.T) {
	var out bytes.Buffer
	if err := runStructuredOutputDemo(&out); err != nil {
		t.Fatalf("runStructuredOutputDemo returned error: %v", err)
	}

	records := decodeDemoRecords(t, out.Bytes())
	if len(records) != 2 {
		t.Fatalf("record count = %d, output:\n%s", len(records), out.String())
	}
	success := records[0]
	if success.Case != "schema-success" || success.Status != "validated" {
		t.Fatalf("success record = %+v", success)
	}
	if string(success.Structured) != `{"status":"ok","risk":"low"}` {
		t.Fatalf("structured output = %s", success.Structured)
	}
	if success.OutputFormat != "status_risk" || success.Schema != "json" {
		t.Fatalf("metadata = %+v", success)
	}

	failure := records[1]
	if failure.Case != "schema-failure" || failure.Status != "blocked" {
		t.Fatalf("failure record = %+v", failure)
	}
	if !failure.ErrorMatches || !failure.HasPartial {
		t.Fatalf("failure flags = %+v", failure)
	}
}

func decodeDemoRecords(t *testing.T, body []byte) []demoRecord {
	t.Helper()
	var records []demoRecord
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		var record demoRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode record %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan output: %v", err)
	}
	return records
}
