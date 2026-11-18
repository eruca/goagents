package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eruca/goagents/memorykit"
	"github.com/google/uuid"
)

const chunkRunes = 1600
const maxFileBytes = 8 << 20
const maxSnapshotBytes = 128 << 10

var refPattern = regexp.MustCompile(`^document:([a-f0-9]{64}):([a-f0-9]{64}):([0-9]{6})$`)
var errSource = errors.New("invalid or unavailable registered source")

type snapshot struct {
	Text         string
	EvidenceHash string
	CreatedAt    time.Time
}

// A registry belongs to exactly one caller-authorized private project. OpenRoot
// confines reads to the local registry, including symlink traversal.
type registry struct {
	root      *os.Root
	scope     memorykit.Scope
	scopeHash string
}

func openRegistry(dir string, scope memorykit.Scope) (*registry, error) {
	if scope.SubjectType != memorykit.SubjectProject || scope.Validate() != nil {
		return nil, memorykit.ErrInvalidScope
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errSource
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errSource
	}
	identity, _ := json.Marshal(scope)
	return &registry{root: root, scope: scope, scopeHash: digest(identity)}, nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (r *registry) filename(ref string) (string, error) {
	m := refPattern.FindStringSubmatch(ref)
	if len(m) != 4 || m[1] != r.scopeHash {
		return "", errSource
	}
	return m[1] + "-" + m[2] + "-" + m[3] + ".json", nil
}
func (r *registry) load(ref string) (snapshot, error) {
	name, err := r.filename(ref)
	if err != nil {
		return snapshot{}, err
	}
	f, err := r.root.Open(name)
	if err != nil {
		return snapshot{}, errSource
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSnapshotBytes+1))
	if err != nil || len(b) > maxSnapshotBytes {
		return snapshot{}, errSource
	}
	var s snapshot
	if json.Unmarshal(b, &s) != nil || s.CreatedAt.IsZero() || !utf8.ValidString(s.Text) || digest([]byte(s.Text)) != s.EvidenceHash {
		return snapshot{}, errSource
	}
	return s, nil
}
func (r *registry) ReadSource(ctx context.Context, source memorykit.Source) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if source.Kind != "document" {
		return "", errSource
	}
	s, err := r.load(source.Ref)
	if err != nil || s.EvidenceHash != source.EvidenceHash {
		return "", errSource
	}
	return s.Text, nil
}

// publish writes a complete private file, then renames it. The CLI serializes
// all registry/database operations with an exclusive local lock.
func (r *registry) publish(name string, b []byte) error {
	tmp := name + ".tmp"
	f, err := r.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errSource
	}
	defer r.root.Remove(tmp)
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errSource
	}
	if err = r.root.Rename(tmp, name); err != nil {
		return errSource
	}
	return nil
}
func (r *registry) register(text string, doc string, index int, now time.Time) (memorykit.Source, error) {
	ref := fmt.Sprintf("document:%s:%s:%06d", r.scopeHash, doc, index)
	name, _ := r.filename(ref)
	s := snapshot{Text: text, EvidenceHash: digest([]byte(text)), CreatedAt: now}
	if old, err := r.load(ref); err == nil {
		if old.Text != text {
			return memorykit.Source{}, errSource
		}
		s = old
	} else {
		// Do not overwrite a corrupt or otherwise existing snapshot.
		if _, statErr := r.root.Stat(name); !errors.Is(statErr, os.ErrNotExist) {
			return memorykit.Source{}, errSource
		}
		b, _ := json.Marshal(s)
		if err := r.publish(name, b); err != nil {
			return memorykit.Source{}, err
		}
	}
	return memorykit.Source{Kind: "document", Ref: ref, EvidenceHash: s.EvidenceHash}, nil
}
func splitText(text string) []string {
	runes := []rune(text)
	var chunks []string
	for start := 0; start < len(runes); start += chunkRunes {
		end := min(start+chunkRunes, len(runes))
		part := strings.TrimSpace(string(runes[start:end]))
		if part != "" {
			chunks = append(chunks, part)
		}
	}
	return chunks
}
func (r *registry) ingest(ctx context.Context, dir string, jobs memorykit.ExtractionJobStore, extractorID string) (int, error) {
	// Flat directory: accept only regular .txt files and never follow input symlinks.
	input, err := os.OpenRoot(dir)
	if err != nil {
		return 0, errors.New("input directory unavailable")
	}
	defer input.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, errors.New("input directory unavailable")
	}
	count := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".txt") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return count, errors.New("input must be a regular text file")
		}
		f, err := input.Open(entry.Name())
		if err != nil {
			return count, errors.New("input read failed")
		}
		b, readErr := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
		f.Close()
		if readErr != nil || len(b) > maxFileBytes || !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
			return count, errors.New("invalid or oversized UTF-8 input")
		}
		doc := digest(b)
		for index, chunk := range splitText(string(b)) {
			source, err := r.register(chunk, doc, index, time.Now().UTC())
			if err != nil {
				return count, err
			}
			s, err := r.load(source.Ref)
			if err != nil {
				return count, err
			}
			identity := source.Ref + "\x00" + extractorID
			jobID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(identity)).String()
			// The caller-owned ingestion job is the source operation for a file;
			// this is provenance, not a claim that an LLM generated the document.
			job := memorykit.ExtractionJob{ID: jobID, Scope: r.scope, Source: source, SourceAgentID: jobID, ExtractorID: extractorID, Status: memorykit.ExtractionPending, CreatedAt: s.CreatedAt, UpdatedAt: s.CreatedAt}
			if err = jobs.EnqueueExtraction(ctx, job); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

