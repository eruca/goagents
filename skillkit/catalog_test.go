package skillkit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverBuildsSortedEntryWithDigestWithoutLeakingPath(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "zeta-skill", validSkillSource("zeta-skill", nil), nil)
	writeSkill(t, root, "clinical-summary", validSkillSource("clinical-summary", []string{"references/schema.md"}), map[string]string{
		"references/schema.md": "schema version one\n",
	})

	catalog, err := Discover([]Root{{ID: "builtin", Dir: root, Scope: ScopeBuiltin, Trusted: true, Enabled: true}})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	entries := catalog.List()
	if len(entries) != 2 {
		t.Fatalf("entries = %#v, want two entries", entries)
	}
	if entries[0].Ref.Name != "clinical-summary" || entries[0].State != EntryReady {
		t.Fatalf("first entry = %#v, want ready clinical-summary", entries[0])
	}
	if len(entries[0].Ref.Digest) != 64 {
		t.Fatalf("digest = %q, want SHA-256 hex", entries[0].Ref.Digest)
	}
	if strings.Contains(fmt.Sprintf("%#v", entries[0]), root) {
		t.Fatalf("entry leaks root path: %#v", entries[0])
	}
	resolved, err := catalog.Resolve(Ref{Name: "clinical-summary", Digest: entries[0].Ref.Digest})
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if resolved.Ref != entries[0].Ref {
		t.Fatalf("Resolve = %#v, want %#v", resolved.Ref, entries[0].Ref)
	}
}

func TestDiscoverMarksDifferentDigestsWithSameNameAmbiguous(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	writeSkill(t, firstRoot, "clinical-summary", validSkillSource("clinical-summary", nil), nil)
	writeSkill(t, secondRoot, "clinical-summary", validSkillSource("clinical-summary", []string{"references/schema.md"}), map[string]string{
		"references/schema.md": "second copy\n",
	})

	catalog, err := Discover([]Root{
		{ID: "builtin", Dir: firstRoot, Scope: ScopeBuiltin, Trusted: true, Enabled: true},
		{ID: "workspace", Dir: secondRoot, Scope: ScopeWorkspace, Trusted: true, Enabled: true},
	})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	entries := catalog.List()
	if len(entries) != 2 || entries[0].State != EntryAmbiguous || entries[1].State != EntryAmbiguous {
		t.Fatalf("entries = %#v, want two ambiguous entries", entries)
	}
	if !hasReason(entries[0].Reasons, "duplicate_name", "clinical-summary") {
		t.Fatalf("reasons = %#v, want duplicate_name", entries[0].Reasons)
	}
	_, err = catalog.Resolve(Ref{Name: "clinical-summary"})
	if !errors.Is(err, ErrSkillAmbiguous) {
		t.Fatalf("Resolve error = %v, want ErrSkillAmbiguous", err)
	}
}

func TestDiscoverCollapsesEqualDigestAndRetainsSourceRoots(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	source := validSkillSource("clinical-summary", nil)
	writeSkill(t, firstRoot, "clinical-summary", source, nil)
	writeSkill(t, secondRoot, "clinical-summary", source, nil)

	catalog, err := Discover([]Root{
		{ID: "workspace", Dir: firstRoot, Scope: ScopeWorkspace, Trusted: true, Enabled: true},
		{ID: "builtin", Dir: secondRoot, Scope: ScopeBuiltin, Trusted: true, Enabled: true},
	})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	entries := catalog.List()
	if len(entries) != 1 || entries[0].State != EntryReady {
		t.Fatalf("entries = %#v, want one ready entry", entries)
	}
	if got := strings.Join(entries[0].SourceRootIDs, ","); got != "builtin,workspace" {
		t.Fatalf("source roots = %q, want builtin,workspace", got)
	}
}

func TestDiscoverLeavesInvalidManifestVisibleButUnresolvable(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "broken-skill", "---\nname: broken-skill\ndescription: \n---\n", nil)

	catalog, err := Discover([]Root{{ID: "builtin", Dir: root, Scope: ScopeBuiltin, Trusted: true, Enabled: true}})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	entries := catalog.List()
	if len(entries) != 1 || entries[0].State != EntryInvalid {
		t.Fatalf("entries = %#v, want one invalid entry", entries)
	}
	if !hasReason(entries[0].Reasons, "invalid_manifest", "broken-skill") {
		t.Fatalf("reasons = %#v, want invalid_manifest", entries[0].Reasons)
	}
	_, err = catalog.Resolve(Ref{Name: "broken-skill"})
	if !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("Resolve error = %v, want ErrSkillNotFound", err)
	}
}

