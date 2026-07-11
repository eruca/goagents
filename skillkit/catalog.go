package skillkit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scope identifies a host-configured skill root. A scope does not imply trust.
type Scope string

const (
	ScopeBuiltin   Scope = "builtin"
	ScopeUser      Scope = "user"
	ScopeWorkspace Scope = "workspace"
)

// Root is an explicit host-provided directory that may contain skill folders.
type Root struct {
	ID      string
	Dir     string
	Scope   Scope
	Trusted bool
	Enabled bool
}

// Ref identifies a specific immutable skill package by name and digest.
type Ref struct {
	Name   string
	Digest string
}

// EntryState describes whether an entry can be considered by a gate.
type EntryState string

const (
	EntryReady     EntryState = "ready"
	EntryInvalid   EntryState = "invalid"
	EntryAmbiguous EntryState = "ambiguous"
)

// Reason is a stable, non-sensitive explanation suitable for operator output.
type Reason struct {
	Code    string
	Subject string
}

// Entry is a catalog-safe skill description. It intentionally omits local
// filesystem paths and source contents.
type Entry struct {
	Ref           Ref
	Manifest      Manifest
	RootID        string
	SourceRootIDs []string
	Scope         Scope
	Trusted       bool
	State         EntryState
	Reasons       []Reason
}

// Catalog is an immutable snapshot produced by Discover.
type Catalog struct {
	records []catalogRecord
}

// catalogRecord keeps the physical location private to skillkit. Public
// catalog APIs return only the associated Entry.
type catalogRecord struct {
	entry     Entry
	skillPath string
}

// String avoids exposing the private canonical roots when a catalog reaches
// host diagnostics or logs.
func (c *Catalog) String() string {
	return fmt.Sprintf("skillkit.Catalog{entries:%v}", c.List())
}

// GoString keeps %#v diagnostics path-free for the same reason as String.
func (c *Catalog) GoString() string {
	return fmt.Sprintf("&skillkit.Catalog{entries:%#v}", c.List())
}

// Discover reads enabled roots and returns a deterministic catalog snapshot.
// It performs no process execution, dependency installation, network I/O, or
// environment mutation.
func Discover(roots []Root) (*Catalog, error) {
	records := make([]catalogRecord, 0)
	seenRootIDs := make(map[string]struct{})
	for _, root := range roots {
		if !root.Enabled {
			continue
		}
		if err := validateRoot(root, seenRootIDs); err != nil {
			return nil, err
		}
		seenRootIDs[root.ID] = struct{}{}
		canonicalRoot, err := canonicalDirectory(root.Dir)
		if err != nil {
			return nil, fmt.Errorf("skill root %q is unavailable", root.ID)
		}
		rootEntries, err := os.ReadDir(canonicalRoot)
		if err != nil {
			return nil, fmt.Errorf("skill root %q is unavailable", root.ID)
		}
		for _, directory := range rootEntries {
			if !directory.IsDir() {
				continue
			}
			record, present := discoverSkill(canonicalRoot, root, directory.Name())
			if present {
				records = append(records, record)
			}
		}
	}

	records = mergeEqualDigests(records)
	markAmbiguous(records)
	sortRecords(records)
	return &Catalog{records: records}, nil
}

// List returns a deep copy ordered by name, digest, and root ID.
func (c *Catalog) List() []Entry {
	if c == nil {
		return nil
	}
	entries := make([]Entry, len(c.records))
	for index, record := range c.records {
		entries[index] = cloneEntry(record.entry)
	}
	return entries
}

