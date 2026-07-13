package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eruca/skillkit"
)

func TestLoadHostSkillConfigDisabled(t *testing.T) {
	catalog, gate, err := loadHostSkillConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("loadHostSkillConfig returned error: %v", err)
	}
	if catalog != nil || gate.OS != "" || len(gate.HostFeatures) != 0 || len(gate.AllowedToolIDs) != 0 {
		t.Fatalf("disabled Skill config = catalog:%v gate:%#v, want zero values", catalog, gate)
	}
}

func TestLoadHostSkillConfigDiscoversTrustedInstructionOnlySkill(t *testing.T) {
	root := t.TempDir()
	writeHostAPISkill(t, root, "workflow-review", "---\nname: workflow-review\ndescription: Review a workflow safely.\n---\n# Instructions\nReview scope and evidence.\n", nil)
	writeHostAPISkill(t, root, "tool-review", "---\nname: tool-review\ndescription: Requires a host tool.\nmetadata:\n  goagents:\n    requires:\n      tools:\n        required: [record_review]\n---\n# Instructions\nUse the required tool.\n", nil)

	catalog, gate, err := loadHostSkillConfig(func(key string) string {
		if key == hostAPISkillRootEnv {
			return root
		}
		return ""
	})
	if err != nil {
		t.Fatalf("loadHostSkillConfig returned error: %v", err)
	}
	if gate.OS != runtime.GOOS || len(gate.HostFeatures) != 0 || len(gate.AllowedToolIDs) != 0 {
		t.Fatalf("gate = %#v, want OS-only context", gate)
	}
	entries := catalog.List()
	if len(entries) != 2 {
		t.Fatalf("entries = %#v, want two Skills", entries)
	}
	for _, entry := range entries {
		if entry.RootID != localUserSkillRootID || entry.Scope != skillkit.ScopeUser || !entry.Trusted {
			t.Fatalf("entry trust = %#v, want trusted local user root", entry)
		}
		report := skillkit.Evaluate(entry, gate)
		if entry.Ref.Name == "workflow-review" && report.State != skillkit.AvailabilityEligible {
			t.Fatalf("workflow-review availability = %#v, want eligible", report)
		}
		if entry.Ref.Name == "tool-review" && report.State != skillkit.AvailabilityUnavailable {
			t.Fatalf("tool-review availability = %#v, want unavailable", report)
		}
	}
}

func TestLoadHostSkillConfigRejectsUnsafeRootWithoutPathLeak(t *testing.T) {
	file := filepath.Join(t.TempDir(), "skills.txt")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write ordinary file: %v", err)
	}
	tests := []struct {
		name string
		root string
	}{
		{name: "relative", root: "relative/skills"},
		{name: "missing", root: filepath.Join(t.TempDir(), "missing")},
		{name: "ordinary file", root: file},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := loadHostSkillConfig(func(key string) string {
				if key == hostAPISkillRootEnv {
					return test.root
				}
				return ""
			})
			if err == nil {
				t.Fatal("loadHostSkillConfig returned nil error")
			}
			if !strings.Contains(err.Error(), hostAPISkillRootEnv) {
				t.Fatalf("error = %v, want environment variable name", err)
			}
			if strings.Contains(err.Error(), test.root) {
				t.Fatalf("error leaks configured root: %v", err)
			}
		})
	}
}
