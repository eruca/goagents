package skillkit

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const maxDescriptionRunes = 280

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Requirements describes capabilities a skill needs from its host. These are
// declarations only; they never grant a tool or permission.
type Requirements struct {
	OS              []string
	HostFeatures    []string
	RequiredToolIDs []string
	OptionalToolIDs []string
}

// Manifest is the portable SKILL.md metadata interpreted by skillkit.
// Metadata preserves unrecognized fields so the package remains portable.
type Manifest struct {
	Name         string
	Description  string
	License      string
	Requirements Requirements
	Resources    []string
	Metadata     map[string]any
}

// ParseManifest reads the YAML frontmatter at the start of a SKILL.md file.
// It intentionally does not execute, install, or resolve any referenced file.
func ParseManifest(directory string, source []byte) (Manifest, error) {
	frontmatter, err := frontmatterDocument(source)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidSkillManifest, err)
	}

	var document struct {
		Name        string         `yaml:"name"`
		Description string         `yaml:"description"`
		License     string         `yaml:"license"`
		Metadata    map[string]any `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(frontmatter, &document); err != nil {
		return Manifest{}, fmt.Errorf("%w: malformed frontmatter", ErrInvalidSkillManifest)
	}

	manifest := Manifest{
		Name:        document.Name,
		Description: document.Description,
		License:     document.License,
		Metadata:    cloneMetadata(document.Metadata),
	}
	requirements, resources, err := parseGoagentsMetadata(document.Metadata)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Requirements = requirements
	manifest.Resources = resources
	if err := ValidateManifest(directory, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// ValidateManifest checks the subset of the Agent Skills format needed for a
// safe local catalog. It does not inspect the filesystem.
func ValidateManifest(directory string, manifest Manifest) error {
	if !skillNamePattern.MatchString(manifest.Name) || len(manifest.Name) > 64 || manifest.Name != directory {
		return fmt.Errorf("%w: name must match directory", ErrInvalidSkillManifest)
	}
	if strings.TrimSpace(manifest.Description) == "" || utf8.RuneCountInString(manifest.Description) > maxDescriptionRunes {
		return fmt.Errorf("%w: description is required and bounded", ErrInvalidSkillManifest)
	}
	seen := make(map[string]struct{}, len(manifest.Resources))
	for _, resource := range manifest.Resources {
		if !validResourcePath(resource) {
			return fmt.Errorf("%w: resource must stay inside its skill", ErrInvalidSkillResource)
		}
		if _, exists := seen[resource]; exists {
			return fmt.Errorf("%w: duplicate resource", ErrInvalidSkillResource)
		}
		seen[resource] = struct{}{}
	}
	return nil
}

func frontmatterDocument(source []byte) ([]byte, error) {
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return nil, fmt.Errorf("frontmatter must start on the first line")
	}
	for index := 1; index < len(lines); index++ {
		if lines[index] == "---" {
			return []byte(strings.Join(lines[1:index], "\n")), nil
		}
	}
	return nil, fmt.Errorf("frontmatter closing delimiter is required")
}

func parseGoagentsMetadata(metadata map[string]any) (Requirements, []string, error) {
	goagents, ok, err := optionalMetadataMap(metadata, "goagents")
	if err != nil {
		return Requirements{}, nil, err
	}
	if !ok {
		return Requirements{}, nil, nil
	}
	requirementsMap, _, err := optionalMetadataMap(goagents, "requires")
	if err != nil {
		return Requirements{}, nil, err
	}
	tools, _, err := optionalMetadataMap(requirementsMap, "tools")
	if err != nil {
		return Requirements{}, nil, err
	}
	resourcesMap, _, err := optionalMetadataMap(goagents, "resources")
	if err != nil {
		return Requirements{}, nil, err
	}

	requirements := Requirements{}
	if requirements.OS, err = optionalStringList(requirementsMap, "os"); err != nil {
		return Requirements{}, nil, err
	}
	if requirements.HostFeatures, err = optionalStringList(requirementsMap, "host_features"); err != nil {
		return Requirements{}, nil, err
	}
	if requirements.RequiredToolIDs, err = optionalStringList(tools, "required"); err != nil {
		return Requirements{}, nil, err
	}
	if requirements.OptionalToolIDs, err = optionalStringList(tools, "optional"); err != nil {
		return Requirements{}, nil, err
	}
	resources, err := optionalResourceList(resourcesMap, "allow")
	if err != nil {
		return Requirements{}, nil, err
	}
	return requirements, resources, nil
}

func optionalMetadataMap(metadata map[string]any, key string) (map[string]any, bool, error) {
	if metadata == nil {
		return nil, false, nil
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return nil, false, nil
	}
	mapValue, ok := value.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("%w: goagents extension map is malformed", ErrInvalidSkillManifest)
	}
	return mapValue, true, nil
}

func optionalStringList(metadata map[string]any, key string) ([]string, error) {
	if metadata == nil {
		return nil, nil
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: metadata list is malformed", ErrInvalidSkillManifest)
	}
	result := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%w: metadata list must contain non-empty strings", ErrInvalidSkillManifest)
		}
		if _, exists := seen[text]; exists {
			continue
		}
		seen[text] = struct{}{}
		result = append(result, text)
	}
	sort.Strings(result)
	return result, nil
}

func optionalResourceList(metadata map[string]any, key string) ([]string, error) {
	if metadata == nil {
		return nil, nil
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: metadata list is malformed", ErrInvalidSkillManifest)
	}
	resources := make([]string, 0, len(items))
	for _, item := range items {
		resource, ok := item.(string)
		if !ok || strings.TrimSpace(resource) == "" {
			return nil, fmt.Errorf("%w: metadata list must contain non-empty strings", ErrInvalidSkillManifest)
		}
		resources = append(resources, resource)
	}
	return resources, nil
}

func validResourcePath(resource string) bool {
	if resource == "" || strings.Contains(resource, "\\") || strings.HasPrefix(resource, "/") {
		return false
	}
	cleaned := path.Clean(resource)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	return cleaned == resource
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = cloneMetadataValue(value)
	}
	return cloned
}

func cloneMetadataValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMetadata(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneMetadataValue(item)
		}
		return cloned
	default:
		return typed
	}
}