// Resolve returns an exact skill when digest is supplied. A digest-less ref
// succeeds only for one ready digest with the requested name.
func (c *Catalog) Resolve(ref Ref) (Entry, error) {
	if c == nil || strings.TrimSpace(ref.Name) == "" {
		return Entry{}, ErrSkillNotFound
	}
	var matches []Entry
	for _, record := range c.records {
		entry := record.entry
		if entry.Ref.Name != ref.Name {
			continue
		}
		if ref.Digest != "" && entry.Ref.Digest != ref.Digest {
			continue
		}
		matches = append(matches, entry)
	}
	if len(matches) == 0 {
		return Entry{}, ErrSkillNotFound
	}
	for _, entry := range matches {
		if entry.State == EntryAmbiguous {
			return Entry{}, ErrSkillAmbiguous
		}
	}
	if ref.Digest != "" {
		if len(matches) == 1 && matches[0].State == EntryReady {
			return cloneEntry(matches[0]), nil
		}
		return Entry{}, ErrSkillNotFound
	}
	ready := make([]Entry, 0, len(matches))
	for _, entry := range matches {
		if entry.State == EntryReady {
			ready = append(ready, entry)
		}
	}
	if len(ready) != 1 {
		return Entry{}, ErrSkillNotFound
	}
	return cloneEntry(ready[0]), nil
}

func validateRoot(root Root, seen map[string]struct{}) error {
	if strings.TrimSpace(root.ID) == "" || strings.TrimSpace(root.Dir) == "" {
		return fmt.Errorf("skill root id and directory are required")
	}
	if _, exists := seen[root.ID]; exists {
		return fmt.Errorf("duplicate skill root id %q", root.ID)
	}
	switch root.Scope {
	case ScopeBuiltin, ScopeUser, ScopeWorkspace:
		return nil
	default:
		return fmt.Errorf("skill root %q has invalid scope", root.ID)
	}
}

func canonicalDirectory(directory string) (string, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return canonical, nil
}

func discoverSkill(rootPath string, root Root, name string) (catalogRecord, bool) {
	skillPath := filepath.Join(rootPath, name)
	canonicalSkillPath, err := canonicalDirectory(skillPath)
	if err != nil || !pathWithin(rootPath, canonicalSkillPath) {
		return invalidRecord(root, name), true
	}
	source, exists, err := readContainedFile(canonicalSkillPath, "SKILL.md")
	if !exists {
		return catalogRecord{}, false
	}
	if err != nil {
		return invalidRecord(root, name), true
	}
	manifest, err := ParseManifest(name, source)
	if err != nil {
		return invalidRecord(root, name), true
	}
	if body, err := skillBody(source); err != nil || len(body) > maxSkillBodyBytes {
		return invalidRecord(root, name), true
	}
	digest, err := packageDigest(canonicalSkillPath, source, manifest.Resources)
	if err != nil {
		return invalidRecord(root, name), true
	}
	return catalogRecord{
		entry: Entry{
			Ref:           Ref{Name: manifest.Name, Digest: digest},
			Manifest:      cloneManifest(manifest),
			RootID:        root.ID,
			SourceRootIDs: []string{root.ID},
			Scope:         root.Scope,
			Trusted:       root.Trusted,
			State:         EntryReady,
		},
		skillPath: canonicalSkillPath,
	}, true
}

func invalidRecord(root Root, name string) catalogRecord {
	return catalogRecord{
		entry: Entry{
			Ref:           Ref{Name: name},
			RootID:        root.ID,
			SourceRootIDs: []string{root.ID},
			Scope:         root.Scope,
			Trusted:       root.Trusted,
			State:         EntryInvalid,
			Reasons:       []Reason{{Code: "invalid_manifest", Subject: name}},
		},
	}
}

