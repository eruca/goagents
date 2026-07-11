package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestGoAgentMCPHTTPExample(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out); err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	var got report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	if got.FinalAnswer != "Final answer: A-123 is active via MCP HTTP." {
		t.Fatalf("final answer = %q", got.FinalAnswer)
	}
	if got.LLMCalls != 2 || got.ToolCalls != 1 {
		t.Fatalf("call counts = %+v", got)
	}
	if !reflect.DeepEqual(got.UsedTools, []string{"lookup_status"}) {
		t.Fatalf("used tools = %#v", got.UsedTools)
	}
	if !got.MCPObservationSeen {
		t.Fatal("mock LLM did not see MCP tool observation")
	}
}