// Cached extraction freezes the first validated output, including its timestamp
// and caller-assigned document keys, across retries and partial worker writes.
type cachedExtractor struct {
	registry    *registry
	delegate    memorykit.CandidateExtractor
	limits      memorykit.Limits
	extractorID string
}

func (e cachedExtractor) Extract(ctx context.Context, req memorykit.ExtractionRequest) ([]memorykit.CandidateDraft, error) {
	if req.Scope != e.registry.scope {
		return nil, memorykit.ErrInvalidScope
	}
	// Check before cache lookup too: a cached old result must not disguise a
	// worker configured for another model/prompt version.
	if e.extractorID == "" || req.ExtractorID != e.extractorID {
		return nil, errors.New("extractor version mismatch; resume with the job's original configuration")
	}
	s, err := e.registry.load(req.Source.Ref)
	if err != nil || s.Text != req.Text || s.EvidenceHash != req.Source.EvidenceHash {
		return nil, errSource
	}
	name := "draft-" + digest([]byte(req.Source.Ref+"\x00"+req.ExtractorID)) + ".json"
	if f, err := e.registry.root.Open(name); err == nil {
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxSnapshotBytes+1))
		var drafts []memorykit.CandidateDraft
		if err != nil || len(b) > maxSnapshotBytes || json.Unmarshal(b, &drafts) != nil {
			return nil, errSource
		}
		if err = e.validate(drafts); err != nil {
			return nil, err
		}
		return drafts, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errSource
	}
	req.Now = s.CreatedAt
	drafts, err := e.delegate.Extract(ctx, req)
	if err != nil {
		return nil, errors.New("candidate extraction failed")
	}
	m := refPattern.FindStringSubmatch(req.Source.Ref)
	for i := range drafts {
		drafts[i].Key = fmt.Sprintf("doc.%s.c%s.k%03d", m[2], m[3], i)
		if drafts[i].ValidFrom.IsZero() {
			drafts[i].ValidFrom = s.CreatedAt
		}
	}
	if err = e.validate(drafts); err != nil {
		return nil, err
	}
	b, err := json.Marshal(drafts)
	if err != nil || len(b) > maxSnapshotBytes {
		return nil, errSource
	}
	if err = e.registry.publish(name, b); err != nil {
		return nil, err
	}
	return drafts, nil
}
func (e cachedExtractor) validate(drafts []memorykit.CandidateDraft) error {
	if len(drafts) > 8 {
		return memorykit.ErrInvalidMemory
	}
	for _, d := range drafts {
		if err := memorykit.ValidateCandidateDraft(d, e.limits); err != nil {
			return err
		}
	}
	return nil
}

// Offline smoke mode copies untrusted source text to a candidate. It is NOT an
// LLM summary and never treats the imported text as approved active knowledge.
type copyExtractor struct{}

func (copyExtractor) Extract(_ context.Context, req memorykit.ExtractionRequest) ([]memorykit.CandidateDraft, error) {
	return []memorykit.CandidateDraft{{Kind: memorykit.KindFact, Key: "pending", Content: req.Text, ValidFrom: req.Now, Importance: 50, Confidence: 0}}, nil
}

type validator struct{}

func (validator) ValidateMemoryContent(_ context.Context, text string) error {
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" || strings.ContainsRune(text, 0) {
		return memorykit.ErrInvalidMemory
	}
	return nil
}
