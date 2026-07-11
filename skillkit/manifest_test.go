package skillkit

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseManifestReadsPortableFieldsAndGoagentsRequirements(t *testing.T) {
	source := []byte("---\n" +
		"name: clinical-summary\n" +
		"description: Produce a bounded summary.\n" +
		"license: Apache-2.0\n" +
		"metadata:\n" +
		"  vendor: preserved\n" +
		"  goagents:\n" +
		"    requires:\n" +
		"      os: [linux, darwin, linux]\n" +
		"      host_features: [artifacts.v1]\n" +
		"      tools:\n" +
		"        required: [artifact.read, artifact.read]\n" +
		"        optional: [web.search]\n" +
		"    resources:\n" +
		"      allow: [references/schema.md]\n" +
		"---\n# Instructions\n")

	manifest, err := ParseManifest("clinical-summary", source)
	if err != nil {
		t.Fatalf("ParseManifest returned error: %v", err)
	}
	if manifest.Name != "clinical-summary" || manifest.Description != "Produce a bounded summary." || manifest.License != "Apache-2.0" {
		t.Fatalf("manifest fields = %#v", manifest)
	}
	if got, want := manifest.Requirements.OS, []string{"darwin", "linux"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OS = %#v, want %#v", got, want)
	}
	if got, want := manifest.Requirements.RequiredToolIDs, []string{"artifact.read"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("required tools = %#v, want %#v", got, want)
	}
	if got, want := manifest.Requirements.OptionalToolIDs, []string{"web.search"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("optional tools = %#v, want %#v", got, want)
	}
	if got, want := manifest.Resources, []string{"references/schema.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resources = %#v, want %#v", got, want)
	}
	if manifest.Metadata["vendor"] != "preserved" {
		t.Fatalf("metadata = %#v, want preserved vendor metadata", manifest.Metadata)
	}
}

func TestParseManifestRejectsInvalidNameDescriptionAndResource(t *testing.T) {
	longDescription := strings.Repeat("a", 281)
	tests := []struct {
		name      string
		directory string
		source    string
		wantErr   error
	}{
		{
			name:      "missing frontmatter",
			directory: "clinical-summary",
			source:    "name: clinical-summary\n",
			wantErr:   ErrInvalidSkillManifest,
		},
		{
			name:      "directory mismatch",
			directory: "other-name",
			source:    manifestSource("clinical-summary", "valid description", nil),
			wantErr:   ErrInvalidSkillManifest,
		},
		{
			name:      "uppercase name",
			directory: "Clinical-Summary",
			source:    manifestSource("Clinical-Summary", "valid description", nil),
			wantErr:   ErrInvalidSkillManifest,
		},
		{
			name:      "empty description",
			directory: "clinical-summary",
			source:    manifestSource("clinical-summary", "", nil),
			wantErr:   ErrInvalidSkillManifest,
		},
		{
			name:      "long description",
			directory: "clinical-summary",
			source:    manifestSource("clinical-summary", longDescription, nil),
			wantErr:   ErrInvalidSkillManifest,
		},
		{
			name:      "parent resource",
			directory: "clinical-summary",
			source:    manifestSource("clinical-summary", "valid description", []string{"../secret.txt"}),
			wantErr:   ErrInvalidSkillResource,
		},
		{
			name:      "absolute resource",
			directory: "clinical-summary",
			source:    manifestSource("clinical-summary", "valid description", []string{"/secret.txt"}),
			wantErr:   ErrInvalidSkillResource,
		},
		{
			name:      "duplicate resource",
			directory: "clinical-summary",
			source:    manifestSource("clinical-summary", "valid description", []string{"references/schema.md", "references/schema.md"}),
			wantErr:   ErrInvalidSkillResource,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseManifest(test.directory, []byte(test.source))
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ParseManifest error = %v, want errors.Is(%v)", err, test.wantErr)
			}
		})
	}
}

func TestParseManifestRejectsMalformedGoagentsMetadata(t *testing.T) {
	tests := []string{
		"metadata:\n  goagents: not-a-map\n",
		"metadata:\n  goagents:\n    requires: not-a-map\n",
		"metadata:\n  goagents:\n    requires:\n      tools: not-a-map\n",
		"metadata:\n  goagents:\n    requires:\n      os: darwin\n",
	}
	for _, metadata := range tests {
		t.Run(metadata, func(t *testing.T) {
			source := "---\nname: clinical-summary\ndescription: valid description\n" + metadata + "---\n# Instructions\n"
			_, err := ParseManifest("clinical-summary", []byte(source))
			if !errors.Is(err, ErrInvalidSkillManifest) {
				t.Fatalf("ParseManifest error = %v, want ErrInvalidSkillManifest", err)
			}
		})
	}
}

func manifestSource(name string, description string, resources []string) string {
	var builder strings.Builder
	builder.WriteString("---\n")
	builder.WriteString("name: ")
	builder.WriteString(name)
	builder.WriteString("\n")
	builder.WriteString("description: ")
	builder.WriteString(description)
	builder.WriteString("\n")
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
