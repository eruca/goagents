// Package boundary enforces the dependency rules between the repository's
// kits. They used to be separate Go modules; within one module this test keeps
// each kit importable on its own without pulling in unrelated kits.
package boundary

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/eruca/goagents/"

// allowedDeps lists, per package path prefix, the other kits its non-test code
// may depend on, directly or transitively. The longest matching prefix wins.
// Example programs under examples/ directories may depend on anything.
var allowedDeps = map[string][]string{
	"artifactkit":             nil,
	"contextkit":              nil,
	"evalkit":                 nil,
	"goagent":                 nil,
	"hostkit":                 nil,
	"llmkit":                  nil,
	"llmkit/adapters/goagent": {"llmkit", "goagent"},
	"mcpkit":                  {"goagent"},
	"mcpkit/officialsdk":      {"mcpkit", "goagent"},
	"memorykit":               nil,
	"memorykit/agentadapter":  {"memorykit", "goagent"},
	"ocrs":                    nil,
	"runkit":                  {"goagent"},
	"skillkit":                nil,
	"skillkit/agentadapter":   {"skillkit", "goagent"},
	"workflowkit":             nil,
	"workflowkit/agentstep":   {"workflowkit", "goagent"},
}

func TestKitDependencyBoundaries(t *testing.T) {
	output, err := exec.Command("go", "list", "-f", "{{.ImportPath}}{{range .Deps}} {{.}}{{end}}", modulePath+"...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	checked := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		pkg := strings.TrimPrefix(fields[0], modulePath)
		if isExample(pkg) || strings.HasPrefix(pkg, "internal/") {
			continue
		}
		rule, ok := matchRule(pkg)
		if !ok {
			t.Errorf("package %s has no dependency rule; add its kit to allowedDeps", pkg)
			continue
		}
		checked++
		allowed := map[string]bool{kitOf(rule): true}
		for _, kit := range allowedDeps[rule] {
			allowed[kit] = true
		}
		var violations []string
		for _, dep := range fields[1:] {
			if !strings.HasPrefix(dep, modulePath) {
				continue
			}
			if kit := kitOf(strings.TrimPrefix(dep, modulePath)); !allowed[kit] {
				violations = append(violations, dep)
			}
		}
		if len(violations) > 0 {
			sort.Strings(violations)
			t.Errorf("%s must not depend on %s", pkg, strings.Join(violations, ", "))
		}
	}
	if checked == 0 {
		t.Fatal("no packages checked")
	}
}

func matchRule(pkg string) (string, bool) {
	best := ""
	for prefix := range allowedDeps {
		if (pkg == prefix || strings.HasPrefix(pkg, prefix+"/")) && len(prefix) > len(best) {
			best = prefix
		}
	}
	return best, best != ""
}

func kitOf(pkg string) string {
	kit, _, _ := strings.Cut(pkg, "/")
	return kit
}

func isExample(pkg string) bool {
	return strings.HasPrefix(pkg, "examples/") || strings.Contains(pkg, "/examples/")
}
