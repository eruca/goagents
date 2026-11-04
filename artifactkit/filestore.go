package artifactkit

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type FileStore struct {
	root string
}

type fileArtifact struct {
	Ref         string         `json:"ref"`
	Content     []byte         `json:"content"`
	ContentType string         `json:"content_type,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   time.Time      `json:"created_at,omitempty"`
}

func NewFileStore(root string) (*FileStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("artifact root is required")
	}
	clean := filepath.Clean(root)
	if err := os.MkdirAll(filepath.Join(clean, "objects"), 0o700); err != nil {
		return nil, err
	}
	return &FileStore{root: clean}, nil
}

func (s *FileStore) Put(ctx context.Context, artifact Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(artifact.Ref) == "" {
		return fmt.Errorf("artifact ref is required")
	}
	record := fileArtifact{
		Ref:         artifact.Ref,
		Content:     append([]byte(nil), artifact.Content...),
		ContentType: artifact.ContentType,
		Metadata:    cloneMetadata(artifact.Metadata),
		CreatedAt:   time.Now().UTC(),
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	// Each write goes to its own temporary file, synced before the rename, so
	// concurrent writers of one ref never share or rename each other's
	// half-written file, and a crash leaves either the old or the new artifact.
	path := s.path(artifact.Ref)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".put-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *FileStore) Get(ctx context.Context, ref string) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	raw, err := os.ReadFile(s.path(ref))
	if os.IsNotExist(err) {
		return Artifact{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, ref)
	}
	if err != nil {
		return Artifact{}, err
	}
	var record fileArtifact
	if err := json.Unmarshal(raw, &record); err != nil {
		return Artifact{}, err
	}
	if record.Ref != ref {
		return Artifact{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, ref)
	}
	return cloneArtifact(Artifact{
		Ref:         record.Ref,
		Content:     record.Content,
		ContentType: record.ContentType,
		Metadata:    record.Metadata,
	}), nil
}

// maxEncodedNameLen keeps file names well under the common 255-byte limit.
const maxEncodedNameLen = 200

// path names a ref's file by its base64 encoding, or by its SHA-256 when that
// would be too long a file name. Get checks the stored ref, so two refs can
// never read each other's file.
func (s *FileStore) path(ref string) string {
	name := base64.RawURLEncoding.EncodeToString([]byte(ref))
	if len(name) > maxEncodedNameLen {
		digest := sha256.Sum256([]byte(ref))
		name = "sha256-" + hex.EncodeToString(digest[:])
	}
	return filepath.Join(s.root, "objects", name+".json")
}