func packageDigest(skillPath string, source []byte, resources []string) (string, error) {
	files := []digestFile{{Path: "SKILL.md", Content: source}}
	for _, resource := range resources {
		content, exists, err := readContainedFile(skillPath, resource)
		if err != nil || !exists {
			return "", fmt.Errorf("resource cannot be read")
		}
		files = append(files, digestFile{Path: resource, Content: content})
	}
	sort.Slice(files, func(left int, right int) bool {
		return files[left].Path < files[right].Path
	})
	hash := sha256.New()
	for _, file := range files {
		_, _ = hash.Write([]byte(file.Path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(file.Content)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type digestFile struct {
	Path    string
	Content []byte
}

func readContainedFile(skillPath string, relativePath string) ([]byte, bool, error) {
	pathOnDisk := filepath.Join(skillPath, filepath.FromSlash(relativePath))
	canonicalPath, err := filepath.EvalSymlinks(pathOnDisk)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil || !pathWithin(skillPath, canonicalPath) {
		return nil, true, fmt.Errorf("resource escapes skill root")
	}
	info, err := os.Stat(canonicalPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("resource is not a regular file")
	}
	content, err := os.ReadFile(canonicalPath)
	if err != nil {
		return nil, true, err
	}
	return content, true, nil
}

func pathWithin(root string, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func mergeEqualDigests(records []catalogRecord) []catalogRecord {
	groups := make(map[string][]catalogRecord)
	invalid := make([]catalogRecord, 0)
	for _, record := range records {
		entry := record.entry
		if entry.State != EntryReady {
			invalid = append(invalid, record)
			continue
		}
		key := entry.Ref.Name + "\x00" + entry.Ref.Digest
		groups[key] = append(groups[key], record)
	}
	merged := make([]catalogRecord, 0, len(groups)+len(invalid))
	for _, group := range groups {
		sort.Slice(group, func(left int, right int) bool {
			if group[left].entry.Trusted != group[right].entry.Trusted {
				return group[left].entry.Trusted
			}
			return group[left].entry.RootID < group[right].entry.RootID
		})
		record := group[0]
		entry := cloneEntry(record.entry)
		entry.SourceRootIDs = entry.SourceRootIDs[:0]
		for _, source := range group {
			entry.SourceRootIDs = append(entry.SourceRootIDs, source.entry.RootID)
			if source.entry.Trusted {
				entry.Trusted = true
			}
		}
		sort.Strings(entry.SourceRootIDs)
		record.entry = entry
		merged = append(merged, record)
	}
	return append(merged, invalid...)
}

func markAmbiguous(records []catalogRecord) {
	digests := make(map[string]map[string]struct{})
	for _, record := range records {
		entry := record.entry
		if entry.State != EntryReady {
			continue
		}
		if digests[entry.Ref.Name] == nil {
			digests[entry.Ref.Name] = make(map[string]struct{})
		}
		digests[entry.Ref.Name][entry.Ref.Digest] = struct{}{}
	}
	for index := range records {
		entry := &records[index].entry
		if entry.State != EntryReady || len(digests[entry.Ref.Name]) < 2 {
			continue
		}
		entry.State = EntryAmbiguous
		entry.Reasons = append(entry.Reasons, Reason{Code: "duplicate_name", Subject: entry.Ref.Name})
		sortReasons(entry.Reasons)
	}
}

func sortRecords(records []catalogRecord) {
	sort.Slice(records, func(left int, right int) bool {
		if records[left].entry.Ref.Name != records[right].entry.Ref.Name {
			return records[left].entry.Ref.Name < records[right].entry.Ref.Name
		}
		if records[left].entry.Ref.Digest != records[right].entry.Ref.Digest {
			return records[left].entry.Ref.Digest < records[right].entry.Ref.Digest
		}
		return records[left].entry.RootID < records[right].entry.RootID
	})
}

func sortReasons(reasons []Reason) {
	sort.Slice(reasons, func(left int, right int) bool {
		if reasons[left].Code != reasons[right].Code {
			return reasons[left].Code < reasons[right].Code
		}
		return reasons[left].Subject < reasons[right].Subject
	})
}

func cloneEntry(entry Entry) Entry {
	entry.Manifest = cloneManifest(entry.Manifest)
	entry.SourceRootIDs = append([]string(nil), entry.SourceRootIDs...)
	entry.Reasons = append([]Reason(nil), entry.Reasons...)
	return entry
}

func cloneManifest(manifest Manifest) Manifest {
	manifest.Requirements.OS = append([]string(nil), manifest.Requirements.OS...)
	manifest.Requirements.HostFeatures = append([]string(nil), manifest.Requirements.HostFeatures...)
	manifest.Requirements.RequiredToolIDs = append([]string(nil), manifest.Requirements.RequiredToolIDs...)
	manifest.Requirements.OptionalToolIDs = append([]string(nil), manifest.Requirements.OptionalToolIDs...)
	manifest.Resources = append([]string(nil), manifest.Resources...)
	manifest.Metadata = cloneMetadata(manifest.Metadata)
	return manifest
}
