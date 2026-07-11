package skillkit

import "testing"

func TestEvaluateRequiresTrustedRootOSFeatureAndRequiredTool(t *testing.T) {
	entry := Entry{
		State:   EntryReady,
		Trusted: true,
		Manifest: Manifest{Requirements: Requirements{
			OS:              []string{"darwin"},
			HostFeatures:    []string{"artifacts.v1"},
			RequiredToolIDs: []string{"artifact.read"},
			OptionalToolIDs: []string{"web.search"},
		}},
	}
	report := Evaluate(entry, GateContext{
		OS:             "darwin",
		HostFeatures:   map[string]bool{"artifacts.v1": true},
		AllowedToolIDs: map[string]bool{"artifact.read": true},
	})
	if report.State != AvailabilityEligible {
		t.Fatalf("report = %#v, want eligible", report)
	}

	report = Evaluate(entry, GateContext{
		OS:             "linux",
		HostFeatures:   map[string]bool{},
		AllowedToolIDs: map[string]bool{},
	})
	if report.State != AvailabilityUnavailable {
		t.Fatalf("report = %#v, want unavailable", report)
	}
	if !hasReason(report.Reasons, "unsupported_os", "linux") {
		t.Fatalf("reasons = %#v, want unsupported_os", report.Reasons)
	}
	if !hasReason(report.Reasons, "missing_feature", "artifacts.v1") {
		t.Fatalf("reasons = %#v, want missing_feature", report.Reasons)
	}
	if !hasReason(report.Reasons, "missing_tool", "artifact.read") {
		t.Fatalf("reasons = %#v, want missing_tool", report.Reasons)
	}
}

func TestEvaluateDoesNotBlockOnMissingOptionalTool(t *testing.T) {
	entry := Entry{
		State:   EntryReady,
		Trusted: true,
		Manifest: Manifest{Requirements: Requirements{
			OptionalToolIDs: []string{"web.search"},
		}},
	}
	report := Evaluate(entry, GateContext{})
	if report.State != AvailabilityEligible || len(report.Reasons) != 0 {
		t.Fatalf("report = %#v, want eligible without optional tool", report)
	}
}

func TestEvaluateRejectsUntrustedInvalidAndAmbiguousEntries(t *testing.T) {
	tests := []struct {
		name        string
		entry       Entry
		wantState   Availability
		wantCode    string
		wantSubject string
	}{
		{
			name:        "untrusted root",
			entry:       Entry{State: EntryReady, Trusted: false},
			wantState:   AvailabilityUnavailable,
			wantCode:    "untrusted_root",
			wantSubject: "",
		},
		{
			name:        "invalid manifest",
			entry:       Entry{State: EntryInvalid, Reasons: []Reason{{Code: "invalid_manifest", Subject: "broken"}}},
			wantState:   AvailabilityInvalid,
			wantCode:    "invalid_manifest",
			wantSubject: "broken",
		},
		{
			name:        "ambiguous name",
			entry:       Entry{State: EntryAmbiguous, Reasons: []Reason{{Code: "duplicate_name", Subject: "same"}}},
			wantState:   AvailabilityAmbiguous,
			wantCode:    "duplicate_name",
			wantSubject: "same",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := Evaluate(test.entry, GateContext{})
			if report.State != test.wantState || !hasReason(report.Reasons, test.wantCode, test.wantSubject) {
				t.Fatalf("report = %#v, want %s with %s", report, test.wantState, test.wantCode)
			}
		})
	}
}

func TestEvaluateSortsReasons(t *testing.T) {
	entry := Entry{
		State:   EntryReady,
		Trusted: true,
		Manifest: Manifest{Requirements: Requirements{
			HostFeatures:    []string{"zeta", "alpha"},
			RequiredToolIDs: []string{"tool.z", "tool.a"},
		}},
	}
	report := Evaluate(entry, GateContext{})
	if len(report.Reasons) != 4 {
		t.Fatalf("reasons = %#v, want four failures", report.Reasons)
	}
	if report.Reasons[0] != (Reason{Code: "missing_feature", Subject: "alpha"}) || report.Reasons[3] != (Reason{Code: "missing_tool", Subject: "tool.z"}) {
		t.Fatalf("reasons = %#v, want stable code/subject order", report.Reasons)
	}
}
