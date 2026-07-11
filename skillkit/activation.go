package skillkit

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

const (
	maxSkillBodyBytes = 128 * 1024
	maxResourceBytes  = 1 << 20
)

// ActivationRequest is the host-controlled selection and capability context
// for one run. Declared requirements never add to GateContext permissions.
type ActivationRequest struct {
	Skills      []Ref
	GateContext GateContext
}

// ActivatedSkill is the safe, path-free view of one activated skill.
type ActivatedSkill struct {
	Ref             Ref
	Name            string
	Description     string
	Content         string
	RequiredToolIDs []string
	OptionalToolIDs []string
}

// Activation is an immutable run-start selection. It retains private catalog
// records only so it can revalidate content before returning a resource.
type Activation struct {
	records []activationRecord
}

type activationRecord struct {
	skill  ActivatedSkill
	record catalogRecord
}

// String avoids exposing private catalog roots through host diagnostics.
func (a *Activation) String() string {
	return fmt.Sprintf("skillkit.Activation{skills:%v}", a.Skills())
}

// GoString keeps %#v diagnostics path-free for the same reason as String.
func (a *Activation) GoString() string {
	return fmt.Sprintf("&skillkit.Activation{skills:%#v}", a.Skills())
}

// Activate resolves, gates, and rechecks each requested skill before exposing
// its instruction body. It never executes files or grants tools.
func (c *Catalog) Activate(request ActivationRequest) (*Activation, error) {
	if c == nil {
		return nil, ErrSkillNotFound
	}
	records := make([]activationRecord, 0, len(request.Skills))
	seen := make(map[Ref]struct{}, len(request.Skills))
	for _, ref := range request.Skills {
		entry, err := c.Resolve(ref)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[entry.Ref]; exists {
			return nil, fmt.Errorf("%w: duplicate skill %q", ErrSkillUnavailable, entry.Ref.Name)
		}
		seen[entry.Ref] = struct{}{}

		report := Evaluate(entry, request.GateContext)
		if report.State != AvailabilityEligible {
			return nil, fmt.Errorf("%w: %s", ErrSkillUnavailable, entry.Ref.Name)
		}
		record, ok := c.record(entry.Ref)
		if !ok {
			return nil, ErrSkillNotFound
		}
		source, manifest, err := verifyRecord(record)
		if err != nil {
			return nil, err
		}
		content, err := skillBody(source)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrSkillDigestMismatch, entry.Ref.Name)
		}
		records = append(records, activationRecord{
			skill: ActivatedSkill{
				Ref:             entry.Ref,
				Name:            manifest.Name,
				Description:     manifest.Description,
				Content:         content,
				RequiredToolIDs: append([]string(nil), manifest.Requirements.RequiredToolIDs...),
				OptionalToolIDs: append([]string(nil), manifest.Requirements.OptionalToolIDs...),
			},
			record: record,
		})
	}
	sort.Slice(records, func(left int, right int) bool {
		if records[left].skill.Name != records[right].skill.Name {
			return records[left].skill.Name < records[right].skill.Name
		}
		return records[left].skill.Ref.Digest < records[right].skill.Ref.Digest
	})
	return &Activation{records: records}, nil
}

// Skills returns a copy of the activated model-facing instructions.
func (a *Activation) Skills() []ActivatedSkill {
	if a == nil {
		return nil
	}
	skills := make([]ActivatedSkill, len(a.records))
	for index, record := range a.records {
		skills[index] = cloneActivatedSkill(record.skill)
	}
	return skills
}

// ResourceURI returns the only supported resource reference form. The target
// must belong to the current activation and be explicitly allowlisted.
func (a *Activation) ResourceURI(ref Ref, resource string) (string, error) {
	record, err := a.record(ref)
	if err != nil {
		return "", err
	}
	if !validResourcePath(resource) || !contains(record.record.entry.Manifest.Resources, resource) {
		return "", ErrInvalidSkillResource
	}
	return (&url.URL{
		Scheme: "skill",
		User:   url.User(record.skill.Ref.Name),
		Host:   record.skill.Ref.Digest,
		Path:   "/" + resource,
	}).String(), nil
}

// ReadResource returns a copy of one current, allowlisted resource. It checks
// the full package digest again so source mutations fail closed.
func (a *Activation) ReadResource(uri string) ([]byte, error) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "skill" || parsed.User == nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return nil, ErrInvalidSkillResource
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return nil, ErrInvalidSkillResource
	}
	resource := strings.TrimPrefix(parsed.Path, "/")
	if !strings.HasPrefix(parsed.Path, "/") || !validResourcePath(resource) {
		return nil, ErrInvalidSkillResource
	}
	record, err := a.record(Ref{Name: parsed.User.Username(), Digest: parsed.Host})
	if err != nil {
		return nil, err
	}
	if !contains(record.record.entry.Manifest.Resources, resource) {
		return nil, ErrInvalidSkillResource
	}
	if _, _, err := verifyRecord(record.record); err != nil {
		return nil, err
	}
	content, exists, err := readContainedFile(record.record.skillPath, resource)
	if err != nil || !exists {
		return nil, fmt.Errorf("%w: %s", ErrSkillDigestMismatch, record.skill.Name)
	}
	if len(content) > maxResourceBytes {
		return nil, ErrInvalidSkillResource
	}
	return append([]byte(nil), content...), nil
}

func (a *Activation) record(ref Ref) (activationRecord, error) {
	if a == nil || ref.Name == "" || ref.Digest == "" {
		return activationRecord{}, ErrSkillNotFound
	}
	for _, record := range a.records {
		if record.skill.Ref == ref {
			return record, nil
		}
	}
	return activationRecord{}, ErrSkillNotFound
}

func (c *Catalog) record(ref Ref) (catalogRecord, bool) {
	for _, record := range c.records {
		if record.entry.Ref == ref {
			return record, true
		}
	}
	return catalogRecord{}, false
}

func verifyRecord(record catalogRecord) ([]byte, Manifest, error) {
	source, exists, err := readContainedFile(record.skillPath, "SKILL.md")
	if err != nil || !exists {
		return nil, Manifest{}, fmt.Errorf("%w: %s", ErrSkillDigestMismatch, record.entry.Ref.Name)
	}
	manifest, err := ParseManifest(record.entry.Ref.Name, source)
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("%w: %s", ErrSkillDigestMismatch, record.entry.Ref.Name)
	}
	digest, err := packageDigest(record.skillPath, source, manifest.Resources)
	if err != nil || digest != record.entry.Ref.Digest {
		return nil, Manifest{}, fmt.Errorf("%w: %s", ErrSkillDigestMismatch, record.entry.Ref.Name)
	}
	return source, manifest, nil
}

func skillBody(source []byte) (string, error) {
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return "", ErrInvalidSkillManifest
	}
	for index := 1; index < len(lines); index++ {
		if lines[index] != "---" {
			continue
		}
		body := strings.Join(lines[index+1:], "\n")
		if strings.TrimSpace(body) == "" {
			return "", ErrInvalidSkillManifest
		}
		return body, nil
	}
	return "", ErrInvalidSkillManifest
}

func cloneActivatedSkill(skill ActivatedSkill) ActivatedSkill {
	skill.RequiredToolIDs = append([]string(nil), skill.RequiredToolIDs...)
	skill.OptionalToolIDs = append([]string(nil), skill.OptionalToolIDs...)
	return skill
}
