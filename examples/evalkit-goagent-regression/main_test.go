package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRegressionDemoRunsGoAgentTrialsAndGraders(t *testing.T) {
	var out bytes.Buffer
	if err := runRegressionDemo(&out); err != nil {
		t.Fatalf("runRegressionDemo returned error: %v", err)
	}

	var got report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	if got.Suite != "goagent-account-regression" {
		t.Fatalf("suite = %q", got.Suite)
	}
	if got.TotalTrials != 2 || got.PassedTrials != 2 || got.FailedTrials != 0 {
		t.Fatalf("summary = %+v", got)
	}
	if len(got.Trials) != 2 {
		t.Fatalf("trial count = %d", len(got.Trials))
	}
	for _, trial := range got.Trials {
		if !trial.Passed {
			t.Fatalf("trial failed: %+v", trial)
		}
		if trial.TaskID != "account-a123-status" || trial.LLMCalls != 1 || trial.ToolCalls != 0 {
			t.Fatalf("trial contract = %+v", trial)
		}
		if trial.StepCount != 1 || trial.AgentSource != "goagent" {
			t.Fatalf("trace mapping = %+v", trial)
		}
		if len(trial.GradeNames) != 2 || trial.GradeNames[0] != "answer-contract" || trial.GradeNames[1] != "trace-contract" {
			t.Fatalf("grade names = %#v", trial.GradeNames)
		}
	}
}
