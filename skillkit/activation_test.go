package skillkit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestActivateLoadsOnlyRequestedSkillBody(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "clinical-summary", activationSkillSource("clinical-summary", true, []string{"references/schema.md"}, "# Instructions\nUse approved sources only.\n"), map[string]string{
		"references/schema.md": "schema version one\n",
	})
	writeSkill(t, root, "unused-skill", activationSkillSource("unused-skill", false, nil, "# Instructions\nThis must not be loaded.\n"), nil)

	catalog := activationCatalog(t, root)
	activation, err := catalog.Activate(ActivationRequest{
		Skills:      []Ref{{Name: "clinical-summary"}},
		GateContext: GateContext{AllowedToolIDs: map[string]bool{"artifact.read": true}},
	})
	if err != nil {
		t.Fatalf("Activate returned error: %v", err)
	}
	skills := activation.Skills()
	if len(skills) != 1 {
		t.Fatalf("skills = %#v, want one activated skill", skills)
	}
	if skills[0].Ref.Digest == "" || skills[0].Name != "clinical-summary" {
		t.Fatalf("skill identity = %#v, want complete clinical-summary ref", skills[0])
	}
	if got, want := skills[0].Content, "# Instructions\nUse approved sources only.\n"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if strings.Contains(skills[0].Content, "metadata:") || strings.Contains(skills[0].Content, "unused-skill") {
		t.Fatalf("content leaks frontmatter or an unrequested skill: %q", skills[0].Content)
	}
}

func TestActivateRejectsUnavailableSkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "clinical-summary", activationSkillSource("clinical-summary", true, nil, "# Instructions\nUse approved sources only.\n"), nil)

	_, err := activationCatalog(t, root).Activate(ActivationRequest{
		Skills:      []Ref{{Name: "clinical-summary"}},
		GateContext: GateContext{},
	})
	if !errors.Is(err, ErrSkillUnavailable) {
		t.Fatalf("Activate error = %v, want ErrSkillUnavailable", err)
	}
}

func TestActivateRejectsChangedPackageDigest(t *testing.T) {
	root := t.TempDir()
	skillDir := writeSkill(t, root, "clinical-summary", activationSkillSource("clinical-summary", true, []string{"references/schema.md"}, "# Instructions\nUse approved sources only.\n"), map[string]string{
		"references/schema.md": "first\n",
	})
	catalog := activationCatalog(t, root)
	if err := os.WriteFile(filepath.Join(skillDir, "references", "schema.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatalf("change resource: %v", err)
	}

	_, err := catalog.Activate(ActivationRequest{
		Skills:      []Ref{{Name: "clinical-summary"}},
		GateContext: GateContext{AllowedToolIDs: map[string]bool{"artifact.read": true}},
	})
	if !errors.Is(err, ErrSkillDigestMismatch) {
		t.Fatalf("Activate error = %v, want ErrSkillDigestMismatch", err)
	}
}

func TestActivationReadsOnlyAllowedResource(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "clinical-summary", activationSkillSource("clinical-summary", true, []string{"references/schema.md"}, "# Instructions\nUse approved sources only.\n"), map[string]string{
		"references/schema.md":  "schema version one\n",
		"references/private.md": "must stay unavailable\n",
	})
	activation := activateClinicalSummary(t, activationCatalog(t, root))
	skill := activation.Skills()[0]

	uri, err := activation.ResourceURI(skill.Ref, "references/schema.md")
	if err != nil {
		t.Fatalf("ResourceURI returned error: %v", err)
	}
	if !strings.HasPrefix(uri, "skill://clinical-summary@"+skill.Ref.Digest+"/") {
		t.Fatalf("uri = %q, want skill ref", uri)
	}
	content, err := activation.ReadResource(uri)
	if err != nil {
		t.Fatalf("ReadResource returned error: %v", err)
	}
	if got, want := string(content), "schema version one\n"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}

	for _, resource := range []string{"references/private.md", "../outside.md", "/outside.md"} {
		_, err := activation.ResourceURI(skill.Ref, resource)
		if !errors.Is(err, ErrInvalidSkillResource) {
			t.Fatalf("ResourceURI(%q) error = %v, want ErrInvalidSkillResource", resource, err)
		}
	}
	_, err = activation.ResourceURI(Ref{Name: "other-skill", Digest: skill.Ref.Digest}, "references/schema.md")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("ResourceURI for unactivated ref error = %v, want ErrSkillNotFound", err)
	}
}

