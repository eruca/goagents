package skillkit

// Availability is the host-relative result of checking one catalog entry.
type Availability string

const (
	AvailabilityEligible    Availability = "eligible"
	AvailabilityUnavailable Availability = "unavailable"
	AvailabilityInvalid     Availability = "invalid"
	AvailabilityAmbiguous   Availability = "ambiguous"
)

// GateContext contains only host-owned capability facts for one run.
type GateContext struct {
	OS             string
	HostFeatures   map[string]bool
	AllowedToolIDs map[string]bool
}

// AvailabilityReport is deterministic and safe to expose to an operator.
type AvailabilityReport struct {
	State   Availability
	Reasons []Reason
}

// Evaluate applies fail-closed entry and capability checks without performing
// I/O. Declared capabilities are requirements, never authorization grants.
func Evaluate(entry Entry, context GateContext) AvailabilityReport {
	switch entry.State {
	case EntryInvalid:
		return AvailabilityReport{State: AvailabilityInvalid, Reasons: cloneReasons(entry.Reasons)}
	case EntryAmbiguous:
		return AvailabilityReport{State: AvailabilityAmbiguous, Reasons: cloneReasons(entry.Reasons)}
	case EntryReady:
		// Continue below.
	default:
		return AvailabilityReport{
			State:   AvailabilityInvalid,
			Reasons: []Reason{{Code: "invalid_manifest", Subject: entry.Ref.Name}},
		}
	}

	if !entry.Trusted {
		return AvailabilityReport{
			State:   AvailabilityUnavailable,
			Reasons: []Reason{{Code: "untrusted_root", Subject: entry.RootID}},
		}
	}

	reasons := make([]Reason, 0, 1+len(entry.Manifest.Requirements.HostFeatures)+len(entry.Manifest.Requirements.RequiredToolIDs))
	if len(entry.Manifest.Requirements.OS) > 0 && !contains(entry.Manifest.Requirements.OS, context.OS) {
		reasons = append(reasons, Reason{Code: "unsupported_os", Subject: context.OS})
	}
	for _, feature := range entry.Manifest.Requirements.HostFeatures {
		if !context.HostFeatures[feature] {
			reasons = append(reasons, Reason{Code: "missing_feature", Subject: feature})
		}
	}
	for _, toolID := range entry.Manifest.Requirements.RequiredToolIDs {
		if !context.AllowedToolIDs[toolID] {
			reasons = append(reasons, Reason{Code: "missing_tool", Subject: toolID})
		}
	}
	if len(reasons) == 0 {
		return AvailabilityReport{State: AvailabilityEligible}
	}
	sortReasons(reasons)
	return AvailabilityReport{State: AvailabilityUnavailable, Reasons: reasons}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneReasons(reasons []Reason) []Reason {
	cloned := append([]Reason(nil), reasons...)
	sortReasons(cloned)
	return cloned
}
