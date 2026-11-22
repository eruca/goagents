package handoff

import (
	"context"
	"sort"
)

// Issue codes recorded on a point that failed a check. They are the only
// verification detail that reaches stats.json.
const (
	IssueRefBadFormat     = "ref_bad_format"      // not shaped like a gocms reference
	IssueRefNotSeen       = "ref_not_seen"        // not in any tool return of this patient's runs
	IssueRefRejected      = "ref_rejected"        // expand refused it (bad_ref, ref_mismatch, ...)
	IssueRefUnchecked     = "ref_unchecked"       // expand could not be reached
	IssueNumberNotInTools = "number_not_in_tools" // a number, date or time not in the tool returns
)

// PointCheck is the verdict on one point.
type PointCheck struct {
	Verified bool     `json:"verified"`
	Issues   []string `json:"issues,omitempty"`
	// MissingNumbers are the tokens of the text that no tool returned.
	MissingNumbers []string `json:"missing_numbers,omitempty"`
	// BadRefs maps each failing reference to its issue (and gocms' code).
	BadRefs map[string]string `json:"bad_refs,omitempty"`
	// Counts for stats.json.
	Refs    int `json:"-"`
	Numbers int `json:"-"`
}

// Expander checks references against one patient (gocms HTTP expand).
type Expander interface {
	Expand(ctx context.Context, stay string, refs []string) (map[string]ExpandResult, error)
}

// pointRefs is the union of a point's refs and any reference written in its
// text, in order.
func pointRefs(p Point) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, ref := range append(append([]string(nil), p.Refs...), ExtractRefs(p.Text)...) {
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	return out
}

// Verify checks every point of s against what the tools returned for this
// patient (ev) and against gocms expand. A failing point is marked, never
// removed. expandErr reports that expand itself could not be reached.
func Verify(ctx context.Context, s Summary, stay string, ev *Evidence, expander Expander) (checks []PointCheck, expandErr error) {
	var toExpand []string
	seen := map[string]struct{}{}
	for _, p := range s.Points {
		for _, ref := range pointRefs(p) {
			if !IsRef(ref) || !ev.HasRef(ref) {
				continue
			}
			if _, ok := seen[ref]; !ok {
				seen[ref] = struct{}{}
				toExpand = append(toExpand, ref)
			}
		}
	}
	sort.Strings(toExpand)
	var expanded map[string]ExpandResult
	if len(toExpand) > 0 && expander != nil {
		expanded, expandErr = expander.Expand(ctx, stay, toExpand)
	}
	checks = make([]PointCheck, len(s.Points))
	for i, p := range s.Points {
		check := PointCheck{}
		issues := map[string]bool{}
		for _, ref := range pointRefs(p) {
			check.Refs++
			issue := ""
			switch {
			case !IsRef(ref):
				issue = IssueRefBadFormat
			case !ev.HasRef(ref):
				issue = IssueRefNotSeen
			case expander == nil || expandErr != nil:
				issue = IssueRefUnchecked
			default:
				result, ok := expanded[ref]
				if !ok {
					issue = IssueRefUnchecked
				} else if result.Error != nil {
					issue = IssueRefRejected + ":" + result.Error.Code
				}
			}
			if issue != "" {
				if check.BadRefs == nil {
					check.BadRefs = map[string]string{}
				}
				check.BadRefs[ref] = issue
				if len(issue) >= len(IssueRefRejected) && issue[:len(IssueRefRejected)] == IssueRefRejected {
					issues[IssueRefRejected] = true
				} else {
					issues[issue] = true
				}
			}
		}
		missing, total := ev.Missing(p.Text)
		check.Numbers = total
		for _, token := range missing {
			check.MissingNumbers = append(check.MissingNumbers, token.Value)
		}
		if len(missing) > 0 {
			issues[IssueNumberNotInTools] = true
		}
		for issue := range issues {
			check.Issues = append(check.Issues, issue)
		}
		sort.Strings(check.Issues)
		check.Verified = len(check.Issues) == 0
		checks[i] = check
	}
	return checks, expandErr
}

// unverified counts the points that failed.
func unverified(checks []PointCheck) int {
	n := 0
	for _, c := range checks {
		if !c.Verified {
			n++
		}
	}
	return n
}