func TestDiscoverRejectsAllowedResourceOutsideSkillRoot(t *testing.T) {
	root := t.TempDir()
	skillDir := writeSkill(t, root, "clinical-summary", validSkillSource("clinical-summary", []string{"references/escape.md"}), nil)
	outside := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatalf("write outside resource: %v", err)
	}
	escapePath := filepath.Join(skillDir, "references", "escape.md")
	if err := os.MkdirAll(filepath.Dir(escapePath), 0o755); err != nil {
		t.Fatalf("create references: %v", err)
	}
	if err := os.Symlink(outside, escapePath); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}

	catalog, err := Discover([]Root{{ID: "builtin", Dir: root, Scope: ScopeBuiltin, Trusted: true, Enabled: true}})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	entries := catalog.List()
	if len(entries) != 1 || entries[0].State != EntryInvalid || entries[0].Ref.Digest != "" {
		t.Fatalf("entries = %#v, want invalid undigested entry", entries)
	}
	if !hasReason(entries[0].Reasons, "invalid_manifest", "clinical-summary") {
		t.Fatalf("reasons = %#v, want invalid_manifest", entries[0].Reasons)
	}
}

func TestDiscoverDigestChangesWhenAllowedResourceChanges(t *testing.T) {
	root := t.TempDir()
	skillDir := writeSkill(t, root, "clinical-summary", validSkillSource("clinical-summary", []string{"references/schema.md"}), map[string]string{
		"references/schema.md": "first\n",
	})
	first, err := Discover([]Root{{ID: "builtin", Dir: root, Scope: ScopeBuiltin, Trusted: true, Enabled: true}})
	if err != nil {
		t.Fatalf("first Discover returned error: %v", err)
	}
	firstDigest := first.List()[0].Ref.Digest
	if err := os.WriteFile(filepath.Join(skillDir, "references", "schema.md"), []byte("second\n"), 0o600); err != nil {
		t.Fatalf("change resource: %v", err)
	}
	second, err := Discover([]Root{{ID: "builtin", Dir: root, Scope: ScopeBuiltin, Trusted: true, Enabled: true}})
	if err != nil {
		t.Fatalf("second Discover returned error: %v", err)
	}
	if second.List()[0].Ref.Digest == firstDigest {
		t.Fatalf("digest = %q, want change after resource mutation", firstDigest)
	}
}

func TestDiscoverIgnoresDisabledRoot(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "clinical-summary", validSkillSource("clinical-summary", nil), nil)

	catalog, err := Discover([]Root{{ID: "disabled", Dir: root, Scope: ScopeBuiltin, Trusted: true, Enabled: false}})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if entries := catalog.List(); len(entries) != 0 {
		t.Fatalf("entries = %#v, want no disabled-root entries", entries)
	}
}

func TestDiscoverDoesNotExposeRootPathInConfigurationError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-root")
	_, err := Discover([]Root{{ID: "workspace", Dir: missing, Scope: ScopeWorkspace, Trusted: true, Enabled: true}})
	if err == nil {
		t.Fatal("Discover returned nil error for missing root")
	}
	if strings.Contains(err.Error(), missing) {
		t.Fatalf("Discover error leaks root path: %v", err)
	}
}

func writeSkill(t *testing.T, root string, name string, source string, resources map[string]string) string {
	t.Helper()
	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("create skill directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	for resource, content := range resources {
		resourcePath := filepath.Join(skillDir, filepath.FromSlash(resource))
		if err := os.MkdirAll(filepath.Dir(resourcePath), 0o755); err != nil {
			t.Fatalf("create resource directory: %v", err)
		}
		if err := os.WriteFile(resourcePath, []byte(content), 0o600); err != nil {
			t.Fatalf("write resource: %v", err)
		}
	}
	return skillDir
}

func validSkillSource(name string, resources []string) string {
	var builder strings.Builder
	builder.WriteString("---\nname: ")
	builder.WriteString(name)
	builder.WriteString("\ndescription: A bounded skill.\n")
	if len(resources) > 0 {
		builder.WriteString("metadata:\n  goagents:\n    resources:\n      allow:\n")
		for _, resource := range resources {
			builder.WriteString("        - ")
			builder.WriteString(resource)
			builder.WriteString("\n")
		}
	}
	builder.WriteString("---\n# Instructions\n")
	return builder.String()
}

func hasReason(reasons []Reason, code string, subject string) bool {
	for _, reason := range reasons {
		if reason.Code == code && reason.Subject == subject {
			return true
		}
	}
	return false
}