func TestActivationReadResourceRejectsChangedAndOversizedContent(t *testing.T) {
	root := t.TempDir()
	skillDir := writeSkill(t, root, "clinical-summary", activationSkillSource("clinical-summary", true, []string{"references/schema.md"}, "# Instructions\nUse approved sources only.\n"), map[string]string{
		"references/schema.md": "first\n",
	})
	activation := activateClinicalSummary(t, activationCatalog(t, root))
	uri, err := activation.ResourceURI(activation.Skills()[0].Ref, "references/schema.md")
	if err != nil {
		t.Fatalf("ResourceURI returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "schema.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatalf("change resource: %v", err)
	}
	if _, err := activation.ReadResource(uri); !errors.Is(err, ErrSkillDigestMismatch) {
		t.Fatalf("ReadResource after mutation error = %v, want ErrSkillDigestMismatch", err)
	}

	oversizedRoot := t.TempDir()
	writeSkill(t, oversizedRoot, "clinical-summary", activationSkillSource("clinical-summary", true, []string{"references/schema.md"}, "# Instructions\nUse approved sources only.\n"), map[string]string{
		"references/schema.md": strings.Repeat("a", maxResourceBytes+1),
	})
	oversized := activateClinicalSummary(t, activationCatalog(t, oversizedRoot))
	oversizedURI, err := oversized.ResourceURI(oversized.Skills()[0].Ref, "references/schema.md")
	if err != nil {
		t.Fatalf("oversized ResourceURI returned error: %v", err)
	}
	if _, err := oversized.ReadResource(oversizedURI); !errors.Is(err, ErrInvalidSkillResource) {
		t.Fatalf("ReadResource oversized error = %v, want ErrInvalidSkillResource", err)
	}
}

func TestActivationErrorsDoNotLeakRootPath(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "clinical-summary", activationSkillSource("clinical-summary", true, nil, "# Instructions\nUse approved sources only.\n"), nil)

	activation := activateClinicalSummary(t, activationCatalog(t, root))
	_, err := activation.ReadResource("skill://clinical-summary@not-a-digest/references/schema.md")
	if err == nil {
		t.Fatal("ReadResource returned nil error")
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("ReadResource error leaks root path: %v", err)
	}
}

func TestCatalogAndActivationFormattingDoNotLeakRootPath(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "clinical-summary", activationSkillSource("clinical-summary", true, nil, "# Instructions\nUse approved sources only.\n"), nil)
	catalog := activationCatalog(t, root)
	activation := activateClinicalSummary(t, catalog)

	for _, value := range []any{catalog, activation} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if formatted := fmt.Sprintf(format, value); strings.Contains(formatted, root) {
				t.Fatalf("formatted value leaks root path: %s", formatted)
			}
		}
	}
}

func activationCatalog(t *testing.T, root string) *Catalog {
	t.Helper()
	catalog, err := Discover([]Root{{ID: "builtin", Dir: root, Scope: ScopeBuiltin, Trusted: true, Enabled: true}})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	return catalog
}

func activateClinicalSummary(t *testing.T, catalog *Catalog) *Activation {
	t.Helper()
	activation, err := catalog.Activate(ActivationRequest{
		Skills:      []Ref{{Name: "clinical-summary"}},
		GateContext: GateContext{AllowedToolIDs: map[string]bool{"artifact.read": true}},
	})
	if err != nil {
		t.Fatalf("Activate returned error: %v", err)
	}
	return activation
}

func activationSkillSource(name string, requiresArtifactRead bool, resources []string, body string) string {
	var builder strings.Builder
	builder.WriteString("---\nname: ")
	builder.WriteString(name)
	builder.WriteString("\ndescription: A bounded skill.\n")
	if requiresArtifactRead || len(resources) > 0 {
		builder.WriteString("metadata:\n  goagents:\n")
	}
	if requiresArtifactRead {
		builder.WriteString("    requires:\n      tools:\n        required: [artifact.read]\n")
	}
	if len(resources) > 0 {
		builder.WriteString("    resources:\n      allow:\n")
		for _, resource := range resources {
			builder.WriteString("        - ")
			builder.WriteString(resource)
			builder.WriteString("\n")
		}
	}
	builder.WriteString("---\n")
	builder.WriteString(body)
	return builder.String()
}
